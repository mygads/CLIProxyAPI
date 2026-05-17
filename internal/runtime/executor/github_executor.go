package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	githubauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/github"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// GitHubCopilotExecutor is a fully-wired executor for the GitHub Copilot
// provider. The implementation mirrors KimiExecutor closely — Copilot's
// upstream API is OpenAI-shaped, so the bulk of the work is just header
// management and Copilot-token rotation.
//
// Upstream auth has two layers:
//
//  1. The long-lived GitHub OAuth access_token (prefix "gho_") — never
//     expires until revoked by the user. Lives in auth.Metadata["access_token"].
//
//  2. The short-lived Copilot bearer token (~30min TTL) minted from the
//     access_token via /copilot_internal/v2/token. Cached under
//     auth.Metadata["copilot_token"] + "copilot_expires_at".
//
// Each Execute/ExecuteStream call makes sure a fresh Copilot token is
// available before building the upstream request, exchanges on expiry, and
// retries once on 401 in case the token was revoked early.
type GitHubCopilotExecutor struct {
	cfg *config.Config
}

// NewGitHubCopilotExecutor builds the executor with a config reference.
// The config is used for proxy-aware HTTP clients and request logging —
// the executor itself is stateless otherwise.
func NewGitHubCopilotExecutor(cfg *config.Config) *GitHubCopilotExecutor {
	return &GitHubCopilotExecutor{cfg: cfg}
}

// Identifier is the provider key. It matches the Auth.Provider field
// written by the github management handler.
func (e *GitHubCopilotExecutor) Identifier() string { return "github" }

// Upstream endpoints. Hard-coded because GitHub Copilot serves all
// accounts from the same hostname — no per-tenant routing like Vertex.
const (
	githubCopilotBaseURL     = "https://api.githubcopilot.com"
	githubCopilotChatPath    = "/chat/completions"
	githubCopilotModelsPath  = "/models"
	// Early-rotate window: if the cached Copilot token expires within
	// this many seconds, exchange a fresh one before the request rather
	// than racing the clock. 60s is generous — Copilot tokens are
	// typically ~30min so the wasted exchange rate is <5%.
	githubCopilotRefreshSkew = 60
)

// copilotHeaders returns the base header set Copilot expects. Values
// mirror OmniRoute's providerHeaderProfiles.ts (2026-05) — keeping them
// byte-exact reduces the chance GitHub gates us on a fingerprint mismatch.
//
// `initiator` follows the Copilot billing convention: "user" turns count
// against the paid quota; "agent" turns (autonomous tool-call continuations)
// are free. When the request carries X-Initiator we forward it here; otherwise
// the default "user" is used.
func copilotHeaders(copilotToken, initiator string) http.Header {
	if initiator != "agent" && initiator != "user" {
		initiator = githubauth.DefaultInitiator
	}
	h := make(http.Header, 11)
	h.Set("Authorization", "Bearer "+copilotToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Copilot-Integration-Id", githubauth.IntegrationID)
	h.Set("Editor-Version", githubauth.EditorVersion)
	h.Set("Editor-Plugin-Version", githubauth.ChatPluginVersion)
	h.Set("OpenAI-Intent", githubauth.OpenAIIntent)
	h.Set("User-Agent", githubauth.UserAgent)
	h.Set("X-Github-Api-Version", githubauth.APIVersion)
	h.Set("X-Vscode-User-Agent-Library-Version", githubauth.UserAgentLibraryVersion)
	h.Set("X-Initiator", initiator)
	return h
}

// extractClientInitiator pulls the X-Initiator header from the incoming
// request, normalizing case. Returns "" when absent so copilotHeaders falls
// back to the "user" default.
func extractClientInitiator(reqHeaders http.Header) string {
	if reqHeaders == nil {
		return ""
	}
	v := reqHeaders.Get("X-Initiator")
	if v == "" {
		v = reqHeaders.Get("x-initiator")
	}
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "agent", "user":
		return v
	}
	return ""
}

// ensureCopilotToken returns a valid Copilot bearer token for the
// credential. The two-layer exchange/refresh logic lives in
// internal/auth/github/refresh_helper.go so management quota handlers can
// reuse it without depending on this executor.
func (e *GitHubCopilotExecutor) ensureCopilotToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("github copilot: nil auth")
	}
	res, err := githubauth.EnsureCopilotToken(ctx, auth.ID, auth.Metadata)
	if err != nil {
		return "", err
	}
	return res.CopilotToken, nil
}

// copilotChatURL returns the full chat-completions URL. Some Copilot
// tenants publish a per-account API host in the token-exchange response's
// `endpoints` map; when present, we honor it.
func (e *GitHubCopilotExecutor) copilotChatURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		if eps, ok := auth.Metadata["copilot_endpoints"].(map[string]string); ok {
			if u, ok := eps["api"]; ok && u != "" {
				return strings.TrimRight(u, "/") + githubCopilotChatPath
			}
		}
		if eps, ok := auth.Metadata["copilot_endpoints"].(map[string]any); ok {
			if u, ok := eps["api"].(string); ok && u != "" {
				return strings.TrimRight(u, "/") + githubCopilotChatPath
			}
		}
	}
	return githubCopilotBaseURL + githubCopilotChatPath
}

// PrepareRequest attaches Copilot auth + headers to an arbitrary request.
// Used by HttpRequest — management probes and debug tooling call through
// here so they respect the same rotation logic as Execute.
//
// PrepareRequest uses the default "user" initiator because management
// probes do not carry an upstream X-Initiator. The main request path in
// doChatRequest derives the initiator from the caller's headers.
func (e *GitHubCopilotExecutor) PrepareRequest(ctx context.Context, req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, err := e.ensureCopilotToken(ctx, auth)
	if err != nil {
		return err
	}
	for k, vs := range copilotHeaders(token, "") {
		req.Header.Set(k, vs[0])
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest attaches Copilot auth to an externally-built request and
// runs it through the proxy-aware HTTP client.
func (e *GitHubCopilotExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("github copilot: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(ctx, httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, e.cfg.UpstreamTimeout())
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming chat completion request to Copilot.
// The payload is translated from opts.SourceFormat to OpenAI format,
// posted upstream, and the response is translated back.
func (e *GitHubCopilotExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)

	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return resp, fmt.Errorf("github copilot: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "openai", e.Identifier())
	if err != nil {
		return resp, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, requestPath)

	// One retry on 401 — Copilot tokens can be invalidated before the
	// cached expiry (policy changes, user revokes, etc.). The retry path
	// forces a re-exchange.
	initiator := extractClientInitiator(opts.Headers)
	for attempt := 0; attempt < 2; attempt++ {
		httpResp, doErr := e.doChatRequest(ctx, auth, body, false, initiator)
		if doErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, doErr)
			return resp, doErr
		}
		if httpResp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = httpResp.Body.Close()
			e.invalidateCopilotToken(auth)
			continue
		}
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("github copilot: close body: %v", errClose)
			}
		}()
		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			helps.AppendAPIResponseChunk(ctx, e.cfg, b)
			helps.LogWithRequestID(ctx).Debugf("request error, status=%d body=%s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
			return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
		}
		data, readErr := io.ReadAll(httpResp.Body)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			return resp, readErr
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
		return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
	}
	// Both attempts returned 401 — surface the second so the scheduler
	// can mark the credential unavailable.
	return resp, &cliproxyauth.Error{
		HTTPStatus: http.StatusUnauthorized,
		Message:    "github copilot: persistent 401 after token re-exchange",
	}
}

// ExecuteStream performs a streaming chat completion request to Copilot.
func (e *GitHubCopilotExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return nil, fmt.Errorf("github copilot: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "openai", e.Identifier())
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("github copilot: set stream_options: %w", err)
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel, requestPath)

	// One retry on 401 — mirror the non-streaming path.
	initiator := extractClientInitiator(opts.Headers)
	var httpResp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		var doErr error
		httpResp, doErr = e.doChatRequest(ctx, auth, body, true, initiator)
		if doErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, doErr)
			return nil, doErr
		}
		if httpResp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = httpResp.Body.Close()
			e.invalidateCopilotToken(auth)
			continue
		}
		break
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("stream error, status=%d body=%s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("github copilot: close body: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("github copilot: close stream body: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB — Copilot emits long tool-call chunks
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		// Emit the final [DONE] signal so downstream translators can
		// close their stream state cleanly.
		doneChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
			case <-ctx.Done():
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// doChatRequest builds and sends a single chat-completions request. It is
// used by both Execute and ExecuteStream so the auth + header +
// proxy-client path stays in one place. `initiator` comes from the caller's
// X-Initiator header so Copilot's billing layer correctly counts agent
// continuations as free.
func (e *GitHubCopilotExecutor) doChatRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte, stream bool, initiator string) (*http.Response, error) {
	token, err := e.ensureCopilotToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	url := e.copilotChatURL(auth)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range copilotHeaders(token, initiator) {
		httpReq.Header.Set(k, vs[0])
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
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
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, e.cfg.UpstreamTimeout())
	return httpClient.Do(httpReq)
}

// invalidateCopilotToken clears the cached Copilot token so the next
// ensureCopilotToken call forces a fresh exchange. Called when upstream
// returns 401 — the long-lived GitHub access_token is kept.
func (e *GitHubCopilotExecutor) invalidateCopilotToken(auth *cliproxyauth.Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, "copilot_token")
	delete(auth.Metadata, "copilot_expires_at")
}

// Refresh proactively exchanges the GitHub access_token for a fresh
// Copilot bearer token. Unlike OAuth refresh-token flows, this never
// rotates the long-lived credential — if the GitHub access_token is
// revoked, the user must re-authorize.
func (e *GitHubCopilotExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("github copilot: refresh called with nil auth")
	}
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	if _, err := e.ensureCopilotToken(ctx, auth); err != nil {
		return nil, err
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	return auth, nil
}

// CountTokens is intentionally unimplemented — Copilot does not publish a
// dedicated token counter and we refuse to ship an unverified tiktoken
// approximation. Callers get a clean 501 so upstream routing can fall back.
func (e *GitHubCopilotExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "github copilot: CountTokens not supported by upstream",
	}
}
