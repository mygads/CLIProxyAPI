// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"golang.org/x/net/context"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds = 0
	defaultStreamingBootstrapRetries = 0
)

type pinnedAuthContextKey struct{}
type selectedAuthCallbackContextKey struct{}
type executionSessionContextKey struct{}
type disallowFreeAuthContextKey struct{}

// WithPinnedAuthID returns a child context that requests execution on a specific auth ID.
func WithPinnedAuthID(ctx context.Context, authID string) context.Context {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedAuthContextKey{}, authID)
}

// WithSelectedAuthIDCallback returns a child context that receives the selected auth ID.
func WithSelectedAuthIDCallback(ctx context.Context, callback func(string)) context.Context {
	if callback == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectedAuthCallbackContextKey{}, callback)
}

// WithExecutionSessionID returns a child context tagged with a long-lived execution session ID.
func WithExecutionSessionID(ctx context.Context, sessionID string) context.Context {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionSessionContextKey{}, sessionID)
}

// WithDisallowFreeAuth returns a child context that requests skipping known free-tier credentials.
func WithDisallowFreeAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, disallowFreeAuthContextKey{}, true)
}

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned as-is to preserve upstream error payloads.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only include it if the client explicitly provides it.
	key := ""
	requestPath := ""
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			requestPath = strings.TrimSpace(ginCtx.FullPath())
			if requestPath == "" && ginCtx.Request.URL != nil {
				requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
			}
		}
	}

	meta := make(map[string]any)
	if key != "" {
		meta[idempotencyKeyMetadataKey] = key
	}
	if requestPath != "" {
		meta[coreexecutor.RequestPathMetadataKey] = requestPath
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if disallowFreeAuthFromContext(ctx) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
	return meta
}

// headersFromContext extracts the original HTTP request headers from the gin context
// embedded in the provided context. This allows session affinity selectors to read
// client headers like X-Amp-Thread-Id.
func headersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.Request.Header.Clone()
	}
	return nil
}

func pinnedAuthIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(pinnedAuthContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func selectedAuthIDCallbackFromContext(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(selectedAuthCallbackContextKey{})
	if callback, ok := raw.(func(string)); ok && callback != nil {
		return callback
	}
	return nil
}

func executionSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(executionSessionContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func disallowFreeAuthFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw, ok := ctx.Value(disallowFreeAuthContextKey{}).(bool)
	return ok && raw
}

// ComboResolver expands a virtual combo name into the ordered list of real
// prefixed models that should be tried. The `internal/combo` package provides
// the production implementation; handlers talk to it through this interface
// so the SDK package does not take a direct dependency on internal packages.
type ComboResolver interface {
	// Has reports whether name is a known combo (regardless of status).
	Has(name string) bool
	// FirstCandidate returns the head of the fallback chain for name. Empty
	// string means "not a combo, pass through".
	FirstCandidate(name string) string
	// Candidates returns the full ordered fallback list for a combo. The
	// second return is the original combo name (for logging) — an empty
	// slice + false means the name is not a combo. Callers iterate this
	// list, stopping on the first success or at the last entry.
	Candidates(name string) ([]ComboCandidate, bool)
}

// ComboCandidate is one step in a fallback chain returned by
// ComboResolver.Candidates.
type ComboCandidate struct {
	// Model is the prefixed upstream identifier to try (e.g. "cc/claude-opus-4-7").
	Model string
	// TriggerOn lists response-body substrings that should cause a fall
	// through to the next entry. Empty = "any retriable status triggers".
	TriggerOn []string
	// IsLast is true when this is the final entry in the chain. Callers
	// should surface upstream errors as-is on this entry.
	IsLast bool
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig

	// Combos is optional. When set, incoming requests whose `model` field
	// matches a combo name are rewritten to the combo's first candidate
	// before provider lookup. A nil resolver disables the feature.
	Combos ComboResolver
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	return &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) { h.Cfg = cfg }

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID = logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)

	endpoint := ""
	if c != nil && c.Request != nil {
		path := strings.TrimSpace(c.FullPath())
		if path == "" && c.Request.URL != nil {
			path = strings.TrimSpace(c.Request.URL.Path)
		}
		if path != "" {
			method := strings.TrimSpace(c.Request.Method)
			if method != "" {
				endpoint = method + " " + path
			} else {
				endpoint = path
			}
		}
	}
	if endpoint != "" {
		newCtx = logging.WithEndpoint(newCtx, endpoint)
	}
	newCtx = logging.WithResponseStatusHolder(newCtx)

	cancelCtx := newCtx
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-cancelCtx.Done():
			}
		}()
	}
	newCtx = context.WithValue(newCtx, "gin", c)
	newCtx = context.WithValue(newCtx, "handler", handler)
	return newCtx, func(params ...interface{}) {
		if c != nil {
			logging.SetResponseStatus(cancelCtx, c.Writer.Status())
		}
		if h.Cfg.RequestLog && len(params) == 1 {
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existing, exists := c.Get("API_RESPONSE"); exists {
					if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
						trimmedPayload := bytes.TrimSpace(payload)
						if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
							cancel()
							return
						}
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive emits blank lines every 5 seconds while waiting for a non-streaming response.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.Cfg)
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
	}

	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+1)
			combined = append(combined, existingBytes...)
			if existingBytes[len(existingBytes)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, data...)
			c.Set("API_RESPONSE", combined)
			return
		}
	}

	c.Set("API_RESPONSE", bytes.Clone(data))
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	// Multi-candidate combo fallback. If the requested model is a combo,
	// resolve the full chain and iterate until one entry succeeds or the
	// list is exhausted. For single-model requests this collapses to the
	// same single-call behaviour the code had before combos existed.
	for _, attempt := range h.resolveModelAttempts(modelName) {
		resp, headers, errMsg := h.executeSingle(ctx, handlerType, attempt.Model, rawJSON, alt)
		if errMsg == nil {
			return resp, headers, nil
		}
		if attempt.IsLast || !comboShouldFallback(errMsg, attempt.TriggerOn) {
			return nil, nil, errMsg
		}
		// Otherwise loop to the next candidate.
	}
	// resolveModelAttempts always returns at least one entry (the original
	// model), so we never reach here; keep the fall-through to satisfy the
	// type checker.
	return nil, nil, &interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      fmt.Errorf("combo fallback: no candidates produced for %q", modelName),
	}
}

// executeSingle issues one non-streaming call using the canonical path that
// existed before combos. It is split out so ExecuteWithAuthManager can loop
// over combo candidates without duplicating the whole body.
func (h *BaseAPIHandler) executeSingle(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
		Headers:         headersFromContext(ctx),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.Execute(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		status := http.StatusInternalServerError
		if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
			if code := se.StatusCode(); code > 0 {
				status = code
			}
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		return nil, nil, &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	if !PassthroughHeadersEnabled(h.Cfg) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
//
// Combo fallback iterates the resolved attempt list the same way
// ExecuteWithAuthManager does — without it, count-tokens requests against
// a combo would only ever hit the head entry and never recover when it
// goes dead.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	for _, attempt := range h.resolveModelAttempts(modelName) {
		resp, headers, errMsg := h.executeCountSingle(ctx, handlerType, attempt.Model, rawJSON, alt)
		if errMsg == nil {
			return resp, headers, nil
		}
		if attempt.IsLast || !comboShouldFallback(errMsg, attempt.TriggerOn) {
			return nil, nil, errMsg
		}
	}
	return nil, nil, &interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      fmt.Errorf("combo fallback (count): no candidates produced for %q", modelName),
	}
}

func (h *BaseAPIHandler) executeCountSingle(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
		Headers:         headersFromContext(ctx),
	}
	opts.Metadata = reqMeta
	resp, err := h.AuthManager.ExecuteCount(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		status := http.StatusInternalServerError
		if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
			if code := se.StatusCode(); code > 0 {
				status = code
			}
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		return nil, nil, &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
	}
	if !PassthroughHeadersEnabled(h.Cfg) {
		return resp.Payload, nil, nil
	}
	return resp.Payload, FilterUpstreamHeaders(resp.Headers), nil
}

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
//
// Combo fallback iterates the resolved attempt list. Fallback only fires
// while the upstream has produced no payload bytes — once any byte
// reaches the goroutine's sendData, the response is committed to that
// candidate and a later upstream failure surfaces as-is. Bootstrap
// recovery (auth rotation within the same candidate) still runs and is
// orthogonal to combo iteration.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	attempts := h.resolveModelAttempts(modelName)
	// Single-candidate path skips the buffered indirection so existing
	// non-combo behaviour stays bit-for-bit identical (header timing,
	// keep-alives, bootstrap retries).
	if len(attempts) <= 1 {
		target := modelName
		if len(attempts) == 1 {
			target = attempts[0].Model
		}
		return h.executeStreamSingle(ctx, handlerType, target, rawJSON, alt)
	}

	// Multi-candidate combo path. Try each entry; the first one that
	// successfully bootstraps wins. We attach a buffered channel pair to
	// the caller and pump each underlying single-attempt stream through
	// it. On bootstrap failure (status not yet sent → no bytes flushed),
	// we drop the candidate and try the next.
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	headers := make(http.Header)

	go func() {
		defer close(dataChan)
		defer close(errChan)

		var lastErr *interfaces.ErrorMessage
		for i, attempt := range attempts {
			if ctx != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
			subData, subHeaders, subErr := h.executeStreamSingle(ctx, handlerType, attempt.Model, rawJSON, alt)
			// Pump bytes/errors. forwardStreamAttempt returns true when
			// any payload was forwarded — at that point we are committed
			// and cannot fall back regardless of subsequent errors.
			committed, errMsg := forwardStreamAttempt(ctx, subData, subErr, dataChan, errChan, headers, subHeaders)
			if committed {
				return
			}
			if errMsg == nil {
				// Stream closed cleanly without forwarding any payload —
				// treat as a soft success from the upstream's POV. The
				// caller probably wants a 200 with empty body; the
				// underlying handler decides. There's nothing more to do.
				return
			}
			lastErr = errMsg
			isLast := attempt.IsLast || i == len(attempts)-1
			if isLast || !comboShouldFallback(errMsg, attempt.TriggerOn) {
				_ = sendStreamErr(ctx, errChan, errMsg)
				return
			}
			// Otherwise try the next combo entry.
		}
		if lastErr != nil {
			_ = sendStreamErr(ctx, errChan, lastErr)
		}
	}()

	return dataChan, headers, errChan
}

// forwardStreamAttempt drains a single-attempt stream and forwards
// payload chunks + headers to the caller-facing channels. It returns
// (committed, errMsg):
//   - committed=true when at least one payload chunk was forwarded; the
//     outer combo loop must not fall back because client bytes are
//     already on the wire.
//   - committed=false, errMsg!=nil when the stream errored before any
//     bytes flushed; combo loop may try the next entry.
//   - committed=false, errMsg=nil when the stream ended cleanly with no
//     payload (shouldn't normally happen but treated as terminal).
func forwardStreamAttempt(
	ctx context.Context,
	subData <-chan []byte,
	subErr <-chan *interfaces.ErrorMessage,
	dataChan chan<- []byte,
	errChan chan<- *interfaces.ErrorMessage,
	headers http.Header,
	subHeaders http.Header,
) (bool, *interfaces.ErrorMessage) {
	// Adopt the underlying stream's headers up-front so combo entries
	// that share a header set still surface the right Content-Type etc.
	// We replace rather than merge — last attempt's headers win, which
	// is the same contract the legacy single-candidate path had.
	replaceHeader(headers, subHeaders)

	committed := false
	for subData != nil || subErr != nil {
		select {
		case <-doneCh(ctx):
			return committed, nil
		case chunk, ok := <-subData:
			if !ok {
				subData = nil
				continue
			}
			committed = true
			if !sendStreamData(ctx, dataChan, chunk) {
				return committed, nil
			}
		case msg, ok := <-subErr:
			if !ok {
				subErr = nil
				continue
			}
			if msg != nil {
				return committed, msg
			}
		}
	}
	return committed, nil
}

func doneCh(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func sendStreamData(ctx context.Context, ch chan<- []byte, chunk []byte) bool {
	if ctx == nil {
		ch <- chunk
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case ch <- chunk:
		return true
	}
}

func sendStreamErr(ctx context.Context, ch chan<- *interfaces.ErrorMessage, msg *interfaces.ErrorMessage) bool {
	if msg == nil {
		return false
	}
	if ctx == nil {
		ch <- msg
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case ch <- msg:
		return true
	}
}

// executeStreamSingle is the original single-attempt streaming path
// extracted so combo fallback can iterate over candidates without
// duplicating bootstrap-retry, keep-alive, and header bookkeeping.
func (h *BaseAPIHandler) executeStreamSingle(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = modelName
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	opts := coreexecutor.Options{
		Stream:          true,
		Alt:             alt,
		OriginalRequest: rawJSON,
		SourceFormat:    sdktranslator.FromString(handlerType),
		Headers:         headersFromContext(ctx),
	}
	opts.Metadata = reqMeta
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		status := http.StatusInternalServerError
		if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
			if code := se.StatusCode(); code > 0 {
				status = code
			}
		}
		var addon http.Header
		if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
			if hdr := he.Headers(); hdr != nil {
				addon = hdr.Clone()
			}
		}
		errChan <- &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
		close(errChan)
		return nil, nil, errChan
	}
	passthroughHeadersEnabled := PassthroughHeadersEnabled(h.Cfg)
	// Capture upstream headers from the initial connection synchronously before the goroutine starts.
	// Keep a mutable map so bootstrap retries can replace it before first payload is sent.
	var upstreamHeaders http.Header
	if passthroughHeadersEnabled {
		upstreamHeaders = cloneHeader(FilterUpstreamHeaders(streamResult.Headers))
		if upstreamHeaders == nil {
			upstreamHeaders = make(http.Header)
		}
	}
	chunks := streamResult.Chunks
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		defer close(dataChan)
		defer close(errChan)
		sentPayload := false
		bootstrapRetries := 0
		maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)

		sendErr := func(msg *interfaces.ErrorMessage) bool {
			if ctx == nil {
				errChan <- msg
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk []byte) bool {
			if ctx == nil {
				dataChan <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case dataChan <- chunk:
				return true
			}
		}

		bootstrapEligible := func(err error) bool {
			status := statusFromError(err)
			if status == 0 {
				return true
			}
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
				http.StatusRequestTimeout, http.StatusTooManyRequests:
				return true
			default:
				return status >= http.StatusInternalServerError
			}
		}

	outer:
		for {
			for {
				var chunk coreexecutor.StreamChunk
				var ok bool
				if ctx != nil {
					select {
					case <-ctx.Done():
						return
					case chunk, ok = <-chunks:
					}
				} else {
					chunk, ok = <-chunks
				}
				if !ok {
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					// Safe bootstrap recovery: if the upstream fails before any payload bytes are sent,
					// retry a few times (to allow auth rotation / transient recovery) and then attempt model fallback.
					if !sentPayload {
						if bootstrapRetries < maxBootstrapRetries && bootstrapEligible(streamErr) {
							bootstrapRetries++
							retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
							if retryErr == nil {
								if passthroughHeadersEnabled {
									replaceHeader(upstreamHeaders, FilterUpstreamHeaders(retryResult.Headers))
								}
								chunks = retryResult.Chunks
								continue outer
							}
							streamErr = enrichAuthSelectionError(retryErr, providers, normalizedModel)
						}
					}

					status := http.StatusInternalServerError
					if se, ok := streamErr.(interface{ StatusCode() int }); ok && se != nil {
						if code := se.StatusCode(); code > 0 {
							status = code
						}
					}
					var addon http.Header
					if he, ok := streamErr.(interface{ Headers() http.Header }); ok && he != nil {
						if hdr := he.Headers(); hdr != nil {
							addon = hdr.Clone()
						}
					}
					_ = sendErr(&interfaces.ErrorMessage{StatusCode: status, Error: streamErr, Addon: addon})
					return
				}
				if len(chunk.Payload) > 0 {
					if handlerType == "openai-response" {
						if err := validateSSEDataJSON(chunk.Payload); err != nil {
							_ = sendErr(&interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
							return
						}
					}
					sentPayload = true
					if okSendData := sendData(cloneBytes(chunk.Payload)); !okSendData {
						return
					}
				}
			}
		}
	}()
	return dataChan, upstreamHeaders, errChan
}

func validateSSEDataJSON(chunk []byte) error {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if json.Valid(data) {
			continue
		}
		const max = 512
		preview := data
		if len(preview) > max {
			preview = preview[:max]
		}
		return fmt.Errorf("invalid SSE data JSON (len=%d): %q", len(data), preview)
	}
	return nil
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

// modelAttempt is one step in a combo fallback chain as seen by the
// Execute* methods. For non-combo requests the slice has exactly one entry
// with IsLast=true and empty TriggerOn.
type modelAttempt struct {
	Model     string
	TriggerOn []string
	IsLast    bool
}

// resolveModelAttempts returns the ordered list of models to try for a
// request. For a combo name it returns all candidates from the registry;
// for a plain prefixed model it returns a single-element slice.
//
// NOTE: combo names MAY contain "/" (e.g. "genfity/auto"). We therefore
// check the combo registry FIRST before falling back to the "slash =
// prefixed model" heuristic. This allows operators to name combos with
// a vendor prefix (e.g. "genfity/gpt-5.5") while still routing plain
// prefixed models (e.g. "kr/auto") directly.
func (h *BaseAPIHandler) resolveModelAttempts(modelName string) []modelAttempt {
	if h == nil || h.Combos == nil {
		return []modelAttempt{{Model: modelName, IsLast: true}}
	}
	base := strings.TrimSpace(modelName)

	// Check combo registry first — combo names may contain "/" so we
	// cannot use the slash as a shortcut to skip the registry lookup.
	candidates, ok := h.Combos.Candidates(base)
	if ok && len(candidates) > 0 {
		out := make([]modelAttempt, len(candidates))
		for i, c := range candidates {
			out[i] = modelAttempt{
				Model:     c.Model,
				TriggerOn: c.TriggerOn,
				IsLast:    c.IsLast,
			}
		}
		return out
	}

	// Not a combo — treat as a plain prefixed model (e.g. "kr/auto").
	return []modelAttempt{{Model: modelName, IsLast: true}}
}

// comboShouldFallback reports whether an error from one combo candidate
// should cause the loop to try the next entry. It mirrors the logic in
// internal/combo.ShouldFallback but operates on the handler-layer error
// type so the SDK package stays free of internal imports.
//
// The classifier is a *blacklist*: anything that is not a clear user
// payload error or a successful response should fall through to the
// next combo entry. Provider-side failures (401/402/403/404/410/429/5xx
// and 400 model_not_supported) all indicate "this candidate cannot
// serve the request right now", which is exactly what combos exist to
// route around. Returning the upstream error directly to the user when
// later entries could have served it is what made `genfity/gpt-5.5`
// surface 400/403 to the client even though a healthy entry existed
// further down the chain.
//
// User-side errors (clear "this request is malformed" signals) must NOT
// trigger fallback — retrying a different model with the same broken
// payload just costs latency and credentials.
func comboShouldFallback(errMsg *interfaces.ErrorMessage, triggers []string) bool {
	if errMsg == nil {
		return false
	}
	status := errMsg.StatusCode
	body := ""
	if errMsg.Error != nil {
		body = errMsg.Error.Error()
	}
	if !comboStatusEligibleForFallback(status, body) {
		return false
	}
	if len(triggers) == 0 {
		return true
	}
	lower := strings.ToLower(body)
	for _, t := range triggers {
		needle := strings.ToLower(strings.TrimSpace(t))
		if needle != "" && strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// comboStatusEligibleForFallback decides whether a given status + body
// combination is the "provider failed, try the next entry" case. The
// table is a deny-list anchored on shapes the gateway is confident are
// user errors; anything else falls through.
//
//	200/201/204               → success, do not fall through
//	400 + invalid_request_*   → user payload bug, do not fall through
//	400 + model_not_supported → provider says this auth/account cannot
//	                            serve this model → DO fall through
//	422                       → request shape error, do not fall through
//	other                     → fall through (provider-side failure)
//
// The 0 case (no status code, e.g. transport error before headers
// arrived) is treated as eligible: better to retry on the next entry
// than surface "internal server error" to the user.
func comboStatusEligibleForFallback(status int, body string) bool {
	switch {
	case status == 0:
		return true
	case status >= 200 && status < 300:
		return false
	case status == http.StatusBadRequest:
		// 400 model_not_supported = provider rejecting the model on this
		// credential. Combo's whole point is to retry on a different
		// credential/upstream — let the loop continue.
		if isModelSupportBodyMessage(body) {
			return true
		}
		// Other 400s are payload-shape errors. Forwarding to another
		// combo entry will just produce the same error.
		lower := strings.ToLower(body)
		if strings.Contains(lower, "invalid_request_error") ||
			strings.Contains(lower, "invalid_argument") ||
			strings.Contains(lower, "failed_precondition") {
			return false
		}
		// Unknown 400 shape — be conservative and try the next entry.
		// Wrong fallback wastes one retry; wrong "no fallback" hides a
		// healthy candidate from the user.
		return true
	case status == http.StatusUnprocessableEntity:
		// 422 is reserved for request shape errors in OpenAI / Anthropic
		// schemas. Trying another model won't fix the request.
		return false
	default:
		// 401/402/403/404/408/410/429/5xx and anything else — these are
		// provider-side failures (auth dead, quota exhausted, upstream
		// down). Combo's job is to route around them.
		return true
	}
}

// isModelSupportBodyMessage matches the same patterns the auth conductor
// uses to detect "this credential does not support this model" errors.
// Kept in sync with sdk/cliproxy/auth/conductor.go isModelSupportErrorMessage.
func isModelSupportBodyMessage(body string) bool {
	lower := strings.ToLower(strings.TrimSpace(body))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"model_not_allowed",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"requested model is not allowed",
		"model is not supported",
		"model not supported",
		"model is not allowed",
		"model not allowed",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (providers []string, normalizedModel string, err *interfaces.ErrorMessage) {
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
			if initialSuffix.HasSuffix {
				resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
			} else {
				resolvedModelName = resolvedBase
			}
		}
	} else {
		if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
			resolvedModelName = modelName
		} else {
			resolvedModelName = util.ResolveAutoModel(modelName)
		}
	}

	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)

	// Virtual combo expansion. If the requested model is a combo name (no
	// slash — combos cannot contain "/"), swap it for the combo's first
	// candidate here. Multi-candidate fallback in the execution loop is a
	// follow-up change; this already covers the common case where the head
	// entry succeeds, which is the whole point of combos in production.
	// Combo registry is checked before splitting on "/" because combo names may contain slashes.
	if h != nil && h.Combos != nil && h.Combos.Has(baseModel) {
		if head := strings.TrimSpace(h.Combos.FirstCandidate(baseModel)); head != "" {
			if parsed.HasSuffix {
				resolvedModelName = fmt.Sprintf("%s(%s)", head, parsed.RawSuffix)
			} else {
				resolvedModelName = head
			}
			parsed = thinking.ParseSuffix(resolvedModelName)
			baseModel = strings.TrimSpace(parsed.ModelName)
		}
	}

	if strings.EqualFold(baseModel, "gpt-image-2") {
		return nil, "", &interfaces.ErrorMessage{
			StatusCode: http.StatusServiceUnavailable,
			Error:      fmt.Errorf("model %s is only supported on /v1/images/generations and /v1/images/edits", baseModel),
		}
	}

	if h != nil && h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return []string{"home"}, resolvedModelName, nil
	}

	providers = util.GetProviderName(baseModel)
	// Fallback: if baseModel has no provider but differs from resolvedModelName,
	// try using the full model name. This handles edge cases where custom models
	// may be registered with their full suffixed name (e.g., "my-model(8192)").
	// Evaluated in Story 11.8: This fallback is intentionally preserved to support
	// custom model registrations that include thinking suffixes.
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}

	if len(providers) == 0 {
		// In strict prefix mode, an unprefixed request that does not match any
		// non-prefixed credential almost always means the caller forgot to
		// include the provider prefix (e.g. asked for "gpt-5.5" instead of
		// "cdx/gpt-5.5"). Surface a hint so operators can diagnose this quickly
		// without having to cross-reference /v1/models and the credential list.
		if !strings.Contains(baseModel, "/") {
			return nil, "", &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error: fmt.Errorf(
					"model %q is not registered. If this model is served by a prefixed credential, include the prefix explicitly (e.g. \"cc/%s\"). See GET /v1/models for the authoritative list",
					modelName,
					baseModel,
				),
			}
		}
		return nil, "", &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("unknown provider for model %s", modelName)}
	}

	// The thinking suffix is preserved in the model name itself, so no
	// metadata-based configuration passing is needed.
	return providers, resolvedModelName, nil
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func enrichAuthSelectionError(err error, providers []string, model string) error {
	if err == nil {
		return nil
	}

	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return err
	}

	code := strings.TrimSpace(authErr.Code)
	if code != "auth_not_found" && code != "auth_unavailable" {
		return err
	}

	providerText := strings.Join(providers, ",")
	if providerText == "" {
		providerText = "unknown"
	}
	modelText := strings.TrimSpace(model)
	if modelText == "" {
		modelText = "unknown"
	}

	baseMessage := strings.TrimSpace(authErr.Message)
	if baseMessage == "" {
		baseMessage = "no auth available"
	}
	detail := fmt.Sprintf("%s (providers=%s, model=%s)", baseMessage, providerText, modelText)

	// Clarify the most common alias confusion between Anthropic route names and internal provider keys.
	if strings.Contains(","+providerText+",", ",claude,") {
		detail += "; check Claude auth/key session and cooldown state via /v0/management/auth-files"
	}

	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}

	return &coreauth.Error{
		Code:       authErr.Code,
		Message:    detail,
		Retryable:  authErr.Retryable,
		HTTPStatus: status,
	}
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.Cfg) {
		for key, values := range msg.Addon {
			if len(values) == 0 {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}

	body := BuildErrorResponseBody(status, errText)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	var previous []byte
	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			previous = existingBytes
		}
	}
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := bytes.TrimSpace(body)
	if len(previous) > 0 {
		if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
			(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
			c.Set("API_RESPONSE", previous)
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if h.Cfg.RequestLog {
		if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
