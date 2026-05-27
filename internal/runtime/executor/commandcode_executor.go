// Package executor provides the Command Code (commandcode.ai) provider executor.
//
// This executor mirrors the 9router commandcode integration. The upstream
// `https://api.commandcode.ai/alpha/generate` endpoint expects a custom
// request body and emits AI SDK v5 NDJSON events. We translate from the
// caller's OpenAI/Claude format into Command Code's request shape and
// re-emit each NDJSON event as an OpenAI-compatible SSE chunk so existing
// downstream consumers stay format-clean.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// commandCodeDefaultBaseURL is the upstream endpoint for Command Code's
// generate API. Falls back when an entry doesn't override base-url.
const commandCodeDefaultBaseURL = "https://api.commandcode.ai/alpha/generate"

// CommandCodeExecutor talks to api.commandcode.ai with bearer-token auth.
// Each request is translated from the caller's source format (OpenAI/Claude)
// into Command Code's bespoke `/alpha/generate` payload shape, and the
// upstream NDJSON response is converted into OpenAI chat-completion chunks.
type CommandCodeExecutor struct {
	cfg *config.Config
}

// NewCommandCodeExecutor creates a Command Code executor bound to the
// supplied config (used for upstream timeout, proxy, payload defaults).
func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{cfg: cfg}
}

// Identifier returns the provider identifier used by the auth manager
// to match credentials synthesized as `provider: "commandcode"`.
func (e *CommandCodeExecutor) Identifier() string { return "commandcode" }

// PrepareRequest injects the bearer token + custom headers from the auth
// entry. Used when the runtime issues a passthrough HTTP request without
// going through Execute/ExecuteStream.
func (e *CommandCodeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-command-code-version", "0.25.7")
	req.Header.Set("x-cli-environment", "cli")
	req.Header.Set("x-session-id", uuid.NewString())
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest is a passthrough hook used for clients that already speak
// Command Code's native shape. It just attaches credentials + headers
// and dispatches the request through the proxy-aware HTTP client.
func (e *CommandCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("commandcode executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, e.cfg.UpstreamTimeout())
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming Command Code request by buffering the
// streaming NDJSON response and emitting a single concatenated OpenAI
// chat.completion JSON document. Command Code does not have a true
// non-streaming endpoint; we always stream upstream and aggregate.
func (e *CommandCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if apiKey == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing commandcode api key"}
		return
	}
	if baseURL == "" {
		baseURL = commandCodeDefaultBaseURL
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	openaiPayload := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)

	commandCodeBody, err := buildCommandCodeBody(baseModel, openaiPayload, false)
	if err != nil {
		return resp, fmt.Errorf("commandcode executor: build payload: %w", err)
	}

	httpResp, err := e.dispatch(ctx, auth, baseURL, apiKey, commandCodeBody)
	if err != nil {
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("commandcode executor: close response body error: %v", errClose)
		}
	}()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	openaiChunks, ccUsage, errAgg := aggregateCommandCodeStream(ctx, e.cfg, httpResp.Body, baseModel)
	if errAgg != nil {
		return resp, errAgg
	}
	finalJSON := buildOpenAINonStreamFromChunks(baseModel, openaiChunks, ccUsage)
	if ccUsage != nil {
		reporter.Publish(ctx, ccUsageToDetail(ccUsage))
	}
	reporter.EnsurePublished(ctx)

	// Translate OpenAI JSON back to caller format if needed.
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, openaiPayload, finalJSON, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	_ = originalPayloadSource // referenced for future payload-config integration
	_ = runtime.GOOS          // keep runtime import live for future telemetry
	return resp, nil
}

// ExecuteStream performs a streaming Command Code request, translating
// each upstream NDJSON event to one or more OpenAI SSE chunks and feeding
// them into the cliproxy stream channel.
func (e *CommandCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if apiKey == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing commandcode api key"}
		return nil, err
	}
	if baseURL == "" {
		baseURL = commandCodeDefaultBaseURL
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	_ = originalPayloadSource
	openaiPayload := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	commandCodeBody, err := buildCommandCodeBody(baseModel, openaiPayload, true)
	if err != nil {
		return nil, fmt.Errorf("commandcode executor: build payload: %w", err)
	}

	httpResp, err := e.dispatch(ctx, auth, baseURL, apiKey, commandCodeBody)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("commandcode executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("commandcode executor: close response body error: %v", errClose)
			}
			reporter.EnsurePublished(ctx)
		}()
		state := newCommandCodeStreamState(baseModel)
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			// Tolerate optional `data:` framing if upstream wraps NDJSON in SSE.
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				trimmed = bytes.TrimSpace(trimmed[len("data:"):])
				if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
					continue
				}
			}
			chunks := state.translate(trimmed)
			for _, ch := range chunks {
				if ch == nil {
					continue
				}
				ssePayload := append([]byte("data: "), ch...)
				ssePayload = append(ssePayload, '\n', '\n')
				// Translate each chunk back to source format if needed.
				translated := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, openaiPayload, ssePayload, &state.translatorParam)
				for i := range translated {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: translated[i]}:
					case <-ctx.Done():
						return
					}
				}
			}
			if state.usage != nil {
				reporter.Publish(ctx, ccUsageToDetail(state.usage))
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		// Emit terminal [DONE] marker so OpenAI clients close the stream cleanly.
		doneSSE := []byte("data: [DONE]\n\n")
		translated := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, openaiPayload, doneSSE, &state.translatorParam)
		for i := range translated {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: translated[i]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens approximates token counts for Command Code by reusing the
// OpenAI tokenizer against the OpenAI-form of the request payload.
func (e *CommandCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	enc, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("commandcode executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("commandcode executor: token counting failed: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	return cliproxyexecutor.Response{Payload: sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)}, nil
}

// Refresh is a no-op: Command Code uses long-lived bearer tokens.
func (e *CommandCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

// dispatch builds and sends the actual HTTPS request, applying any custom
// per-credential headers stored in auth.Attributes.
func (e *CommandCodeExecutor) dispatch(ctx context.Context, auth *cliproxyauth.Auth, baseURL, apiKey string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("commandcode executor: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("x-command-code-version", "0.25.7")
	httpReq.Header.Set("x-cli-environment", "cli")
	httpReq.Header.Set("x-session-id", uuid.NewString())
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
		URL:       baseURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, e.cfg.UpstreamTimeout())
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	return resp, nil
}

func (e *CommandCodeExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil || auth.Attributes == nil {
		return "", ""
	}
	baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	return
}

// ─── Request translator: OpenAI → Command Code ─────────────────────────────
// Mirror of 9router/open-sse/translator/request/openai-to-commandcode.js.
// Command Code's `/alpha/generate` expects:
//   - top-level: threadId, memory, config{}, params{}
//   - params.system as string at top level (NOT in messages)
//   - messages[].role ∈ {user, assistant, tool}
//   - messages[].content as content blocks (NEVER string)
//   - tool calls/results as Anthropic-style blocks

type ccContentBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     map[string]any  `json:"output,omitempty"`
}

type ccMessage struct {
	Role    string           `json:"role"`
	Content []ccContentBlock `json:"content"`
}

type ccTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type ccParams struct {
	Model       string      `json:"model"`
	Messages    []ccMessage `json:"messages"`
	Stream      bool        `json:"stream"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature float64     `json:"temperature,omitempty"`
	System      string      `json:"system,omitempty"`
	Tools       []ccTool    `json:"tools,omitempty"`
	TopP        *float64    `json:"top_p,omitempty"`
}

type ccConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type ccRequest struct {
	ThreadID string   `json:"threadId"`
	Memory   string   `json:"memory"`
	Config   ccConfig `json:"config"`
	Params   ccParams `json:"params"`
}

func buildCommandCodeBody(model string, openaiPayload []byte, stream bool) ([]byte, error) {
	maxTokens := int(gjson.GetBytes(openaiPayload, "max_tokens").Int())
	if maxTokens == 0 {
		if v := gjson.GetBytes(openaiPayload, "max_output_tokens").Int(); v > 0 {
			maxTokens = int(v)
		}
	}
	if maxTokens == 0 {
		maxTokens = 64000
	}
	temperature := gjson.GetBytes(openaiPayload, "temperature").Float()
	if temperature == 0 {
		temperature = 0.3
	}

	messages, system := convertMessages(openaiPayload)
	tools := convertTools(openaiPayload)

	params := ccParams{
		Model:       model,
		Messages:    messages,
		Stream:      stream,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	}
	if system != "" {
		params.System = system
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	if v := gjson.GetBytes(openaiPayload, "top_p"); v.Exists() {
		f := v.Float()
		params.TopP = &f
	}

	cfg := ccConfig{
		WorkingDir:    "/tmp",
		Date:          time.Now().UTC().Format("2006-01-02"),
		Environment:   runtime.GOOS,
		Structure:     []string{},
		RecentCommits: []string{},
	}

	body := ccRequest{
		ThreadID: uuid.NewString(),
		Memory:   "",
		Config:   cfg,
		Params:   params,
	}
	return json.Marshal(body)
}

func convertMessages(payload []byte) ([]ccMessage, string) {
	var systemTexts []string
	out := make([]ccMessage, 0)
	gjson.GetBytes(payload, "messages").ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		if role == "system" {
			systemTexts = append(systemTexts, flattenContentText(m.Get("content")))
			return true
		}
		if role == "tool" {
			out = append(out, ccMessage{
				Role: "tool",
				Content: []ccContentBlock{{
					Type:       "tool-result",
					ToolCallID: m.Get("tool_call_id").String(),
					ToolName:   m.Get("name").String(),
					Output: map[string]any{
						"type":  "text",
						"value": flattenContentText(m.Get("content")),
					},
				}},
			})
			return true
		}
		if role == "assistant" {
			blocks := []ccContentBlock{}
			if text := flattenContentText(m.Get("content")); text != "" {
				blocks = append(blocks, ccContentBlock{Type: "text", Text: text})
			}
			m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
				args := tc.Get("function.arguments")
				var raw json.RawMessage
				argStr := args.String()
				if argStr != "" {
					var probe any
					if err := json.Unmarshal([]byte(argStr), &probe); err == nil {
						raw = json.RawMessage(argStr)
					} else {
						raw = json.RawMessage(`{}`)
					}
				} else {
					raw = json.RawMessage(`{}`)
				}
				blocks = append(blocks, ccContentBlock{
					Type:       "tool-call",
					ToolCallID: tc.Get("id").String(),
					ToolName:   tc.Get("function.name").String(),
					Input:      raw,
				})
				return true
			})
			if len(blocks) == 0 {
				blocks = []ccContentBlock{{Type: "text", Text: ""}}
			}
			out = append(out, ccMessage{Role: "assistant", Content: blocks})
			return true
		}
		// Default user role: convert content to blocks.
		out = append(out, ccMessage{Role: "user", Content: contentToBlocks(m.Get("content"))})
		return true
	})
	return out, strings.Join(systemTexts, "\n\n")
}

func flattenContentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var parts []string
		content.ForEach(func(_, p gjson.Result) bool {
			if p.Type == gjson.String {
				parts = append(parts, p.String())
			} else if t := p.Get("text"); t.Exists() {
				parts = append(parts, t.String())
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	return content.String()
}

func contentToBlocks(content gjson.Result) []ccContentBlock {
	if !content.Exists() {
		return []ccContentBlock{{Type: "text", Text: ""}}
	}
	if content.Type == gjson.String {
		return []ccContentBlock{{Type: "text", Text: content.String()}}
	}
	if content.IsArray() {
		blocks := make([]ccContentBlock, 0)
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Type == gjson.String {
				blocks = append(blocks, ccContentBlock{Type: "text", Text: part.String()})
				return true
			}
			ptype := part.Get("type").String()
			if ptype == "text" || part.Get("text").Exists() {
				blocks = append(blocks, ccContentBlock{Type: "text", Text: part.Get("text").String()})
			} else if ptype == "image_url" || ptype == "image" {
				blocks = append(blocks, ccContentBlock{Type: "text", Text: "[image omitted]"})
			}
			return true
		})
		if len(blocks) == 0 {
			blocks = append(blocks, ccContentBlock{Type: "text", Text: ""})
		}
		return blocks
	}
	return []ccContentBlock{{Type: "text", Text: content.String()}}
}

func convertTools(payload []byte) []ccTool {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.IsArray() {
		return nil
	}
	out := make([]ccTool, 0)
	tools.ForEach(func(_, t gjson.Result) bool {
		if t.Get("type").String() == "function" && t.Get("function").Exists() {
			schema := t.Get("function.parameters")
			var raw json.RawMessage
			if schema.Exists() {
				raw = json.RawMessage(schema.Raw)
			} else {
				raw = json.RawMessage(`{"type":"object"}`)
			}
			out = append(out, ccTool{
				Name:        t.Get("function.name").String(),
				Description: t.Get("function.description").String(),
				InputSchema: raw,
			})
			return true
		}
		if name := t.Get("name").String(); name != "" {
			schema := t.Get("input_schema")
			if !schema.Exists() {
				schema = t.Get("parameters")
			}
			var raw json.RawMessage
			if schema.Exists() {
				raw = json.RawMessage(schema.Raw)
			}
			out = append(out, ccTool{
				Name:        name,
				Description: t.Get("description").String(),
				InputSchema: raw,
			})
		}
		return true
	})
	return out
}

// ─── Response translator: Command Code NDJSON → OpenAI SSE ────────────────
// Mirror of 9router/open-sse/translator/response/commandcode-to-openai.js.
// Command Code emits AI SDK v5 events as one JSON object per line:
//   start, start-step, reasoning-start/delta/end, text-start/delta/end,
//   tool-input-start/delta/end, tool-call, finish-step, finish, error.

type ccUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ccUsageToDetail converts the Command Code aggregated usage into the
// cliproxy usage.Detail shape used by the reporter.
func ccUsageToDetail(u *ccUsage) usage.Detail {
	if u == nil {
		return usage.Detail{}
	}
	return usage.Detail{
		InputTokens:  int64(u.PromptTokens),
		OutputTokens: int64(u.CompletionTokens),
		TotalTokens:  int64(u.TotalTokens),
	}
}

type commandCodeStreamState struct {
	responseID      string
	created         int64
	model           string
	chunkIndex      int
	toolIndex       int
	toolIndexByID   map[string]int
	finishReason    string
	usage           *ccUsage
	translatorParam any
}

func newCommandCodeStreamState(model string) *commandCodeStreamState {
	return &commandCodeStreamState{
		responseID:    fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		created:       time.Now().Unix(),
		model:         model,
		toolIndexByID: make(map[string]int),
	}
}

// translate consumes a single NDJSON event and returns zero or more
// OpenAI chat-completion-chunk JSON documents (without the SSE framing).
func (s *commandCodeStreamState) translate(line []byte) [][]byte {
	if len(line) == 0 {
		return nil
	}
	eventType := gjson.GetBytes(line, "type").String()
	if eventType == "" {
		return nil
	}
	if s.model == "" {
		s.model = gjson.GetBytes(line, "model").String()
	}
	switch eventType {
	case "text-delta":
		text := gjson.GetBytes(line, "text").String()
		if text == "" {
			text = gjson.GetBytes(line, "delta").String()
		}
		if text == "" {
			return nil
		}
		var delta map[string]any
		if s.chunkIndex == 0 {
			delta = map[string]any{"role": "assistant", "content": text}
		} else {
			delta = map[string]any{"content": text}
		}
		s.chunkIndex++
		return [][]byte{s.makeChunk(delta, "")}
	case "reasoning-delta":
		text := gjson.GetBytes(line, "text").String()
		if text == "" {
			return nil
		}
		var delta map[string]any
		if s.chunkIndex == 0 {
			delta = map[string]any{"role": "assistant", "reasoning_content": text}
		} else {
			delta = map[string]any{"reasoning_content": text}
		}
		s.chunkIndex++
		return [][]byte{s.makeChunk(delta, "")}
	case "tool-input-start":
		id := gjson.GetBytes(line, "id").String()
		if id == "" {
			id = gjson.GetBytes(line, "toolCallId").String()
		}
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), s.toolIndex)
		}
		idx, ok := s.toolIndexByID[id]
		if !ok {
			idx = s.toolIndex
			s.toolIndex++
			s.toolIndexByID[id] = idx
		}
		toolCall := map[string]any{
			"index": idx,
			"id":    id,
			"type":  "function",
			"function": map[string]any{
				"name":      gjson.GetBytes(line, "toolName").String(),
				"arguments": "",
			},
		}
		var delta map[string]any
		if s.chunkIndex == 0 {
			delta = map[string]any{"role": "assistant", "tool_calls": []any{toolCall}}
		} else {
			delta = map[string]any{"tool_calls": []any{toolCall}}
		}
		s.chunkIndex++
		return [][]byte{s.makeChunk(delta, "")}
	case "tool-input-delta":
		id := gjson.GetBytes(line, "id").String()
		if id == "" {
			id = gjson.GetBytes(line, "toolCallId").String()
		}
		idx, ok := s.toolIndexByID[id]
		if !ok {
			return nil
		}
		argsDelta := gjson.GetBytes(line, "delta").String()
		if argsDelta == "" {
			argsDelta = gjson.GetBytes(line, "inputTextDelta").String()
		}
		toolCall := map[string]any{
			"index": idx,
			"function": map[string]any{
				"arguments": argsDelta,
			},
		}
		return [][]byte{s.makeChunk(map[string]any{"tool_calls": []any{toolCall}}, "")}
	case "tool-call":
		id := gjson.GetBytes(line, "toolCallId").String()
		if _, exists := s.toolIndexByID[id]; exists {
			return nil
		}
		idx := s.toolIndex
		s.toolIndex++
		s.toolIndexByID[id] = idx
		input := gjson.GetBytes(line, "input")
		var argsStr string
		if input.Type == gjson.String {
			argsStr = input.String()
		} else if input.Exists() {
			argsStr = input.Raw
		} else {
			argsStr = "{}"
		}
		toolCall := map[string]any{
			"index": idx,
			"id":    id,
			"type":  "function",
			"function": map[string]any{
				"name":      gjson.GetBytes(line, "toolName").String(),
				"arguments": argsStr,
			},
		}
		var delta map[string]any
		if s.chunkIndex == 0 {
			delta = map[string]any{"role": "assistant", "tool_calls": []any{toolCall}}
		} else {
			delta = map[string]any{"tool_calls": []any{toolCall}}
		}
		s.chunkIndex++
		return [][]byte{s.makeChunk(delta, "")}
	case "finish-step":
		s.finishReason = mapFinishReason(gjson.GetBytes(line, "finishReason").String())
		usage := gjson.GetBytes(line, "usage")
		if usage.Exists() {
			s.usage = parseCCUsage(usage)
		}
		return nil
	case "finish":
		finishReason := s.finishReason
		if finishReason == "" {
			finishReason = mapFinishReason(gjson.GetBytes(line, "finishReason").String())
			if finishReason == "" {
				finishReason = "stop"
			}
		}
		final := s.makeChunk(map[string]any{}, finishReason)
		totalUsage := gjson.GetBytes(line, "totalUsage")
		if !totalUsage.Exists() {
			totalUsage = gjson.GetBytes(line, "usage")
		}
		if totalUsage.Exists() {
			u := parseCCUsage(totalUsage)
			if u != nil {
				s.usage = u
				// Inject usage at root of chunk.
				final = injectUsage(final, u)
			}
		}
		return [][]byte{final}
	case "error":
		errVal := gjson.GetBytes(line, "error").String()
		if errVal == "" {
			errVal = gjson.GetBytes(line, "message").String()
		}
		if errVal == "" {
			errVal = "unknown"
		}
		s.finishReason = "stop"
		errChunk := s.makeChunk(map[string]any{"content": fmt.Sprintf("\n\n[CommandCode error: %s]", errVal)}, "")
		stopChunk := s.makeChunk(map[string]any{}, "stop")
		return [][]byte{errChunk, stopChunk}
	default:
		return nil
	}
}

func (s *commandCodeStreamState) makeChunk(delta map[string]any, finishReason string) []byte {
	choice := map[string]any{
		"index": 0,
		"delta": delta,
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	} else {
		choice["finish_reason"] = nil
	}
	chunk := map[string]any{
		"id":      s.responseID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{choice},
	}
	out, _ := json.Marshal(chunk)
	return out
}

func parseCCUsage(node gjson.Result) *ccUsage {
	if !node.Exists() {
		return nil
	}
	in := int(node.Get("inputTokens").Int())
	out := int(node.Get("outputTokens").Int())
	total := int(node.Get("totalTokens").Int())
	if total == 0 {
		total = in + out
	}
	if in == 0 && out == 0 && total == 0 {
		return nil
	}
	return &ccUsage{PromptTokens: in, CompletionTokens: out, TotalTokens: total}
}

func injectUsage(chunk []byte, u *ccUsage) []byte {
	var doc map[string]any
	if err := json.Unmarshal(chunk, &doc); err != nil {
		return chunk
	}
	doc["usage"] = map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return chunk
	}
	return out
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "stop"
	case "length":
		return "length"
	case "tool-calls", "tool_use":
		return "tool_calls"
	case "content-filter":
		return "content_filter"
	case "error":
		return "stop"
	case "":
		return ""
	default:
		return reason
	}
}

// aggregateCommandCodeStream consumes the entire NDJSON stream into an
// in-memory list of OpenAI chunks (used for the non-streaming Execute path).
func aggregateCommandCodeStream(ctx context.Context, cfg *config.Config, body io.Reader, model string) ([][]byte, *ccUsage, error) {
	state := newCommandCodeStreamState(model)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 52_428_800)
	chunks := make([][]byte, 0)
	for scanner.Scan() {
		line := scanner.Bytes()
		helps.AppendAPIResponseChunk(ctx, cfg, line)
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			trimmed = bytes.TrimSpace(trimmed[len("data:"):])
			if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
				continue
			}
		}
		out := state.translate(trimmed)
		chunks = append(chunks, out...)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return chunks, state.usage, nil
}

// buildOpenAINonStreamFromChunks aggregates streaming chunks into a single
// OpenAI chat-completion JSON document for non-streaming callers.
func buildOpenAINonStreamFromChunks(model string, chunks [][]byte, usage *ccUsage) []byte {
	var content strings.Builder
	finishReason := "stop"
	type toolCall struct {
		ID        string
		Name      string
		Arguments strings.Builder
	}
	toolCalls := make(map[int]*toolCall)
	toolOrder := make([]int, 0)
	for _, raw := range chunks {
		choices := gjson.GetBytes(raw, "choices")
		if !choices.IsArray() {
			continue
		}
		choice := choices.Array()[0]
		delta := choice.Get("delta")
		if c := delta.Get("content"); c.Exists() {
			content.WriteString(c.String())
		}
		delta.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			idx := int(tc.Get("index").Int())
			t, ok := toolCalls[idx]
			if !ok {
				t = &toolCall{}
				toolCalls[idx] = t
				toolOrder = append(toolOrder, idx)
			}
			if id := tc.Get("id").String(); id != "" {
				t.ID = id
			}
			if name := tc.Get("function.name").String(); name != "" {
				t.Name = name
			}
			if args := tc.Get("function.arguments").String(); args != "" {
				t.Arguments.WriteString(args)
			}
			return true
		})
		if fr := choice.Get("finish_reason").String(); fr != "" {
			finishReason = fr
		}
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if len(toolOrder) > 0 {
		tcArr := make([]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			t := toolCalls[idx]
			tcArr = append(tcArr, map[string]any{
				"id":   t.ID,
				"type": "function",
				"function": map[string]any{
					"name":      t.Name,
					"arguments": t.Arguments.String(),
				},
			})
		}
		message["tool_calls"] = tcArr
	}
	doc := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		doc["usage"] = map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	out, _ := json.Marshal(doc)
	return out
}
