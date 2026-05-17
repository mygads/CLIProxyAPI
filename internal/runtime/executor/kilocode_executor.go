package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	kilocodeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kilocode"
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

// KiloCodeExecutor proxies chat completions to KiloCode's OpenRouter-
// compatible endpoint. KiloCode's catalog is OpenRouter-style with
// vendor-prefixed model IDs (e.g. "anthropic/claude-opus-4.7"), so we
// pass the requested model through unchanged.
//
// There is no refresh token flow — ensure* just returns the persisted
// access token. If that token is rejected (401), the credential needs
// manual re-auth; we do not attempt silent recovery.
type KiloCodeExecutor struct {
	cfg *config.Config
}

// NewKiloCodeExecutor builds the executor.
func NewKiloCodeExecutor(cfg *config.Config) *KiloCodeExecutor {
	return &KiloCodeExecutor{cfg: cfg}
}

// Identifier is the provider key; matches auth.Provider written by
// the management handler.
func (e *KiloCodeExecutor) Identifier() string { return "kilocode" }

// kilocodeHeaders returns the standard attribution + auth header set.
func kilocodeHeaders(accessToken string) http.Header {
	h := make(http.Header, 4)
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	// KiloCode uses OpenRouter-compatible attribution.
	h.Set("HTTP-Referer", "https://kilo.ai")
	return h
}

// accessToken extracts the token from auth.Metadata. KiloCode credentials
// have no refresh step, so there is no fallback — returning empty means
// the user needs to re-auth.
func (e *KiloCodeExecutor) accessToken(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil || auth.Metadata == nil {
		return "", fmt.Errorf("kilocode: nil auth/metadata")
	}
	token, _ := auth.Metadata["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return "", &cliproxyauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "kilocode: missing access_token; user must log in again",
		}
	}
	return token, nil
}

// PrepareRequest wires auth into the outgoing request (management
// probe path). The hot path builds headers inline.
func (e *KiloCodeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if token, _ := auth.Metadata["access_token"].(string); strings.TrimSpace(token) != "" {
		for k, vs := range kilocodeHeaders(token) {
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

// HttpRequest runs an externally-built request through this executor.
func (e *KiloCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kilocode executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	token, err := e.accessToken(auth)
	if err != nil {
		return nil, err
	}
	for k, vs := range kilocodeHeaders(token) {
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

// Execute sends a non-streaming chat completion.
func (e *KiloCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return resp, fmt.Errorf("kilocode: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "kilocode", e.Identifier())
	if err != nil {
		return resp, err
	}

	httpResp, err := e.doKiloCodeRequest(ctx, auth, body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kilocode executor: close body: %v", errClose)
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

// ExecuteStream sends a streaming chat completion.
func (e *KiloCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return nil, fmt.Errorf("kilocode: set model: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "kilocode", e.Identifier())
	if err != nil {
		return nil, err
	}

	httpResp, err := e.doKiloCodeRequest(ctx, auth, body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kilocode executor: close body: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kilocode executor: close stream body: %v", errClose)
			}
		}()
		var param any
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576)
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

// doKiloCodeRequest posts the body to KiloCode's chat endpoint. No
// 401-retry loop — there is no refresh mechanism.
func (e *KiloCodeExecutor) doKiloCodeRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte) (*http.Response, error) {
	token, err := e.accessToken(auth)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, kilocodeauth.ChatCompletionsURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range kilocodeHeaders(token) {
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
		URL:       kilocodeauth.ChatCompletionsURL,
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

// Refresh is a no-op for KiloCode — the credential has no refresh
// token. Returning the unchanged auth keeps the generic manager happy.
func (e *KiloCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("kilocode: refresh called with nil auth")
	}
	return auth, nil
}

// CountTokens is not supported by KiloCode — callers get a clean 501.
func (e *KiloCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "kilocode: CountTokens not supported",
	}
}
