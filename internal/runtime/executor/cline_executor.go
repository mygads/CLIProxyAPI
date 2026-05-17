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

	clineauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cline"
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

// ClineExecutor is a stateless executor for the Cline Bot provider.
// Cline is OpenAI-compatible at /api/v1/chat/completions; the quirk is
// that requests must carry OpenRouter-style HTTP-Referer + X-Title
// attribution headers — missing X-Title triggers a silent 429 on the
// quota path.
type ClineExecutor struct {
	cfg *config.Config

	// refreshMu serializes per-credential refreshes so parallel
	// requests do not race on the rotated refresh_token.
	refreshMu sync.Map
}

// NewClineExecutor builds the executor.
func NewClineExecutor(cfg *config.Config) *ClineExecutor { return &ClineExecutor{cfg: cfg} }

// Identifier is the provider key; matches auth.Provider written by the
// management handler.
func (e *ClineExecutor) Identifier() string { return "cline" }

// clineHeaders returns the required attribution header set. Authorization
// is a separate concern (set at call time from the cached access token).
func clineHeaders(accessToken string) http.Header {
	h := make(http.Header, 5)
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set(clineauth.RefererHeader, clineauth.RefererValue)
	h.Set(clineauth.TitleHeader, clineauth.TitleValue)
	return h
}

// ensureAccessToken returns a valid Cline access token, refreshing
// when the cached one is past `expires_at` (minus 60s skew).
// Serialized per auth.ID so parallel requests do not race.
func (e *ClineExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil || auth.Metadata == nil {
		return "", fmt.Errorf("cline: nil auth/metadata")
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
			Message:    "cline: missing refresh_token; user must log in again",
		}
	}
	resp, err := clineauth.Refresh(ctx, refreshToken)
	if err != nil {
		return "", fmt.Errorf("cline: refresh: %w", err)
	}
	auth.Metadata["access_token"] = resp.AccessToken
	if resp.ExpiresAt != "" {
		if ts := clineauth.ParseExpiresAt(resp.ExpiresAt); ts > 0 {
			auth.Metadata["expires_at"] = ts
		}
	}
	if rotated := strings.TrimSpace(resp.RefreshToken); rotated != "" {
		auth.Metadata["refresh_token"] = rotated
	}
	return resp.AccessToken, nil
}

// PrepareRequest wires authorization + attribution headers for the
// generic management-probe path. The hot path builds headers inline.
func (e *ClineExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if token, _ := auth.Metadata["access_token"].(string); strings.TrimSpace(token) != "" {
		for k, vs := range clineHeaders(token) {
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
func (e *ClineExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("cline executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	token, err := e.ensureAccessToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	for k, vs := range clineHeaders(token) {
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

// Execute sends a non-streaming chat completion and returns the response
// translated back to the caller's requested format.
func (e *ClineExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return resp, fmt.Errorf("cline: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "cline", e.Identifier())
	if err != nil {
		return resp, err
	}

	httpResp, err := e.doClineRequest(ctx, auth, body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("cline executor: close body: %v", errClose)
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

// ExecuteStream sends a streaming chat completion and forwards SSE
// chunks to the caller's channel.
func (e *ClineExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return nil, fmt.Errorf("cline: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "cline", e.Identifier())
	if err != nil {
		return nil, err
	}

	httpResp, err := e.doClineRequest(ctx, auth, body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("cline executor: close body: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("cline executor: close stream body: %v", errClose)
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

// doClineRequest posts to Cline's chat endpoint with one-shot 401
// retry. On 401, cached access token is invalidated so the next
// attempt forces a refresh.
func (e *ClineExecutor) doClineRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte) (*http.Response, error) {
	var lastResp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		token, err := e.ensureAccessToken(ctx, auth)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, clineauth.ChatCompletionsURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, vs := range clineHeaders(token) {
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
			URL:       clineauth.ChatCompletionsURL,
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
			Message:    "cline: persistent 401 after refresh",
		}
	}
	return lastResp, nil
}

// Refresh persists a rotated access token back into auth.Metadata.
func (e *ClineExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("cline: refresh called with nil auth")
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

// CountTokens is not supported by Cline — callers get a clean 501.
func (e *ClineExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "cline: CountTokens not supported",
	}
}
