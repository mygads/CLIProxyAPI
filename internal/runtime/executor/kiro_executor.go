package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/eventstream"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/google/uuid"
)

// KiroExecutor is a wiring-complete executor for the Kiro AI provider.
//
// Behavior is parity with 9router's openai-to-kiro translator (2026-05):
//
//   - Full conversation history with proper user/assistant turns.
//   - Tool / function calling: body.tools → userInputMessageContext.tools,
//     assistant.tool_calls → assistantResponseMessage.toolUses, tool role
//     and content[].type==tool_result → userInputMessageContext.toolResults.
//   - Consecutive user turns are merged so Kiro does not reject the chain.
//   - Deterministic conversationId via uuidv5(first turn content, NAMESPACE)
//     so AWS Builder ID context cache stays warm.
//   - Multimodal: text and base64 image_url/image parts are forwarded;
//     remote URLs fall back to a "[Image: …]" placeholder.
type KiroExecutor struct {
	cfg       *config.Config
	refreshMu sync.Map
}

// NewKiroExecutor builds the executor. The config is used for proxy-aware
// HTTP clients and request logging.
func NewKiroExecutor(cfg *config.Config) *KiroExecutor { return &KiroExecutor{cfg: cfg} }

// Identifier is the provider key. Matches Auth.Provider written by the
// kiro management handler.
func (e *KiroExecutor) Identifier() string { return "kiro" }

// kiroURL returns the full CodeWhisperer endpoint for the region recorded
// in auth.Metadata. Defaults to us-east-1 when absent.
func kiroURL(auth *cliproxyauth.Auth) string {
	region := kiroauth.DefaultRegion
	if auth != nil && auth.Metadata != nil {
		if r, ok := auth.Metadata["region"].(string); ok && r != "" {
			region = r
		}
	}
	return fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/generateAssistantResponse", region)
}

// kiroHeaders returns the CodeWhisperer header set that mirrors OmniRoute's
// providerHeaderProfiles.ts (2026-05). Header names and values below are
// load-bearing — missing X-Amz-Target gets a 400, missing X-Amz-User-Agent
// or a non-matching UA pair triggers rate-limit gating, and the
// anthropic-beta + bedrock cache-control headers are required for Kiro's
// prompt-caching credit path. Amz-Sdk-Invocation-Id is per-call so AWS's
// idempotency layer does not collapse retries.
func kiroHeaders(accessToken string) http.Header {
	h := make(http.Header, 10)
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", kiroauth.AcceptEventStream)
	h.Set("User-Agent", kiroauth.UserAgent)
	h.Set("X-Amz-User-Agent", kiroauth.XAmzUserAgent)
	h.Set("X-Amz-Target", kiroauth.XAmzTarget)
	h.Set("anthropic-beta", kiroauth.AnthropicBeta)
	h.Set("x-amzn-bedrock-cache-control", kiroauth.BedrockCacheControl)
	h.Set("Amz-Sdk-Request", kiroauth.AmzSdkRequest)
	h.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	return h
}

// ensureAccessToken returns a valid Kiro access token, refreshing when
// the cached one is past its `expired` field. Refresh logic lives in
// internal/auth/kiro/refresh_helper.go so management quota handlers can
// reuse the same code path without depending on this executor.
func (e *KiroExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("kiro: nil auth")
	}
	mu, _ := e.refreshMu.LoadOrStore(auth.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	res, err := kiroauth.RefreshIfExpired(ctx, auth.ID, auth.Metadata)
	if err != nil {
		return "", err
	}
	return res.AccessToken, nil
}

// buildKiroPayload converts an OpenAI-format request to CodeWhisperer's
// GenerateAssistantResponse shape. Behavior is parity with 9router's
// openai-to-kiro translator: full history, tools, toolUses, toolResults,
// images, deterministic conversationId.
func (e *KiroExecutor) buildKiroPayload(openaiBody []byte, model string, auth *cliproxyauth.Auth) ([]byte, error) {
	messages := gjson.GetBytes(openaiBody, "messages").Array()
	tools := gjson.GetBytes(openaiBody, "tools").Array()

	history, currentMessage, err := convertKiroMessages(messages, tools, model)
	if err != nil {
		return nil, err
	}

	finalContent, _ := getKiroUserInputString(currentMessage, "content")
	finalContent = fmt.Sprintf("[Context: Current time is %s]\n\n%s",
		time.Now().UTC().Format(time.RFC3339), finalContent)

	currentUserInput := map[string]any{
		"content": finalContent,
		"modelId": model,
		"origin":  "AI_EDITOR",
	}
	if currentMessage != nil {
		if uim, ok := currentMessage["userInputMessage"].(map[string]any); ok {
			if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok && len(ctx) > 0 {
				currentUserInput["userInputMessageContext"] = ctx
			}
			if imgs, ok := uim["images"].([]any); ok && len(imgs) > 0 {
				currentUserInput["images"] = imgs
			}
		}
	}

	// Deterministic conversationId for AWS Builder ID context cache.
	// Use uuidv5 over the first turn's content (capped at 4000 chars).
	firstContent := finalContent
	if len(history) > 0 {
		if uim, ok := history[0].(map[string]any)["userInputMessage"].(map[string]any); ok {
			if c, ok := uim["content"].(string); ok && c != "" {
				firstContent = c
			}
		}
	}
	if len(firstContent) > 4000 {
		firstContent = firstContent[:4000]
	}
	convoID := uuid.NewSHA1(kiroConversationNamespace, []byte(firstContent)).String()

	payload := map[string]any{
		"conversationState": map[string]any{
			"chatTriggerType": "MANUAL",
			"conversationId":  convoID,
			"currentMessage": map[string]any{
				"userInputMessage": currentUserInput,
			},
			"history": history,
		},
	}
	if auth != nil && auth.Metadata != nil {
		if profileArn, _ := auth.Metadata["profile_arn"].(string); profileArn != "" {
			payload["profileArn"] = profileArn
		}
	}

	// inferenceConfig is optional — honor max_tokens/temperature/top_p if set.
	inferenceConfig := map[string]any{}
	if v := gjson.GetBytes(openaiBody, "max_tokens"); v.Exists() {
		inferenceConfig["maxTokens"] = v.Int()
	} else if v := gjson.GetBytes(openaiBody, "max_completion_tokens"); v.Exists() {
		inferenceConfig["maxTokens"] = v.Int()
	}
	if v := gjson.GetBytes(openaiBody, "temperature"); v.Exists() {
		inferenceConfig["temperature"] = v.Float()
	}
	if v := gjson.GetBytes(openaiBody, "top_p"); v.Exists() {
		inferenceConfig["topP"] = v.Float()
	}
	if len(inferenceConfig) > 0 {
		payload["inferenceConfig"] = inferenceConfig
	}

	return json.Marshal(payload)
}

// kiroConversationNamespace mirrors 9router's NAMESPACE_KIRO constant so
// the same first-turn content yields the same UUID, keeping AWS Builder
// ID prompt-cache lookups consistent across deployments.
var kiroConversationNamespace = uuid.MustParse("34f7193f-561d-4050-bc84-9547d953d6bf")

// getKiroUserInputString safely fetches a string field from
// currentMessage.userInputMessage. Returns empty string when missing.
func getKiroUserInputString(currentMessage map[string]any, key string) (string, bool) {
	if currentMessage == nil {
		return "", false
	}
	uim, ok := currentMessage["userInputMessage"].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := uim[key].(string)
	return v, ok
}

// convertKiroMessages walks an OpenAI-shaped messages array and produces
// (history, currentMessage) for Kiro's GenerateAssistantResponse payload.
// It is a Go port of 9router's convertMessages — see
// 9router/open-sse/translator/request/openai-to-kiro.js for parity.
//
// Rules:
//   - system/tool roles are normalized to user.
//   - Consecutive same-role turns are buffered into one Kiro turn.
//   - Tools (body.tools) are attached to the first user turn's
//     userInputMessageContext.tools and forwarded to currentMessage so the
//     final turn carries tool defs to upstream.
//   - assistant.tool_calls and assistant.content[].type=="tool_use" are
//     attached as assistantResponseMessage.toolUses.
//   - tool role and content[].type=="tool_result" become
//     userInputMessageContext.toolResults on the next user turn.
//   - Base64 image_url / Claude image parts are forwarded; remote http(s)
//     URLs degrade to a "[Image: …]" placeholder.
//   - The last userInputMessage in history becomes currentMessage; if the
//     conversation ends with assistant/tool, currentMessage is a
//     "Continue" stub.
//   - Adjacent userInputMessage entries left over after the assistant
//     branch resets currentRole are merged into one.
func convertKiroMessages(messages []gjson.Result, tools []gjson.Result, model string) ([]any, map[string]any, error) {
	history := []any{}
	var pendingUserContent []string
	var pendingAssistantContent []string
	var pendingToolResults []map[string]any
	var pendingImages []map[string]any
	var currentRole string

	flushPending := func() {
		switch currentRole {
		case "user":
			content := strings.TrimSpace(strings.Join(pendingUserContent, "\n\n"))
			if content == "" {
				content = "continue"
			}
			userMsg := map[string]any{
				"userInputMessage": map[string]any{
					"content": content,
					"modelId": "",
				},
			}
			uim := userMsg["userInputMessage"].(map[string]any)
			if len(pendingImages) > 0 {
				imgs := make([]any, 0, len(pendingImages))
				for _, img := range pendingImages {
					imgs = append(imgs, img)
				}
				uim["images"] = imgs
			}
			if len(pendingToolResults) > 0 {
				tr := make([]any, 0, len(pendingToolResults))
				for _, r := range pendingToolResults {
					tr = append(tr, r)
				}
				uim["userInputMessageContext"] = map[string]any{"toolResults": tr}
			}
			// Tools attach to the first user turn only.
			if len(tools) > 0 && len(history) == 0 {
				ctx, _ := uim["userInputMessageContext"].(map[string]any)
				if ctx == nil {
					ctx = map[string]any{}
				}
				ctx["tools"] = buildKiroToolSpecs(tools)
				uim["userInputMessageContext"] = ctx
			}
			history = append(history, userMsg)
			pendingUserContent = nil
			pendingToolResults = nil
			pendingImages = nil
		case "assistant":
			content := strings.TrimSpace(strings.Join(pendingAssistantContent, "\n\n"))
			if content == "" {
				content = "..."
			}
			assistantMsg := map[string]any{
				"assistantResponseMessage": map[string]any{
					"content": content,
				},
			}
			history = append(history, assistantMsg)
			pendingAssistantContent = nil
		}
	}

	for _, msg := range messages {
		role := msg.Get("role").String()
		if role == "system" || role == "tool" {
			role = "user"
		}
		if role != currentRole && currentRole != "" {
			flushPending()
		}
		currentRole = role

		if role == "user" {
			textContent, images, toolResults := extractKiroUserParts(msg)
			pendingImages = append(pendingImages, images...)
			pendingToolResults = append(pendingToolResults, toolResults...)
			if msg.Get("role").String() == "tool" {
				toolContent := msg.Get("content").String()
				pendingToolResults = append(pendingToolResults, map[string]any{
					"toolUseId": msg.Get("tool_call_id").String(),
					"status":    "success",
					"content":   []any{map[string]any{"text": toolContent}},
				})
			} else if textContent != "" {
				pendingUserContent = append(pendingUserContent, textContent)
			}
			continue
		}

		if role == "assistant" {
			textContent, toolUses := extractKiroAssistantParts(msg)
			if textContent != "" {
				pendingAssistantContent = append(pendingAssistantContent, textContent)
			}
			if len(toolUses) > 0 {
				flushPending()
				if last, ok := history[len(history)-1].(map[string]any); ok {
					if arm, ok := last["assistantResponseMessage"].(map[string]any); ok {
						arm["toolUses"] = toolUses
					}
				}
				currentRole = ""
			}
		}
	}

	if currentRole != "" {
		flushPending()
	}

	if len(history) == 0 {
		return nil, nil, fmt.Errorf("kiro: no messages to send")
	}

	// Pop the last userInputMessage from history as currentMessage.
	var currentMessage map[string]any
	for i := len(history) - 1; i >= 0; i-- {
		entry, _ := history[i].(map[string]any)
		if entry == nil {
			continue
		}
		if _, ok := entry["userInputMessage"]; ok {
			currentMessage = entry
			history = append(history[:i], history[i+1:]...)
			break
		}
	}

	// Capture first-history tools BEFORE cleanup deletes them — they
	// must travel on currentMessage so upstream sees the spec.
	var firstTools any
	if len(history) > 0 {
		if entry, ok := history[0].(map[string]any); ok {
			if uim, ok := entry["userInputMessage"].(map[string]any); ok {
				if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
					if t, ok := ctx["tools"]; ok {
						firstTools = t
					}
				}
			}
		}
	}

	// Cleanup history: strip tools from history user turns (currentMessage
	// owns them), drop empty contexts, and ensure modelId is set.
	for _, raw := range history {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		uim, _ := entry["userInputMessage"].(map[string]any)
		if uim == nil {
			continue
		}
		if ctx, ok := uim["userInputMessageContext"].(map[string]any); ok {
			delete(ctx, "tools")
			if len(ctx) == 0 {
				delete(uim, "userInputMessageContext")
			}
		}
		if id, _ := uim["modelId"].(string); id == "" {
			uim["modelId"] = model
		}
	}

	// Merge consecutive userInputMessage entries (Kiro requires
	// alternating user/assistant). The assistant branch's currentRole
	// reset can leave adjacent user turns in history.
	mergedHistory := make([]any, 0, len(history))
	for _, raw := range history {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			mergedHistory = append(mergedHistory, raw)
			continue
		}
		curUIM, isUser := entry["userInputMessage"].(map[string]any)
		if isUser && len(mergedHistory) > 0 {
			prev, _ := mergedHistory[len(mergedHistory)-1].(map[string]any)
			if prev != nil {
				if prevUIM, ok := prev["userInputMessage"].(map[string]any); ok {
					prevContent, _ := prevUIM["content"].(string)
					curContent, _ := curUIM["content"].(string)
					if prevContent != "" && curContent != "" {
						prevUIM["content"] = prevContent + "\n\n" + curContent
					} else if curContent != "" {
						prevUIM["content"] = curContent
					}
					if curCtx, ok := curUIM["userInputMessageContext"].(map[string]any); ok {
						prevCtx, _ := prevUIM["userInputMessageContext"].(map[string]any)
						if prevCtx == nil {
							prevCtx = map[string]any{}
						}
						for k, v := range curCtx {
							if existing, ok := prevCtx[k].([]any); ok {
								if next, ok := v.([]any); ok {
									prevCtx[k] = append(existing, next...)
									continue
								}
							}
							prevCtx[k] = v
						}
						prevUIM["userInputMessageContext"] = prevCtx
					}
					if curImgs, ok := curUIM["images"].([]any); ok && len(curImgs) > 0 {
						prevImgs, _ := prevUIM["images"].([]any)
						prevUIM["images"] = append(prevImgs, curImgs...)
					}
					continue
				}
			}
		}
		mergedHistory = append(mergedHistory, raw)
	}
	history = mergedHistory

	// If the conversation ended with assistant/tool we still need a
	// user turn for currentMessage. Use a "Continue" stub so upstream
	// keeps generating from the prior assistant tail.
	if currentMessage == nil {
		currentMessage = map[string]any{
			"userInputMessage": map[string]any{
				"content": "Continue",
				"modelId": model,
			},
		}
	}

	// Inject tools onto currentMessage AFTER cleanup so upstream sees them.
	if firstTools != nil {
		if uim, ok := currentMessage["userInputMessage"].(map[string]any); ok {
			ctx, _ := uim["userInputMessageContext"].(map[string]any)
			if ctx == nil {
				ctx = map[string]any{}
			}
			if _, has := ctx["tools"]; !has {
				ctx["tools"] = firstTools
			}
			uim["userInputMessageContext"] = ctx
		}
	}

	return history, currentMessage, nil
}

// buildKiroToolSpecs converts OpenAI/Anthropic tool definitions into
// Kiro's toolSpecification shape. Kiro requires every schema to have
// {type, properties, required}, so we normalize defensively.
func buildKiroToolSpecs(tools []gjson.Result) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		name := t.Get("function.name").String()
		if name == "" {
			name = t.Get("name").String()
		}
		desc := t.Get("function.description").String()
		if desc == "" {
			desc = t.Get("description").String()
		}
		if strings.TrimSpace(desc) == "" {
			desc = "Tool: " + name
		}
		schemaResult := t.Get("function.parameters")
		if !schemaResult.Exists() {
			schemaResult = t.Get("parameters")
		}
		if !schemaResult.Exists() {
			schemaResult = t.Get("input_schema")
		}
		var schema map[string]any
		if schemaResult.Exists() && schemaResult.IsObject() {
			_ = json.Unmarshal([]byte(schemaResult.Raw), &schema)
		}
		schema = normalizeKiroToolSchema(schema)
		out = append(out, map[string]any{
			"toolSpecification": map[string]any{
				"name":        name,
				"description": desc,
				"inputSchema": map[string]any{"json": schema},
			},
		})
	}
	return out
}

func normalizeKiroToolSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []any{},
		}
	}
	out := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	for k, v := range schema {
		out[k] = v
	}
	if _, ok := out["required"].([]any); !ok {
		// Tolerate []string or []interface variations.
		if r, ok := schema["required"]; ok {
			if rArr, ok := r.([]any); ok {
				out["required"] = rArr
			} else {
				out["required"] = []any{}
			}
		} else {
			out["required"] = []any{}
		}
	}
	return out
}

// extractKiroUserParts pulls (text, images, toolResults) from a single
// user-role message. Content may be a string or an OpenAI/Claude content
// array; both are accepted.
func extractKiroUserParts(msg gjson.Result) (string, []map[string]any, []map[string]any) {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()), nil, nil
	}
	if !content.IsArray() {
		return "", nil, nil
	}
	var textParts []string
	var images []map[string]any
	var toolResults []map[string]any
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text":
			textParts = append(textParts, part.Get("text").String())
		case "image_url":
			url := part.Get("image_url.url").String()
			if img := parseDataURIImage(url); img != nil {
				images = append(images, img)
			} else if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				textParts = append(textParts, "[Image: "+url+"]")
			}
		case "image":
			if part.Get("source.type").String() == "base64" {
				if data := part.Get("source.data").String(); data != "" {
					mediaType := part.Get("source.media_type").String()
					if mediaType == "" {
						mediaType = "image/png"
					}
					images = append(images, map[string]any{
						"format": imageFormatFromMediaType(mediaType),
						"source": map[string]any{"bytes": data},
					})
				}
			}
		case "tool_result":
			text := ""
			inner := part.Get("content")
			if inner.IsArray() {
				var parts []string
				for _, c := range inner.Array() {
					parts = append(parts, c.Get("text").String())
				}
				text = strings.Join(parts, "\n")
			} else if inner.Type == gjson.String {
				text = inner.String()
			}
			toolResults = append(toolResults, map[string]any{
				"toolUseId": part.Get("tool_use_id").String(),
				"status":    "success",
				"content":   []any{map[string]any{"text": text}},
			})
		default:
			// Some clients send untyped {text: "..."} parts.
			if t := part.Get("text").String(); t != "" {
				textParts = append(textParts, t)
			}
		}
	}
	return strings.TrimSpace(strings.Join(textParts, "\n")), images, toolResults
}

// extractKiroAssistantParts returns (textContent, toolUses) for an
// assistant message. tool_calls (OpenAI) and content[].type=="tool_use"
// (Claude) both feed the same toolUses array.
func extractKiroAssistantParts(msg gjson.Result) (string, []any) {
	var textContent string
	var toolUses []any

	content := msg.Get("content")
	if content.IsArray() {
		var textParts []string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				textParts = append(textParts, part.Get("text").String())
			case "tool_use":
				toolUses = append(toolUses, map[string]any{
					"toolUseId": orFallback(part.Get("id").String(), uuid.New().String()),
					"name":      part.Get("name").String(),
					"input":     parseKiroToolInput(part.Get("input")),
				})
			}
		}
		textContent = strings.TrimSpace(strings.Join(textParts, "\n"))
	} else if content.Type == gjson.String {
		textContent = strings.TrimSpace(content.String())
	}

	if tc := msg.Get("tool_calls"); tc.IsArray() {
		// Replace any inferred Claude-style tool uses with OpenAI ones.
		toolUses = toolUses[:0]
		for _, t := range tc.Array() {
			if fn := t.Get("function"); fn.Exists() {
				toolUses = append(toolUses, map[string]any{
					"toolUseId": orFallback(t.Get("id").String(), uuid.New().String()),
					"name":      fn.Get("name").String(),
					"input":     parseKiroToolInput(fn.Get("arguments")),
				})
			} else {
				toolUses = append(toolUses, map[string]any{
					"toolUseId": orFallback(t.Get("id").String(), uuid.New().String()),
					"name":      t.Get("name").String(),
					"input":     parseKiroToolInput(t.Get("input")),
				})
			}
		}
	}

	return textContent, toolUses
}

func parseKiroToolInput(v gjson.Result) any {
	if !v.Exists() {
		return map[string]any{}
	}
	if v.IsObject() {
		var out map[string]any
		if err := json.Unmarshal([]byte(v.Raw), &out); err == nil {
			return out
		}
		return map[string]any{}
	}
	if v.Type == gjson.String {
		s := strings.TrimSpace(v.String())
		if s == "" {
			return map[string]any{}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return map[string]any{}
	}
	return map[string]any{}
}

func orFallback(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// parseDataURIImage extracts {format, source.bytes} from a data: URL like
// "data:image/png;base64,…". Returns nil for non-data URIs.
func parseDataURIImage(url string) map[string]any {
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	rest := url[len("data:"):]
	semi := strings.Index(rest, ";")
	if semi <= 0 {
		return nil
	}
	mediaType := rest[:semi]
	rest = rest[semi+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return nil
	}
	data := rest[len("base64,"):]
	if data == "" {
		return nil
	}
	return map[string]any{
		"format": imageFormatFromMediaType(mediaType),
		"source": map[string]any{"bytes": data},
	}
}

func imageFormatFromMediaType(mediaType string) string {
	if i := strings.Index(mediaType, "/"); i >= 0 {
		return mediaType[i+1:]
	}
	return mediaType
}

// Execute performs a non-streaming request to CodeWhisperer and assembles
// the decoded event stream into a single OpenAI chat completion response.
func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	// Translate incoming payload to OpenAI-shaped JSON first — our
	// payload builder reads messages in OpenAI format. The source format
	// may already be openai; TranslateRequest is a no-op in that case.
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	openaiBody := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)

	kiroBody, err := e.buildKiroPayload(openaiBody, baseModel, auth)
	if err != nil {
		return resp, fmt.Errorf("kiro: build payload: %w", err)
	}

	httpResp, err := e.doKiroRequest(ctx, auth, kiroBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: close body: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	assembled, err := assembleKiroResponse(ctx, e.cfg, httpResp.Body, baseModel, openaiBody)
	if err != nil {
		return resp, err
	}

	// Convert the assembled OpenAI-shaped response back to the caller's
	// format. TranslateNonStream handles the reverse direction.
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, openaiBody, assembled, &param)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(assembled))
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// ExecuteStream is the streaming variant — each decoded frame is converted
// to an OpenAI-style SSE chunk and pushed to the caller's channel.
func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	openaiBody := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	kiroBody, err := e.buildKiroPayload(openaiBody, baseModel, auth)
	if err != nil {
		return nil, fmt.Errorf("kiro: build payload: %w", err)
	}

	httpResp, err := e.doKiroRequest(ctx, auth, kiroBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: close body: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kiro executor: close stream body: %v", errClose)
			}
		}()
		streamKiroResponseAsOpenAI(ctx, e.cfg, httpResp.Body, baseModel, out, reporter, req.Model, from, to, opts.OriginalRequest, openaiBody)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// doKiroRequest builds and sends a single CodeWhisperer request, handling
// the one-shot 401 retry pattern. On 401 the access token is invalidated
// so the next attempt forces a refresh.
func (e *KiroExecutor) doKiroRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte) (*http.Response, error) {
	var lastResp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		token, err := e.ensureAccessToken(ctx, auth)
		if err != nil {
			return nil, err
		}
		url := kiroURL(auth)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range kiroHeaders(token) {
			httpReq.Header.Set(k, vs[0])
		}
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

		var authID, authLabel, authType, authValue string
		if auth != nil {
			authID = auth.ID
			authLabel = auth.Label
			authType, authValue = auth.AccountInfo()
		}
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      body,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 60*time.Second)
		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			// Invalidate cached access token so the next ensure call refreshes.
			if auth != nil && auth.Metadata != nil {
				delete(auth.Metadata, "access_token")
				auth.Metadata["expires_at"] = int64(0)
			}
			continue
		}
		lastResp = resp
		break
	}
	if lastResp == nil {
		return nil, &cliproxyauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "kiro: persistent 401 after token refresh",
		}
	}
	return lastResp, nil
}

// kiroStreamState accumulates per-response data while walking the
// CodeWhisperer event stream. The shape mirrors OmniRoute's kiro.ts
// executor: content deltas, tool-call accumulator, usage numbers, and a
// terminal flag. Both the buffering (Execute) and streaming
// (ExecuteStream) paths use it so event handling stays in one place.
type kiroStreamState struct {
	content       strings.Builder
	reasoning     strings.Builder
	toolCalls     map[string]*kiroToolCall
	toolOrder     []string
	stopSeen      bool
	usage         *kiroUsage
	contextUsage  float64
	hasMetering   bool
}

type kiroToolCall struct {
	ID    string
	Name  string
	Input strings.Builder
}

type kiroUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// applyKiroEvent dispatches one decoded eventstream frame into the state
// struct. The caller supplies an optional emit function that is invoked
// for every streamed delta (content, reasoning, tool call) — Execute
// leaves it nil, ExecuteStream wires it to an SSE emitter. Returns true
// when the event ends the response (messageStop / exception handled by
// caller).
//
// Event taxonomy mirrors OmniRoute's transformEventStreamToSSE:
//
//   assistantResponseEvent / codeEvent — {"content": "..."} text delta.
//   reasoningContentEvent              — {"content": "..."} thinking trace.
//   toolUseEvent                       — {"toolUseId", "name", "input", "stop"}.
//                                       Multiple events per tool; last has stop=true.
//   messageStopEvent                   — terminates the response.
//   metricsEvent                       — final token usage. Only trustable number.
//   contextUsageEvent                  — informational, percentage-used.
//   meteringEvent                      — no-op flag.
type kiroEmitter func(delta map[string]any)

func (s *kiroStreamState) apply(msg *eventstream.Message, emit kiroEmitter) {
	switch msg.EventType() {
	case "assistantResponseEvent", "codeEvent":
		content := gjson.GetBytes(msg.Payload, "content")
		if !content.Exists() || content.String() == "" {
			return
		}
		s.content.WriteString(content.String())
		if emit != nil {
			emit(map[string]any{"content": content.String()})
		}

	case "reasoningContentEvent":
		// OmniRoute wraps reasoning in <thinking> tags so downstream
		// OpenAI clients render it as a thought block. We do the same
		// so translator behavior is identical across gateways.
		content := gjson.GetBytes(msg.Payload, "content")
		if !content.Exists() || content.String() == "" {
			return
		}
		s.reasoning.WriteString(content.String())
		if emit != nil {
			emit(map[string]any{"content": "<thinking>" + content.String() + "</thinking>"})
		}

	case "toolUseEvent":
		if s.toolCalls == nil {
			s.toolCalls = make(map[string]*kiroToolCall)
		}
		id := gjson.GetBytes(msg.Payload, "toolUseId").String()
		if id == "" {
			return
		}
		tc, exists := s.toolCalls[id]
		if !exists {
			tc = &kiroToolCall{ID: id}
			s.toolCalls[id] = tc
			s.toolOrder = append(s.toolOrder, id)
			if name := gjson.GetBytes(msg.Payload, "name").String(); name != "" {
				tc.Name = name
			}
			if emit != nil {
				emit(map[string]any{
					"tool_calls": []map[string]any{{
						"index": len(s.toolOrder) - 1,
						"id":    id,
						"type":  "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": "",
						},
					}},
				})
			}
		}
		if input := gjson.GetBytes(msg.Payload, "input"); input.Exists() && input.String() != "" {
			tc.Input.WriteString(input.String())
			if emit != nil {
				emit(map[string]any{
					"tool_calls": []map[string]any{{
						"index": indexOf(s.toolOrder, id),
						"function": map[string]any{
							"arguments": input.String(),
						},
					}},
				})
			}
		}

	case "messageStopEvent":
		s.stopSeen = true

	case "metricsEvent":
		s.usage = &kiroUsage{
			InputTokens:         gjson.GetBytes(msg.Payload, "inputTokens").Int(),
			OutputTokens:        gjson.GetBytes(msg.Payload, "outputTokens").Int(),
			CacheReadTokens:     gjson.GetBytes(msg.Payload, "cacheReadTokens").Int(),
			CacheCreationTokens: gjson.GetBytes(msg.Payload, "cacheCreationTokens").Int(),
		}

	case "contextUsageEvent":
		s.contextUsage = gjson.GetBytes(msg.Payload, "contextUsagePercentage").Float()

	case "meteringEvent":
		s.hasMetering = true

	default:
		// Unknown event — fall back to the legacy top-level "content"
		// extraction so pre-2026 Kiro responses (which only emitted
		// this shape) still work.
		if content := gjson.GetBytes(msg.Payload, "content"); content.Exists() && content.String() != "" {
			s.content.WriteString(content.String())
			if emit != nil {
				emit(map[string]any{"content": content.String()})
			}
		}
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
func estimateTokensFromContent(openaiBody []byte) int64 {
	messages := gjson.GetBytes(openaiBody, "messages")
	if !messages.Exists() {
		return 0
	}
	var total int64
	for _, msg := range messages.Array() {
		content := msg.Get("content")
		if content.Type == gjson.String {
			total += int64(len(content.String()))
		} else if content.IsArray() {
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" {
					total += int64(len(part.Get("text").String()))
				}
			}
		}
	}
	return total / 4
}

func assembleKiroResponse(ctx context.Context, cfg *config.Config, body io.Reader, model string, openaiBody []byte) ([]byte, error) {
	dec := eventstream.NewDecoder(body)
	state := &kiroStreamState{}
	for {
		msg, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("kiro: decode eventstream: %w", err)
		}
		helps.AppendAPIResponseChunk(ctx, cfg, msg.Payload)

		if msg.MessageType() == "exception" {
			return nil, fmt.Errorf("kiro: upstream exception: %s", string(msg.Payload))
		}
		if msg.MessageType() != "event" && msg.MessageType() != "" {
			continue
		}
		state.apply(msg, nil)
	}

	now := time.Now().Unix()
	message := map[string]any{
		"role":    "assistant",
		"content": state.content.String(),
	}
	if len(state.toolOrder) > 0 {
		toolCalls := make([]map[string]any, 0, len(state.toolOrder))
		for i, id := range state.toolOrder {
			tc := state.toolCalls[id]
			toolCalls = append(toolCalls, map[string]any{
				"index": i,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Input.String(),
				},
			})
		}
		message["tool_calls"] = toolCalls
	}
	finishReason := "stop"
	if len(state.toolOrder) > 0 {
		finishReason = "tool_calls"
	}
	usage := map[string]any{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
	}
	if state.usage != nil {
		usage["prompt_tokens"] = state.usage.InputTokens
		usage["completion_tokens"] = state.usage.OutputTokens
		usage["total_tokens"] = state.usage.InputTokens + state.usage.OutputTokens
		if state.usage.CacheReadTokens > 0 || state.usage.CacheCreationTokens > 0 {
			usage["prompt_tokens_details"] = map[string]any{
				"cached_tokens":         state.usage.CacheReadTokens,
				"cache_creation_tokens": state.usage.CacheCreationTokens,
			}
		}
	} else {
		promptEst := estimateTokensFromContent(openaiBody)
		completionEst := int64(state.content.Len()+state.reasoning.Len()) / 4
		if completionEst < 1 && state.content.Len() > 0 {
			completionEst = 1
		}
		usage["prompt_tokens"] = promptEst
		usage["completion_tokens"] = completionEst
		usage["total_tokens"] = promptEst + completionEst
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", now),
		"object":  "chat.completion",
		"created": now,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
	}
	return json.Marshal(resp)
}

// streamKiroResponseAsOpenAI decodes an eventstream body and emits OpenAI
// chat.completion.chunk SSE frames on out. It stops cleanly on EOF and
// forwards decode errors as a terminal Err chunk.
func streamKiroResponseAsOpenAI(
	ctx context.Context,
	cfg *config.Config,
	body io.Reader,
	model string,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *helps.UsageReporter,
	requestedModel string,
	from, to sdktranslator.Format,
	originalRequest, translatedRequest []byte,
) {
	dec := eventstream.NewDecoder(body)
	now := time.Now().Unix()
	streamID := fmt.Sprintf("chatcmpl-%d", now)
	var param any

	emit := func(raw []byte) {
		chunks := sdktranslator.TranslateStream(ctx, to, from, requestedModel, originalRequest, translatedRequest, raw, &param)
		for i := range chunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
			case <-ctx.Done():
				return
			}
		}
	}

	// First chunk carries role=assistant per OpenAI streaming convention.
	firstChunk := buildSSEChunk(streamID, now, model, map[string]any{"role": "assistant"}, "")
	emit(firstChunk)

	state := &kiroStreamState{}
	emitter := func(delta map[string]any) {
		emit(buildSSEChunk(streamID, now, model, delta, ""))
	}

	for {
		msg, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			helps.RecordAPIResponseError(ctx, cfg, err)
			reporter.PublishFailure(ctx, err)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: err}:
			case <-ctx.Done():
			}
			return
		}
		helps.AppendAPIResponseChunk(ctx, cfg, msg.Payload)
		if msg.MessageType() == "exception" {
			exceptionErr := fmt.Errorf("kiro: upstream exception: %s", string(msg.Payload))
			reporter.PublishFailure(ctx, exceptionErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: exceptionErr}:
			case <-ctx.Done():
			}
			return
		}
		if msg.MessageType() != "event" && msg.MessageType() != "" {
			continue
		}
		state.apply(msg, emitter)
		if state.stopSeen {
			break
		}
	}

	// Final chunk: tool_calls finish if any tools were used, else stop.
	finishReason := "stop"
	if len(state.toolOrder) > 0 {
		finishReason = "tool_calls"
	}

	// Build the usage block once so it can travel both to the reporter
	// and on the final SSE chunk. The OpenAI->Claude translator only
	// emits message_delta + message_stop when the terminating chunk
	// carries a non-null usage; without it the Claude client never sees
	// the stream close, which surfaces as truncated tool_use blocks.
	var finalUsage map[string]any
	if state.usage != nil {
		reporter.Publish(ctx, cliproxyusage.Detail{
			InputTokens:         state.usage.InputTokens,
			OutputTokens:        state.usage.OutputTokens,
			CacheReadTokens:     state.usage.CacheReadTokens,
			CacheCreationTokens: state.usage.CacheCreationTokens,
			TotalTokens:         state.usage.InputTokens + state.usage.OutputTokens,
		})
		finalUsage = map[string]any{
			"prompt_tokens":     state.usage.InputTokens,
			"completion_tokens": state.usage.OutputTokens,
			"total_tokens":      state.usage.InputTokens + state.usage.OutputTokens,
		}
		if state.usage.CacheReadTokens > 0 || state.usage.CacheCreationTokens > 0 {
			finalUsage["prompt_tokens_details"] = map[string]any{
				"cached_tokens":         state.usage.CacheReadTokens,
				"cache_creation_tokens": state.usage.CacheCreationTokens,
			}
		}
	} else {
		promptEst := estimateTokensFromContent(translatedRequest)
		completionEst := int64(state.content.Len()+state.reasoning.Len()) / 4
		if completionEst < 1 && state.content.Len() > 0 {
			completionEst = 1
		}
		reporter.Publish(ctx, cliproxyusage.Detail{
			InputTokens:  promptEst,
			OutputTokens: completionEst,
			TotalTokens:  promptEst + completionEst,
		})
		finalUsage = map[string]any{
			"prompt_tokens":     promptEst,
			"completion_tokens": completionEst,
			"total_tokens":      promptEst + completionEst,
		}
	}

	emit(buildSSEChunkWithUsage(streamID, now, model, nil, finishReason, finalUsage))
	// IMPORTANT: the [DONE] sentinel must be emitted with the SSE
	// "data: " prefix. The OpenAI->Claude translator hard-rejects any
	// chunk that does not start with "data:", which would silently drop
	// our terminator and leave the Anthropic-shape stream without a
	// message_stop. That is the failure mode that surfaces to clients
	// (Claude Code) as a truncated tool_use block / "Write failed".
	emit([]byte("data: [DONE]"))
}

// buildSSEChunk constructs one `data: {...}` event body in OpenAI
// chat.completion.chunk shape. delta is the per-chunk payload (role,
// content). finishReason is set only on the terminating chunk.
func buildSSEChunk(id string, created int64, model string, delta map[string]any, finishReason string) []byte {
	return buildSSEChunkWithUsage(id, created, model, delta, finishReason, nil)
}

// buildSSEChunkWithUsage is the same as buildSSEChunk but also embeds a
// top-level "usage" object — required on the terminating chunk so the
// downstream OpenAI->Claude translator emits message_delta + message_stop.
// Without usage on the final chunk, Anthropic-shape clients (Claude Code)
// see the stream close mid-tool-use and report "Write failed".
func buildSSEChunkWithUsage(id string, created int64, model string, delta map[string]any, finishReason string, usage map[string]any) []byte {
	choice := map[string]any{"index": 0}
	if delta != nil {
		choice["delta"] = delta
	} else {
		choice["delta"] = map[string]any{}
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	b, _ := json.Marshal(payload)
	return append([]byte("data: "), b...)
}

// Refresh exchanges the long-lived refresh token for a fresh access token
// and persists the result back into auth.Metadata. It is safe to call
// from hot paths — ensureAccessToken already calls it when the cached
// token is nearing expiry.
func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("kiro: refresh called with nil auth")
	}
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	if _, err := e.ensureAccessToken(ctx, auth); err != nil {
		return nil, err
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	return auth, nil
}

// CountTokens is intentionally unimplemented — Kiro does not publish a
// dedicated token counter. Callers get a clean 501 so upstream routing
// can fall back.
func (e *KiroExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "kiro: CountTokens not supported by upstream",
	}
}

// HttpRequest runs an externally-built request through the executor's
// auth + proxy-aware client. Used by management probes and debug tooling.
func (e *KiroExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kiro: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	token, err := e.ensureAccessToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	for k, vs := range kiroHeaders(token) {
		httpReq.Header.Set(k, vs[0])
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second)
	return httpClient.Do(httpReq)
}
