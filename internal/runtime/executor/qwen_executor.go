package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	qwenauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qwen"
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

// QwenExecutor is a stateless executor for Qwen Code using the OpenAI-
// compatible chat-completions shape. The upstream endpoint expects
// Dashscope-style headers on top of a standard Bearer auth — missing
// X-Dashscope-AuthType is the most common cause of a silent 401.
//
// Refresh strategy: pure refresh-token rotation. When the cached access
// token is within 60s of expiry or a 401 comes back, we call
// qwenauth.Refresh with the stored refresh_token. Qwen sometimes
// rotates the refresh_token on refresh; we persist the rotated value
// back into auth.Metadata to avoid bricking future refreshes.
type QwenExecutor struct {
	cfg *config.Config

	// refreshMu serializes per-credential refreshes so parallel requests
	// do not race on the rotated refresh_token.
	refreshMu sync.Map
}

// NewQwenExecutor builds the executor with the supplied config.
func NewQwenExecutor(cfg *config.Config) *QwenExecutor { return &QwenExecutor{cfg: cfg} }

// Identifier is the provider key; matches auth.Provider written by the
// management handler.
func (e *QwenExecutor) Identifier() string { return "qwen" }

// qwenBaseURL returns the effective chat endpoint. When a resource_url
// is present in auth metadata (set by the login/refresh response) we
// prefer that; otherwise the default CLI URL.
func qwenBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		if ru, ok := auth.Metadata["resource_url"].(string); ok && ru != "" {
			ru = strings.TrimSpace(ru)
			if !strings.HasPrefix(ru, "http://") && !strings.HasPrefix(ru, "https://") {
				ru = "https://" + ru
			}
			return strings.TrimSuffix(ru, "/")
		}
	}
	return strings.TrimSuffix(qwenauth.DefaultBaseURL, "/")
}

// qwenHeaders returns the full Dashscope-compatible header set that
// OmniRoute's getQwenOauthHeaders() emits. See
// providerHeaderProfiles.ts (2026-05) for the reference values.
func qwenHeaders(accessToken string) http.Header {
	h := make(http.Header, 10)
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", qwenauth.UserAgent())
	h.Set(qwenauth.DashscopeAuthTypeHeader, qwenauth.DashscopeAuthTypeValue)
	h.Set(qwenauth.DashscopeCacheCtrlHeader, qwenauth.DashscopeCacheCtrlValue)
	h.Set("X-Dashscope-UserAgent", qwenauth.UserAgent())
	h.Set("X-Stainless-Lang", qwenauth.StainlessLang)
	h.Set("X-Stainless-Package-Version", qwenauth.StainlessPackageVersion)
	h.Set("X-Stainless-Retry-Count", qwenauth.StainlessRetryCount)
	h.Set("X-Stainless-Runtime", qwenauth.StainlessRuntime)
	return h
}

// ensureAccessToken returns a valid Qwen access token, refreshing when
// the cached one is past `expired` (minus 60s skew). Serialized per
// auth.ID to avoid parallel refreshes racing on the rotated refresh_token.
func (e *QwenExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil || auth.Metadata == nil {
		return "", fmt.Errorf("qwen: nil auth/metadata")
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	var expUnix int64
	switch v := auth.Metadata["expires_at"].(type) {
	case int64:
		expUnix = v
	case float64:
		expUnix = int64(v)
	case int:
		expUnix = int64(v)
	}
	if accessToken != "" && expUnix > time.Now().Unix()+60 {
		return accessToken, nil
	}

	muAny, _ := e.refreshMu.LoadOrStore(auth.ID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under lock.
	accessToken, _ = auth.Metadata["access_token"].(string)
	switch v := auth.Metadata["expires_at"].(type) {
	case int64:
		expUnix = v
	case float64:
		expUnix = int64(v)
	case int:
		expUnix = int64(v)
	}
	if accessToken != "" && expUnix > time.Now().Unix()+60 {
		return accessToken, nil
	}

	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	if refreshToken == "" {
		return "", &cliproxyauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "qwen: missing refresh_token; user must log in again",
		}
	}
	resp, err := qwenauth.Refresh(ctx, "", refreshToken)
	if err != nil {
		return "", fmt.Errorf("qwen: refresh: %w", err)
	}
	auth.Metadata["access_token"] = resp.AccessToken
	if resp.ExpiresIn > 0 {
		auth.Metadata["expires_at"] = time.Now().Unix() + int64(resp.ExpiresIn)
	}
	if rotated := strings.TrimSpace(resp.RefreshToken); rotated != "" {
		auth.Metadata["refresh_token"] = rotated
	}
	if resp.ResourceURL != "" {
		auth.Metadata["resource_url"] = resp.ResourceURL
	}
	return resp.AccessToken, nil
}

// PrepareRequest is wired for the generic management probe path. For
// the hot path (Execute/ExecuteStream) we mint headers inline.
func (e *QwenExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if token, _ := auth.Metadata["access_token"].(string); strings.TrimSpace(token) != "" {
		for k, vs := range qwenHeaders(token) {
			req.Header.Set(k, vs[0])
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest runs an externally-built request through this executor's
// auth + proxy-aware client. Used by management probes and debug tools.
func (e *QwenExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("qwen executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	token, err := e.ensureAccessToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	for k, vs := range qwenHeaders(token) {
		httpReq.Header.Set(k, vs[0])
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, e.cfg.UpstreamTimeout())
	return httpClient.Do(httpReq)
}

// Execute sends a non-streaming chat completion to Qwen and returns
// the response in the caller's requested format.
func (e *QwenExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return resp, fmt.Errorf("qwen: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "qwen", e.Identifier())
	if err != nil {
		return resp, err
	}

	httpResp, err := e.doQwenRequest(ctx, auth, body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close body: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	raw, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return resp, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return resp, statusErr{code: httpResp.StatusCode, msg: string(raw)}
	}

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, raw, &param)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(raw))
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// ExecuteStream sends a streaming chat completion to Qwen and forwards
// SSE chunks to the caller's channel.
func (e *QwenExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return nil, fmt.Errorf("qwen: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "qwen", e.Identifier())
	if err != nil {
		return nil, err
	}

	httpResp, err := e.doQwenRequest(ctx, auth, body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close body: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("qwen executor: close stream body: %v", errClose)
			}
		}()
		var param any
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
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

// doQwenRequest posts the body to Qwen's chat endpoint with the
// one-shot 401 retry pattern. On 401 the cached access token is
// invalidated so the next attempt forces a refresh.
func (e *QwenExecutor) doQwenRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte) (*http.Response, error) {
	var lastResp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		token, err := e.ensureAccessToken(ctx, auth)
		if err != nil {
			return nil, err
		}
		url := qwenBaseURL(auth) + "/v1/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range qwenHeaders(token) {
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
		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, e.cfg.UpstreamTimeout())
		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
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
			Message:    "qwen: persistent 401 after refresh",
		}
	}
	return lastResp, nil
}

// Refresh exchanges the refresh token for a fresh access token and
// persists the result back into auth.Metadata. Safe to call from hot
// paths — ensureAccessToken invokes it when cached token is nearing expiry.
func (e *QwenExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("qwen: refresh called with nil auth")
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

// CountTokens is not supported by Qwen — callers get a clean 501 so
// combo routing can fall through.
func (e *QwenExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "qwen: CountTokens not supported",
	}
}
