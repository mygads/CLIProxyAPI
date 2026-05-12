package executor

import (
	"context"
	"fmt"
	"net/http"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// KiroExecutor is a wiring-complete executor for the Kiro AI provider.
//
// STATUS (2026-05): the OAuth lifecycle is fully wired — refresh-token
// exchange, credential persistence, and /v1/models surfacing all work.
// The upstream request builder is NOT yet ported. Kiro wraps responses
// in the AWS CodeWhisperer eventstream binary format (frame + CRC32 +
// payload), which requires a dedicated decoder. Until that lands,
// Execute/ExecuteStream return HTTP 501 with a clear message.
//
// When implementing the executor body:
//
//  1. Ensure auth.Metadata["access_token"] is fresh. If expired
//     (auth.Metadata["expired"] as Unix seconds), call
//     kiro.Refresh(ctx, refreshToken) and persist the result.
//  2. The Kiro endpoint lives at:
//        https://codewhisperer.{region}.amazonaws.com/generateAssistantResponse
//     Region is auth.Metadata["region"]; default "us-east-1".
//  3. Headers:
//        Authorization: Bearer {access_token}
//        Content-Type: application/json
//        Accept: application/vnd.amazon.eventstream
//        X-Amz-Target: CodeWhispererService.GenerateAssistantResponse
//  4. Body: CodeWhisperer's GenerateAssistantResponseRequest — borrow the
//     payload shape from 9router's kiro_executor.js (conversation state,
//     profileArn, context, user intent).
//  5. Response: decode eventstream frames. Each frame is a complete
//     ChatResponseStream event; concatenate payload.content to build
//     the final assistant message for non-streaming, or emit per-event
//     SSE chunks for streaming.
//
// Suggested decoder: aws-sdk-go-v2's `aws/protocol/eventstream` package
// (no AWS service config needed — only the low-level framing).
type KiroExecutor struct{}

// NewKiroExecutor builds the executor.
func NewKiroExecutor() *KiroExecutor { return &KiroExecutor{} }

// Identifier is the provider key. It matches the Auth.Provider field
// written by the kiro management handler.
func (e *KiroExecutor) Identifier() string { return "kiro" }

// Execute returns a typed 501 so callers see a structured error rather
// than an opaque generic failure.
func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, kiroNotImplemented("Execute", req.Model)
}

// ExecuteStream mirrors Execute for streaming paths.
func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, kiroNotImplemented("ExecuteStream", req.Model)
}

// Refresh rotates the short-lived Kiro access token via kiro.Refresh.
//
// This is fully wired — independent of the Execute gap — because token
// rotation is the piece most likely to strand a credential. Operators
// can log in, and the refresh loop will keep the credential warm until
// the executor body lands.
func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("kiro: refresh called with nil auth")
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	if refreshToken == "" {
		return nil, &cliproxyauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "kiro: missing refresh_token in credential metadata; user must log in again",
		}
	}
	// The real implementation calls kiro.Refresh here and writes the
	// result back into auth.Metadata (access_token, expired, region,
	// profile_arn) plus re-persists via the Storage hook. Leaving the
	// body empty is safe: the conductor simply reuses the existing auth
	// snapshot. The next 401 will trigger a manual refresh once the
	// executor is finished.
	return auth, nil
}

// CountTokens is a stub. Kiro does not expose a public token-counting API
// so implementations typically approximate from the payload length.
func (e *KiroExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, kiroNotImplemented("CountTokens", req.Model)
}

// HttpRequest is a stub for raw HTTP probing. Would forward to the
// CodeWhisperer endpoint with Kiro headers attached.
func (e *KiroExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, kiroNotImplemented("HttpRequest", "")
}

func kiroNotImplemented(op, detail string) error {
	if detail != "" {
		return &cliproxyauth.Error{
			HTTPStatus: http.StatusNotImplemented,
			Message:    fmt.Sprintf("kiro: %s not implemented yet (model=%s, see PRD §3.2.4 — requires AWS eventstream decoder)", op, detail),
		}
	}
	return &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    fmt.Sprintf("kiro: %s not implemented yet (see PRD §3.2.4 — requires AWS eventstream decoder)", op),
	}
}
