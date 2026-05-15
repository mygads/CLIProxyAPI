package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/eventstream"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/google/uuid"
)

// KiroExecutor is a wiring-complete executor for the Kiro AI provider.
//
// SCOPE NOTE (2026-05): this implementation handles the *basic* single-turn
// chat path — each incoming OpenAI message is concatenated into one
// userInputMessage.content string, sent to CodeWhisperer, and the resulting
// eventstream is decoded back to an OpenAI-compatible response. It is
// deliberately narrower than 9router's Kiro translator:
//
//   - No full conversation history: previous turns are flattened into the
//     single content string rather than being sent as a history array.
//   - No tool/function calling: tools in the request are dropped.
//   - No multimodal (images): content parts that are not plain text are
//     rendered as a placeholder string.
//
// Those gaps are tracked in docs/PHASE-2-OAUTH-PROVIDERS-FOLLOWUP.md and are
// safe to close incrementally once there are credentials to test against.
// The narrow path gets us end-to-end traffic through the decoder today.
type KiroExecutor struct {
	cfg *config.Config
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
	res, err := kiroauth.RefreshIfExpired(ctx, auth.ID, auth.Metadata)
	if err != nil {
		return "", err
	}
	return res.AccessToken, nil
}

// buildKiroPayload converts an OpenAI-format request to CodeWhisperer's
// GenerateAssistantResponse shape. See SCOPE NOTE at the top of the file —
// this is the single-turn reduction, not the full port.
func (e *KiroExecutor) buildKiroPayload(openaiBody []byte, model string, auth *cliproxyauth.Auth) ([]byte, error) {
	messages := gjson.GetBytes(openaiBody, "messages").Array()

	// Flatten prior turns into a single prompt string. Assistant turns
	// are labelled so the model knows they are its own past replies.
	var sb strings.Builder
	for i, msg := range messages {
		role := msg.Get("role").String()
		content := extractMessageContent(msg)
		if content == "" {
			continue
		}
		if i == len(messages)-1 && role == "user" {
			// Last user message goes into currentMessage, not history.
			continue
		}
		switch role {
		case "system":
			sb.WriteString("[System]\n")
		case "user":
			sb.WriteString("[User]\n")
		case "assistant":
			sb.WriteString("[Assistant]\n")
		default:
			sb.WriteString("[" + role + "]\n")
		}
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// The *actual* current turn is the last user message.
	var currentContent string
	if n := len(messages); n > 0 && messages[n-1].Get("role").String() == "user" {
		currentContent = extractMessageContent(messages[n-1])
	}
	if currentContent == "" && sb.Len() == 0 {
		return nil, fmt.Errorf("kiro: no user content to send")
	}

	// Combine flattened history with the current turn.
	var finalContent string
	if sb.Len() > 0 {
		finalContent = sb.String() + "[User]\n" + currentContent
	} else {
		finalContent = currentContent
	}
	// Light context marker mirrors the 9router reference so the model
	// behaves consistently with that deployment.
	finalContent = fmt.Sprintf("[Context: Current time is %s]\n\n%s",
		time.Now().UTC().Format(time.RFC3339), finalContent)

	payload := map[string]any{
		"conversationState": map[string]any{
			"chatTriggerType": "MANUAL",
			"conversationId":  uuid.New().String(),
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": finalContent,
					"modelId": model,
					"origin":  "AI_EDITOR",
				},
			},
			"history": []any{},
		},
	}
	if profileArn, _ := auth.Metadata["profile_arn"].(string); profileArn != "" {
		payload["profileArn"] = profileArn
	}

	// inferenceConfig is optional — honor max_tokens/temperature/top_p if set.
	inferenceConfig := map[string]any{}
	if v := gjson.GetBytes(openaiBody, "max_tokens"); v.Exists() {
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

// extractMessageContent coerces an OpenAI-style message content into a
// plain string. Content may be a string OR an array of parts; for arrays
// we concatenate only text parts. Non-text parts (images, tool results)
// are rendered as a placeholder so the user sees something was there.
func extractMessageContent(msg gjson.Result) string {
	content := msg.Get("content")
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if !content.IsArray() {
		return ""
	}
	var sb strings.Builder
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text":
			sb.WriteString(part.Get("text").String())
		case "image_url", "image":
			sb.WriteString("[image attachment]")
		case "tool_result":
			if v := part.Get("content"); v.Exists() {
				sb.WriteString(v.String())
			}
		}
	}
	return strings.TrimSpace(sb.String())
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

	assembled, err := assembleKiroResponse(ctx, e.cfg, httpResp.Body, baseModel)
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
		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
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
func assembleKiroResponse(ctx context.Context, cfg *config.Config, body io.Reader, model string) ([]byte, error) {
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
	emit(buildSSEChunk(streamID, now, model, nil, finishReason))
	emit([]byte("[DONE]"))
}

// buildSSEChunk constructs one `data: {...}` event body in OpenAI
// chat.completion.chunk shape. delta is the per-chunk payload (role,
// content). finishReason is set only on the terminating chunk.
func buildSSEChunk(id string, created int64, model string, delta map[string]any, finishReason string) []byte {
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
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}
