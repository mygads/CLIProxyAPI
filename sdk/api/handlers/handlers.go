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
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/combo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
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
// Internal provider details are sanitized before being included in the response.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		// Parse the upstream JSON and sanitize any internal details.
		sanitized := sanitizeUpstreamErrorJSON([]byte(trimmed), status)
		return sanitized
	}

	// Sanitize plain-text error messages.
	errText = sanitizeErrorText(errText, status)

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

func setReasoningEffortMetadata(meta map[string]any, handlerType, model string, rawJSON []byte) {
	if meta == nil {
		return
	}
	effort := thinking.ExtractReasoningEffort(rawJSON, handlerType, model)
	if effort == "" {
		return
	}
	meta[coreexecutor.ReasoningEffortMetadataKey] = effort
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
	// DisplayName returns the pipe-separated display name for a combo
	// (e.g. "GPT-5.5|OpenAI"). Empty string means no identity intercept.
	DisplayName(name string) string
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

// ComboMetricsRecorder is the minimal surface the handler needs to record
// per-attempt combo outcomes. Production resolvers implement this via
// combo.Registry; tests can supply a fake or leave it nil to disable recording.
type ComboMetricsRecorder interface {
	Record(comboName string, entryIndex int, success bool, latency time.Duration, triggerReason string)
}

// ImageRouteResolver exposes the global image-routing scheme to the request
// path. When a request carries an image AND targets a combo flagged here,
// the handler replaces the combo's normal fallback chain with ChainModels.
// A nil resolver (or Enabled()==false) disables the feature entirely.
type ImageRouteResolver interface {
	// Enabled reports whether image routing is turned on globally.
	Enabled() bool
	// IsRoutedCombo reports whether the given combo name is flagged for
	// image routing (implies Enabled).
	IsRoutedCombo(name string) bool
	// ChainModels returns the ordered image fallback chain (target first).
	// Entries may be plain prefixed models or combo names.
	ChainModels() []string
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

	// ComboMetrics is optional. When set, combo attempt outcomes are
	// recorded so operators can inspect success/failure/latency via the
	// management metrics endpoint.
	ComboMetrics ComboMetricsRecorder

	// comboCooldowns tracks failures that only become visible after the auth
	// executor has returned (for example a stream with no visible content).
	comboCooldowns *comboCandidateCooldownRegistry

	// ImageRouter is optional. When set and enabled, an image-carrying
	// request to a flagged combo is re-routed onto a dedicated fallback
	// chain instead of the combo's normal chain. Nil disables the feature.
	ImageRouter ImageRouteResolver
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
		Cfg:            cfg,
		AuthManager:    authManager,
		comboCooldowns: newDefaultComboCandidateCooldownRegistry(),
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
	newCtx = logging.WithResponseHeadersHolder(newCtx)

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
	// Identity intercept: if the model is a combo with a DisplayName and
	// the request is an identity question, short-circuit with a fabricated
	// response instead of hitting upstream.
	if resp, hdr := h.tryComboIdentityIntercept(handlerType, modelName, rawJSON); resp != nil {
		return resp, hdr, nil
	}

	// Language-guard injection: for Western-branded combos (claude/gpt/gemini
	// display name) that fall back across Chinese upstreams, append an
	// output-language directive so those upstreams don't leak Chinese into a
	// reply the customer expects to read as Claude/GPT/Gemini.
	rawJSON = h.injectComboLanguageGuard(handlerType, modelName, rawJSON)

	// Multi-candidate combo fallback. If the requested model is a combo,
	// resolve the full chain and iterate until one entry succeeds or the
	// list is exhausted. For single-model requests this collapses to the
	// same single-call behaviour the code had before combos existed.
	attempts := h.resolveModelAttempts(modelName)
	// Image override: an image-carrying request to a flagged combo runs the
	// dedicated image chain INSTEAD of the combo's normal chain (isolated).
	if imgAttempts, ok := h.maybeImageReroute(modelName, rawJSON); ok {
		attempts = imgAttempts
	}
	isCombo := len(attempts) > 1
	for i, attempt := range attempts {
		if isCombo && i < len(attempts)-1 && !h.comboCandidateAvailable(modelName, attempt.Model) {
			continue
		}
		start := time.Now()
		resp, headers, errMsg := h.executeSingleWithAttemptTimeout(ctx, handlerType, attempt, rawJSON, alt)
		if errMsg == nil {
			if !attempt.IsLast && shouldFallbackMalformedToolCallResponse(rawJSON, resp) {
				h.recordComboAttempt(modelName, i, attempt.Model, isCombo, false, start, "malformed_tool_response")
				continue
			}
			h.recordComboAttempt(modelName, i, attempt.Model, isCombo, true, start, "")
			return SanitizePublicResponse(resp, modelName), headers, nil
		}
		triggerReason := h.classifyFallbackReason(errMsg)
		h.recordComboAttempt(modelName, i, attempt.Model, isCombo, false, start, triggerReason)
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

// defaultComboAttemptTimeout caps how long a single NON-LAST combo candidate may run
// before it is abandoned and the loop falls through to the next entry. Some
// upstreams accept the connection but never respond (or stall mid-generation);
// combo fallback only fires on an *error*, so without a per-attempt deadline a
// hung head entry pins the whole request until the gateway's 120s client
// timeout — defeating the fallback and surfacing as provider_error/500 with
// ~125s latency. The LAST/only attempt is exempt: it keeps the full request
// budget so a genuinely long single-model generation is not cut short.
//
// Production code reads the configured value from SDKConfig via the
// comboAttemptTimeout method; this var is kept so existing tests can still
// shrink it by assigning directly. Thirty seconds bounds only bootstrap, not
// the full generation, and lets the chain reach later candidates before common
// 60s client idle deadlines. Override via
// combo-attempt-timeout-seconds in config.
var comboAttemptTimeout = 30 * time.Second

// comboStreamKeepaliveInterval keeps downstream SSE readers alive while a
// combo candidate is still bootstrapping or while fallback is in progress.
// Heartbeats never count as provider content and therefore never commit a
// candidate or prevent fallback.
var comboStreamKeepaliveInterval = 15 * time.Second

// defaultComboStreamIdleTimeout bounds how long an ALREADY-COMMITTED stream may go
// without receiving any byte from the upstream before it is abandoned. The
// bootstrap watchdog (comboAttemptTimeout) only guards time-to-first-byte;
// once the first visible chunk commits the stream it is disarmed. Without a
// post-commit guard, an upstream that emits a few tokens and then stalls
// mid-generation pins the request to the gateway's ~120s client timeout: the
// downstream client RTOs and sees a hang, yet the gateway later books a 200
// and bills it. The timer resets on every upstream byte, so a long but
// steadily-producing generation is never cut short — only a genuine stall is.
// It applies to every committed candidate (last or not), since the stall can
// happen on whichever leaf won the bootstrap.
//
// Production code reads the configured value from SDKConfig via the
// comboStreamIdleTimeout method; this var is kept so existing tests can still
// shrink it by assigning directly.
var comboStreamIdleTimeout = 60 * time.Second

// comboAttemptTimeout returns the configured per-attempt combo timeout.
// Falls back to the package-level default when no config is available.
func (h *BaseAPIHandler) comboAttemptTimeout() time.Duration {
	if h != nil && h.Cfg != nil {
		if d := h.Cfg.ComboAttemptTimeout(); d > 0 {
			return d
		}
	}
	return comboAttemptTimeout
}

// comboStreamIdleTimeout returns the configured post-commit idle timeout.
// Falls back to the package-level default when no config is available.
func (h *BaseAPIHandler) comboStreamIdleTimeout() time.Duration {
	if h != nil && h.Cfg != nil {
		if d := h.Cfg.ComboStreamIdleTimeout(); d > 0 {
			return d
		}
	}
	return comboStreamIdleTimeout
}

// executeSingleWithAttemptTimeout runs one combo candidate. For non-last
// candidates it bounds the call with comboAttemptTimeout so a hanging upstream
// triggers fallback instead of stalling the request; the resulting "context
// deadline exceeded" error is classified as a transport error by
// comboShouldFallback and lets the loop continue. The last attempt runs with
// the parent context unchanged.
func (h *BaseAPIHandler) executeSingleWithAttemptTimeout(ctx context.Context, handlerType string, attempt modelAttempt, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	if attempt.IsLast {
		return h.executeSingle(ctx, handlerType, attempt.Model, rawJSON, alt)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, h.comboAttemptTimeout())
	defer cancel()
	return h.executeSingle(attemptCtx, handlerType, attempt.Model, rawJSON, alt)
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
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
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
	normalizedPayload, normalizeErr := normalizeNonStreamingPayload(resp.Payload)
	if normalizeErr != nil {
		return nil, nil, normalizeErr
	}
	resp.Payload = normalizedPayload
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
	attempts := h.resolveModelAttempts(modelName)
	isCombo := len(attempts) > 1
	for i, attempt := range attempts {
		if isCombo && i < len(attempts)-1 && !h.comboCandidateAvailable(modelName, attempt.Model) {
			continue
		}
		start := time.Now()
		resp, headers, errMsg := h.executeCountSingle(ctx, handlerType, attempt.Model, rawJSON, alt)
		if errMsg == nil {
			h.recordComboAttempt(modelName, i, attempt.Model, isCombo, true, start, "")
			return resp, headers, nil
		}
		triggerReason := h.classifyFallbackReason(errMsg)
		h.recordComboAttempt(modelName, i, attempt.Model, isCombo, false, start, triggerReason)
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
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
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
	// Identity intercept (streaming): if the model is a combo with a
	// DisplayName and the request is an identity question, emit fabricated
	// SSE chunks instead of hitting upstream.
	if dataChan, hdr := h.tryComboIdentityInterceptStream(handlerType, modelName, rawJSON); dataChan != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		close(errChan)
		return dataChan, hdr, errChan
	}

	rawJSON = h.injectComboLanguageGuard(handlerType, modelName, rawJSON)

	attempts := h.resolveModelAttempts(modelName)
	// Image override: an image-carrying request to a flagged combo runs the
	// dedicated image chain INSTEAD of the combo's normal chain (isolated).
	if imgAttempts, ok := h.maybeImageReroute(modelName, rawJSON); ok {
		attempts = imgAttempts
	}
	// Single-candidate path skips the buffered indirection so existing
	// non-combo behaviour stays bit-for-bit identical (header timing,
	// keep-alives, bootstrap retries).
	if len(attempts) <= 1 {
		target := modelName
		if len(attempts) == 1 {
			target = attempts[0].Model
		}
		data, headers, errs := h.executeStreamSingle(ctx, handlerType, target, rawJSON, alt)
		return sanitizePublicStream(data, modelName), headers, errs
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
			if i < len(attempts)-1 && !h.comboCandidateAvailable(modelName, attempt.Model) {
				continue
			}
			start := time.Now()
			isLast := attempt.IsLast || i == len(attempts)-1
			// Bound the BOOTSTRAP (time-to-first-byte) of NON-LAST candidates:
			// a hung upstream that accepts the connection but never emits a
			// first byte would otherwise pin the stream until the gateway's
			// 120s client timeout, defeating fallback ("agent stops
			// mid-stream"). We use a cancellable context plus a watchdog that
			// cancels ONLY if no byte has been forwarded within
			// comboAttemptTimeout. Once the first byte commits the stream the
			// watchdog is disarmed, so a long generation is never truncated.
			//
			// bootstrapTimedOut / idleStalled record WHY attemptCtx was
			// cancelled so the code after forward can tell a watchdog abort
			// (must fall through + record failure) apart from a clean empty
			// close or a client disconnect (must stop silently). Cancellation
			// alone is ambiguous — forward returns (false, nil) in every case.
			var bootstrapTimedOut atomic.Bool
			var idleStalled atomic.Bool
			attemptCtx, cancel := context.WithCancel(ctx)
			committedCh := make(chan struct{})
			if !isLast {
				go func() {
					timer := time.NewTimer(h.comboAttemptTimeout())
					defer timer.Stop()
					select {
					case <-committedCh: // first byte forwarded — disarm
					case <-attemptCtx.Done(): // request ended
					case <-timer.C:
						bootstrapTimedOut.Store(true)
						cancel() // bootstrap stalled — abort, trigger fallback
					}
				}()
			}
			subData, subHeaders, subErr := h.executeStreamSingle(attemptCtx, handlerType, attempt.Model, rawJSON, alt)
			// onCommit fires once, when the first visible byte is forwarded.
			// onIdleStall aborts a stream that committed then stalled
			// mid-generation; it flags the reason before cancelling.
			onIdleStall := context.CancelFunc(func() {
				idleStalled.Store(true)
				cancel()
			})
			committed, errMsg := forwardStreamAttemptOnCommit(attemptCtx, subData, subErr, dataChan, errChan, headers, subHeaders, newPublicStreamSanitizer(modelName), func() { close(committedCh) }, onIdleStall, h.comboStreamIdleTimeout())
			if committed {
				// Bytes already reached the client, so we cannot fall back —
				// but a watchdog abort is still a degraded outcome and must not
				// be booked as a clean success. Two cases reach here:
				//   - idleStalled: committed then stalled mid-generation.
				//   - bootstrapTimedOut: the first byte landed in the same
				//     instant the bootstrap timer fired, so the select may have
				//     cancelled the attempt even though it technically committed
				//     (a nanosecond-wide race at exactly comboAttemptTimeout).
				switch {
				case idleStalled.Load():
					h.recordComboAttempt(modelName, i, attempt.Model, true, false, start, "idle_timeout")
				case bootstrapTimedOut.Load():
					h.recordComboAttempt(modelName, i, attempt.Model, true, false, start, "timeout")
				default:
					// Live stream owns attemptCtx for its full lifetime; it
					// shares the parent's cancellation. Do NOT cancel here.
					h.recordComboAttempt(modelName, i, attempt.Model, true, true, start, "")
				}
				return
			}
			cancel()
			if errMsg == nil {
				if bootstrapTimedOut.Load() {
					// Bootstrap watchdog aborted this candidate before any
					// byte was forwarded. Synthesize a timeout error so the
					// loop falls through to the next entry instead of
					// returning an empty 200 to the client.
					errMsg = &interfaces.ErrorMessage{
						StatusCode: http.StatusGatewayTimeout,
						Error:      fmt.Errorf("combo candidate %q timed out before first byte", attempt.Model),
					}
					h.recordComboAttempt(modelName, i, attempt.Model, true, false, start, "timeout")
					lastErr = errMsg
					// Watchdog only arms for non-last candidates, so there is
					// always a later entry to try.
					continue
				}
				if ctx != nil && ctx.Err() != nil {
					// Parent context ended (client disconnect) — stop silently
					// without booking a success.
					return
				}
				// A completion stream that closes without a single visible
				// payload is not a successful completion. Treat it as an
				// upstream failure so combo routing can continue instead of
				// returning HTTP 200 with an empty body to the gateway.
				errMsg = emptyUpstreamResponseError(attempt.Model)
				h.recordComboAttempt(modelName, i, attempt.Model, true, false, start, "empty_response")
				lastErr = errMsg
				if isLast {
					_ = sendStreamErr(ctx, errChan, errMsg)
					return
				}
				continue
			}
			triggerReason := h.classifyFallbackReason(errMsg)
			h.recordComboAttempt(modelName, i, attempt.Model, true, false, start, triggerReason)
			lastErr = errMsg
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

func emptyUpstreamResponseError(model string) *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      fmt.Errorf("upstream model %q returned an empty response", model),
	}
}

func sanitizePublicStream(data <-chan []byte, publicModel string) <-chan []byte {
	if data == nil {
		return data
	}
	out := make(chan []byte)
	sanitizer := newPublicStreamSanitizer(publicModel)
	go func() {
		defer close(out)
		for chunk := range data {
			safe := sanitizePublicResponseWithState(chunk, publicModel, sanitizer)
			if len(safe) == 0 || !publicChunkHasVisibleContent(safe) {
				continue
			}
			out <- safe
		}
	}()
	return out
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
	sanitizer *publicStreamSanitizer,
) (bool, *interfaces.ErrorMessage) {
	return forwardStreamAttemptOnCommit(ctx, subData, subErr, dataChan, errChan, headers, subHeaders, sanitizer, nil, nil, comboStreamIdleTimeout)
}

// forwardStreamAttemptOnCommit is forwardStreamAttempt with an optional
// onCommit callback invoked exactly once, the moment the first visible byte is
// forwarded downstream. The combo stream loop uses it to disarm the bootstrap
// watchdog so the per-attempt timeout never truncates an already-flowing
// stream.
func forwardStreamAttemptOnCommit(
	ctx context.Context,
	subData <-chan []byte,
	subErr <-chan *interfaces.ErrorMessage,
	dataChan chan<- []byte,
	errChan chan<- *interfaces.ErrorMessage,
	headers http.Header,
	subHeaders http.Header,
	sanitizer *publicStreamSanitizer,
	onCommit func(),
	onIdleStall context.CancelFunc,
	idleTimeout time.Duration,
) (bool, *interfaces.ErrorMessage) {
	// Adopt the underlying stream's headers up-front so combo entries
	// that share a header set still surface the right Content-Type etc.
	// We replace rather than merge — last attempt's headers win, which
	// is the same contract the legacy single-candidate path had.
	replaceHeader(headers, subHeaders)

	// Post-commit idle watchdog. The bootstrap watchdog only guards
	// time-to-first-byte; once committed it is disarmed. This timer guards
	// against an upstream that commits a few tokens then stalls forever,
	// which would otherwise pin the request to the gateway's ~120s client
	// timeout (client RTOs, gateway still books a 200). The timer is armed
	// on commit and reset on every upstream byte, so a steadily-producing
	// long generation is never cut — only a genuine stall trips it.
	var idle *time.Timer
	armIdle := func() {
		if onIdleStall == nil || idleTimeout <= 0 {
			return
		}
		idle = time.NewTimer(idleTimeout)
		go func() {
			select {
			case <-idle.C:
				onIdleStall() // stall — cancel the attempt ctx, ending the stream
			case <-ctx.Done():
			}
		}()
	}
	resetIdle := func() {
		if idle != nil {
			idle.Reset(idleTimeout)
		}
	}
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()

	var keepalive *time.Ticker
	var keepaliveCh <-chan time.Time
	if comboStreamKeepaliveInterval > 0 {
		keepalive = time.NewTicker(comboStreamKeepaliveInterval)
		keepaliveCh = keepalive.C
		defer keepalive.Stop()
	}

	committed := false
	var pendingChunks [][]byte
	var pendingPayload []byte
	for subData != nil || subErr != nil {
		select {
		case <-doneCh(ctx):
			return committed, nil
		case <-keepaliveCh:
			if !sendStreamData(ctx, dataChan, []byte(": keep-alive\n\n")) {
				return committed, nil
			}
		case chunk, ok := <-subData:
			if !ok {
				subData = nil
				continue
			}
			safe := sanitizePublicResponseWithState(chunk, sanitizer.publicModel, sanitizer)
			if len(safe) == 0 || !publicChunkHasVisibleContent(safe) {
				continue
			}
			if !committed {
				pendingChunks = append(pendingChunks, safe)
				trimmedSafe := bytes.TrimSpace(safe)
				if len(pendingPayload) > 0 && !bytes.HasSuffix(pendingPayload, []byte("\n")) &&
					(bytes.HasPrefix(trimmedSafe, []byte("data:")) || bytes.HasPrefix(trimmedSafe, []byte("event:"))) {
					// Executors usually deliver one complete SSE event per chunk,
					// but not all include the trailing blank line. Keep adjacent
					// complete events separable for progress inspection while still
					// allowing genuinely fragmented events to concatenate verbatim.
					pendingPayload = append(pendingPayload, '\n', '\n')
				}
				pendingPayload = append(pendingPayload, safe...)
				if !publicChunkHasCompletionProgress(pendingPayload) {
					continue
				}
				if onCommit != nil {
					onCommit()
					armIdle()
				}
				committed = true
				resetIdle()
				for _, pending := range pendingChunks {
					if !sendStreamData(ctx, dataChan, pending) {
						return committed, nil
					}
				}
				pendingChunks = nil
				pendingPayload = nil
				continue
			}
			resetIdle()
			if !sendStreamData(ctx, dataChan, safe) {
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
	setReasoningEffortMetadata(reqMeta, handlerType, normalizedModel, rawJSON)
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

	// Recursively flatten the combo into real leaf models. A combo entry
	// may itself be another combo (e.g. genfity/claude-opus-4.8 falls back
	// to genfity/claude-opus-4.7, which has its own chain). Without this
	// inlining the loop would hand the nested combo name to executeSingle,
	// which only resolves the nested head and reports "unknown provider"
	// when that head is a dead prefix — defeating the whole fallback.
	flat := h.flattenComboAttempts(strings.TrimSpace(modelName), make(map[string]struct{}))
	if len(flat) == 0 {
		// Not a combo — treat as a plain prefixed model (e.g. "kr/auto").
		return []modelAttempt{{Model: modelName, IsLast: true}}
	}

	// Dedup leaves by model so a diamond combo graph (A→B, A→C, both →D)
	// does not retry the same dead upstream twice, preserving first-seen
	// order. Only the final surviving leaf carries IsLast.
	out := make([]modelAttempt, 0, len(flat))
	seenModel := make(map[string]struct{}, len(flat))
	for _, a := range flat {
		key := strings.ToLower(strings.TrimSpace(a.Model))
		if key == "" {
			continue
		}
		if _, dup := seenModel[key]; dup {
			continue
		}
		seenModel[key] = struct{}{}
		a.IsLast = false
		out = append(out, a)
	}
	if len(out) == 0 {
		return []modelAttempt{{Model: modelName, IsLast: true}}
	}
	out[len(out)-1].IsLast = true
	return out
}

// maybeImageReroute returns a replacement attempt chain when the request
// should be re-routed to the global image-routing scheme, and false when it
// should follow its normal combo chain. The rewrite is TOTAL (isolated): an
// image request to a flagged combo runs ONLY the image chain — it never falls
// back to the combo's own candidates. Guards, cheapest first:
//   - image router not wired / disabled
//   - modelName is not a flagged combo (cheap set lookup, no body scan)
//   - request carries no image (only scanned once the combo is flagged)
func (h *BaseAPIHandler) maybeImageReroute(modelName string, rawJSON []byte) ([]modelAttempt, bool) {
	if h == nil || h.ImageRouter == nil || !h.ImageRouter.Enabled() {
		return nil, false
	}
	if !h.ImageRouter.IsRoutedCombo(modelName) {
		return nil, false
	}
	if !requestHasImage(rawJSON) {
		return nil, false
	}
	attempts := h.resolveImageAttempts()
	if len(attempts) == 0 {
		return nil, false
	}
	return attempts, true
}

// resolveImageAttempts expands the global image chain into leaf attempts,
// reusing flattenComboAttempts so a chain entry that is itself a combo is
// inlined exactly like a normal combo entry. Dedups leaves and stamps the
// final one IsLast — mirrors resolveModelAttempts.
func (h *BaseAPIHandler) resolveImageAttempts() []modelAttempt {
	if h == nil || h.ImageRouter == nil {
		return nil
	}
	models := h.ImageRouter.ChainModels()
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAttempt, 0, len(models))
	seenModel := make(map[string]struct{}, len(models))
	add := func(a modelAttempt) {
		key := strings.ToLower(strings.TrimSpace(a.Model))
		if key == "" {
			return
		}
		if _, dup := seenModel[key]; dup {
			return
		}
		seenModel[key] = struct{}{}
		a.IsLast = false
		out = append(out, a)
	}
	for _, m := range models {
		// A chain entry may itself be a combo — inline its leaves. If it is
		// not a combo (or unexpandable), treat it as a plain leaf model.
		if child := h.flattenComboAttempts(strings.TrimSpace(m), make(map[string]struct{})); len(child) > 0 {
			for _, a := range child {
				add(a)
			}
			continue
		}
		add(modelAttempt{Model: m})
	}
	if len(out) == 0 {
		return nil
	}
	out[len(out)-1].IsLast = true
	return out
}

// requestHasImage reports whether the raw request body carries an image part.
// It covers the two client formats coding agents use — OpenAI chat/responses
// (content parts of type "image_url" or "input_image") and Anthropic messages
// (content parts of type "image"). Gemini inline_data is intentionally out of
// scope for now. The scan is a bounded gjson walk over messages[].content[];
// it never allocates the whole payload and returns on the first hit.
func requestHasImage(rawJSON []byte) bool {
	if len(rawJSON) == 0 {
		return false
	}
	found := false
	scanContent := func(content gjson.Result) {
		if found || !content.IsArray() {
			return
		}
		content.ForEach(func(_, part gjson.Result) bool {
			switch strings.ToLower(part.Get("type").String()) {
			case "image_url", "image", "input_image":
				found = true
				return false
			}
			return true
		})
	}
	// OpenAI chat + Anthropic messages: top-level messages[].content[].
	gjson.GetBytes(rawJSON, "messages").ForEach(func(_, msg gjson.Result) bool {
		scanContent(msg.Get("content"))
		return !found
	})
	if found {
		return true
	}
	// OpenAI Responses format: top-level input[].content[].
	gjson.GetBytes(rawJSON, "input").ForEach(func(_, item gjson.Result) bool {
		scanContent(item.Get("content"))
		return !found
	})
	return found
}

// flattenComboAttempts expands a combo name into the ordered list of leaf
// attempts, recursively inlining any candidate that is itself a combo. The
// `seen` set guards the current recursion path against cyclic combo
// references (A→B→A) — entries are removed on the way back out so sibling
// branches can still reuse a combo. IsLast is left unset on every returned
// element; resolveModelAttempts stamps the final one. Returns nil when
// modelName is not a combo (or resolves to no candidates).
func (h *BaseAPIHandler) flattenComboAttempts(modelName string, seen map[string]struct{}) []modelAttempt {
	key := strings.ToLower(strings.TrimSpace(modelName))
	if key == "" {
		return nil
	}
	if _, cycle := seen[key]; cycle {
		return nil
	}
	candidates, ok := h.Combos.Candidates(modelName)
	if !ok || len(candidates) == 0 {
		return nil
	}
	seen[key] = struct{}{}
	defer delete(seen, key)

	out := make([]modelAttempt, 0, len(candidates))
	for _, c := range candidates {
		if child := h.flattenComboAttempts(c.Model, seen); len(child) > 0 {
			// c.Model is itself a combo — inline its leaves in place.
			out = append(out, child...)
			continue
		}
		// child is empty: either c.Model is a real leaf model, or it is a
		// known combo we could not expand (cycle / disabled / empty). Drop
		// the unexpandable combo reference — emitting the combo NAME as a
		// literal model would just produce a dead "unknown provider"
		// attempt. Keep genuine leaves so the existing skip + fallback
		// still applies to dead prefixes.
		if h.Combos.Has(c.Model) {
			continue
		}
		out = append(out, modelAttempt{Model: c.Model, TriggerOn: c.TriggerOn})
	}
	return out
}

// injectComboLanguageGuard appends an output-language directive to the system
// prompt when modelName is a Western-branded combo (claude/gpt/gemini display
// name). This steers the combo's Chinese fallback upstreams (Qwen/GLM/Kimi/
// MiniMax/MiMo/DeepSeek) away from emitting Chinese into a reply the customer
// expects to read as the published brand. No-op for non-combo models, combos
// with no display name, and Chinese-branded combos. Idempotent.
func (h *BaseAPIHandler) injectComboLanguageGuard(handlerType, modelName string, rawJSON []byte) []byte {
	if h == nil || h.Combos == nil {
		return rawJSON
	}
	displayName := h.Combos.DisplayName(modelName)
	if displayName == "" {
		return rawJSON
	}
	return combo.InjectLanguageGuard(handlerType, rawJSON, displayName)
}

// tryComboIdentityIntercept checks whether the request is an identity
// question targeting a combo with a DisplayName. If so, it returns a
// fabricated non-streaming response. Returns (nil, nil) to let the
// request flow normally.
func (h *BaseAPIHandler) tryComboIdentityIntercept(handlerType, modelName string, rawJSON []byte) ([]byte, http.Header) {
	if h == nil || h.Combos == nil {
		return nil, nil
	}
	displayName := h.Combos.DisplayName(modelName)
	if displayName == "" {
		return nil, nil
	}
	// Translate to OpenAI format for identity detection (source may be
	// claude/gemini/etc). Identity check needs messages in OpenAI shape.
	from := sdktranslator.FromString(handlerType)
	to := sdktranslator.FromString("openai")
	openaiBody := sdktranslator.TranslateRequest(from, to, modelName, rawJSON, false)

	if !combo.IsIdentityQuestion(openaiBody) {
		return nil, nil
	}

	answer := combo.BuildIdentityAnswer(displayName, openaiBody)
	if answer == "" {
		return nil, nil
	}

	fakeResp := combo.BuildIdentityResponse(answer, modelName)

	// Translate back to the caller's format.
	var param any
	out := sdktranslator.TranslateNonStream(context.Background(), to, from, modelName, rawJSON, openaiBody, fakeResp, &param)

	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("X-Genfity-Identity-Rewrite", "1")
	return out, hdr
}

// tryComboIdentityInterceptStream is the streaming variant of
// tryComboIdentityIntercept. Returns a channel of SSE chunks when
// intercepted, or (nil, nil) to let the request flow normally.
func (h *BaseAPIHandler) tryComboIdentityInterceptStream(handlerType, modelName string, rawJSON []byte) (<-chan []byte, http.Header) {
	if h == nil || h.Combos == nil {
		return nil, nil
	}
	displayName := h.Combos.DisplayName(modelName)
	if displayName == "" {
		return nil, nil
	}
	from := sdktranslator.FromString(handlerType)
	to := sdktranslator.FromString("openai")
	openaiBody := sdktranslator.TranslateRequest(from, to, modelName, rawJSON, true)

	if !combo.IsIdentityQuestion(openaiBody) {
		return nil, nil
	}

	answer := combo.BuildIdentityAnswer(displayName, openaiBody)
	if answer == "" {
		return nil, nil
	}

	chunks := combo.BuildIdentityStreamChunks(answer, modelName)

	// Translate each chunk to the caller's format.
	var param any
	dataChan := make(chan []byte, len(chunks)*2)
	for _, raw := range chunks {
		lines := sdktranslator.TranslateStream(context.Background(), to, from, modelName, rawJSON, openaiBody, raw, &param)
		for _, ln := range lines {
			dataChan <- ln
		}
	}
	close(dataChan)

	hdr := http.Header{}
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("X-Genfity-Identity-Rewrite", "1")
	return dataChan, hdr
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
	if isTransportError(body) {
		return true
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
		if isProviderBusyBodyMessage(body) {
			return true
		}
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

func isProviderBusyBodyMessage(body string) bool {
	lower := strings.ToLower(strings.TrimSpace(body))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"rate_limit",
		"too many requests",
		"high traffic",
		"cooldown",
		"quota exceeded",
		"insufficient_quota",
		"temporarily unavailable",
		"service unavailable",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isTransportError(body string) bool {
	lower := strings.ToLower(body)
	patterns := [...]string{
		"context deadline exceeded",
		"connection reset",
		"connection refused",
		"broken pipe",
		"eof",
		"timeout",
		"no such host",
		"tls handshake",
		"network is unreachable",
		"i/o timeout",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// recordComboAttempt records one combo candidate outcome when a metrics
// recorder is wired. It is a no-op for non-combo (single-attempt) requests
// so plain model requests do not pollute the combo metrics sink.
func (h *BaseAPIHandler) recordComboAttempt(comboName string, entryIndex int, candidateModel string, isCombo bool, success bool, start time.Time, triggerReason string) {
	if !isCombo || h == nil {
		return
	}
	h.recordComboCandidateHealth(comboName, candidateModel, success, triggerReason)
	if h.ComboMetrics == nil {
		return
	}
	h.ComboMetrics.Record(comboName, entryIndex, success, time.Since(start), triggerReason)
}

// classifyFallbackReason returns a short trigger reason for a failed combo
// attempt. It is best-effort and used only for metrics tagging.
func (h *BaseAPIHandler) classifyFallbackReason(errMsg *interfaces.ErrorMessage) string {
	if errMsg == nil {
		return ""
	}
	body := ""
	if errMsg.Error != nil {
		body = errMsg.Error.Error()
	}
	// The per-attempt watchdog (comboAttemptTimeout) cancels a hung candidate
	// via context deadline. Surface that as its own reason — timeouts are the
	// primary signal operators watch, and folding them into transport_error
	// would hide how often combo candidates stall.
	lowerBody := strings.ToLower(body)
	if strings.Contains(lowerBody, "empty_stream") || strings.Contains(lowerBody, "empty response") {
		return "empty_response"
	}
	if strings.Contains(lowerBody, "context deadline exceeded") || strings.Contains(lowerBody, "context canceled") {
		return "timeout"
	}
	if isTransportError(body) {
		return "transport_error"
	}
	switch errMsg.StatusCode {
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return "auth_error"
	case http.StatusBadRequest:
		if isModelSupportBodyMessage(body) {
			return "model_not_supported"
		}
		if isProviderBusyBodyMessage(body) {
			return "provider_busy"
		}
		return "bad_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "upstream_unavailable"
	}
	if errMsg.StatusCode >= 500 {
		return "server_error"
	}
	return "provider_error"
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
		"model_not_found",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"requested model is not allowed",
		"model is not supported",
		"model not supported",
		"model not found",
		"model is not allowed",
		"model not allowed",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
		// Public-catalog rejections — the upstream removed the model from its
		// served catalog (e.g. kvc "Model ... is not available in current
		// public model catalog"). Same shape: this auth/upstream cannot serve
		// the model right now, so the combo loop should fall through.
		"not available in current public model catalog",
		"is not available in current public model catalog",
		"is not in the public model catalog",
		"is not available in this account's model catalog",
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

	// Return a safe customer-facing message without exposing internal
	// provider names, model routing details, or management endpoints.
	safeMessage := "No available credentials for the requested model. Please retry shortly."

	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}

	return &coreauth.Error{
		Code:       authErr.Code,
		Message:    safeMessage,
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
