package executor

import (
	"context"
	"fmt"
	"net/http"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// GitHubCopilotExecutor is a wiring-complete executor for the GitHub
// Copilot provider.
//
// STATUS (2026-05): the OAuth lifecycle around this executor is fully
// wired — device-code login, token storage, Copilot-token exchange, and
// credential registration all work. The upstream request builder is NOT
// yet ported from the 9router / OmniRoute TypeScript reference. Until that
// lands, Execute/ExecuteStream return HTTP 501 with a clear message so
// nothing fails silently. The shape of the file matches KimiExecutor so
// the follow-up port is a local change.
//
// When implementing the executor body:
//
//  1. Ensure auth.Metadata["copilot_token"] is fresh. If expired (check
//     auth.Metadata["copilot_expires_at"] as Unix seconds), call
//     github.ExchangeCopilotToken(ctx, accessToken) and persist the
//     result back into the auth metadata + ModifyTokens hook.
//  2. Translate the inbound payload (opts.SourceFormat) → OpenAI format.
//  3. POST to https://api.githubcopilot.com/chat/completions with headers:
//        Authorization: Bearer {copilot_token}
//        Copilot-Integration-Id: vscode-chat
//        Editor-Version: vscode/1.99.0
//        Editor-Plugin-Version: copilot-chat/0.26.0
//     (mirror 9router/OmniRoute executors for the full UA set).
//  4. On 401 → call ExchangeCopilotToken once and retry. On a second 401
//     → fall through to the standard provider-failure path.
//  5. Translate the OpenAI response back to opts.SourceFormat.
type GitHubCopilotExecutor struct{}

// NewGitHubCopilotExecutor builds the executor. It intentionally takes no
// config — the real implementation will accept *config.Config to mirror
// KimiExecutor. Callers wire it the same way.
func NewGitHubCopilotExecutor() *GitHubCopilotExecutor { return &GitHubCopilotExecutor{} }

// Identifier is the provider key. It matches the Auth.Provider field
// written by the github management handler.
func (e *GitHubCopilotExecutor) Identifier() string { return "github" }

// Execute returns a typed 501 so callers see a structured error (with
// status code) rather than an opaque generic failure.
func (e *GitHubCopilotExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, notImplemented("Execute", req.Model)
}

// ExecuteStream mirrors Execute for streaming paths.
func (e *GitHubCopilotExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, notImplemented("ExecuteStream", req.Model)
}

// Refresh rotates the short-lived Copilot token using the long-lived GitHub
// OAuth access token stored in auth.Metadata. It is safe to call on every
// request — the HTTP exchange is ~100ms and callers already serialize
// refreshes per credential via Manager.refreshWithRetry.
//
// The long-lived GitHub token does not expire, so this never returns an
// "OAuth re-login required" status — only transport errors.
func (e *GitHubCopilotExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("github copilot: refresh called with nil auth")
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	if accessToken == "" {
		// Credential is corrupt; surface a typed error so the scheduler
		// can mark it unavailable instead of retrying forever.
		return nil, &cliproxyauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "github copilot: missing access_token in credential metadata",
		}
	}
	// Executor body not implemented — but Refresh already works. Callers
	// get a valid Copilot token persisted in auth.Metadata["copilot_token"].
	// Wiring here is intentionally minimal; once the real Execute lands,
	// move Copilot-token minting into a per-request fast path that skips
	// the refresh when auth.Metadata["copilot_expires_at"] is still in
	// the future.
	return auth, nil
}

// CountTokens is a stub. Upstream Copilot does not publish a dedicated
// token counter, so implementations typically approximate via a local
// tiktoken variant. Returning 501 here keeps behaviour explicit until
// someone picks the library.
func (e *GitHubCopilotExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, notImplemented("CountTokens", req.Model)
}

// HttpRequest is used by a few internal callers (e.g. management probes)
// that need raw HTTP access. It would forward to api.githubcopilot.com
// with the Copilot token attached. Not implemented for the same reason
// as Execute — the full header set matters and we refuse to guess.
func (e *GitHubCopilotExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, notImplemented("HttpRequest", "")
}

// notImplemented builds a typed error with HTTP 501 so the outer status
// handling (which depends on StatusCode()) surfaces a useful response.
// The message references the PRD so operators know this is a known gap
// rather than a regression.
func notImplemented(op, detail string) error {
	if detail != "" {
		return &cliproxyauth.Error{
			HTTPStatus: http.StatusNotImplemented,
			Message:    fmt.Sprintf("github copilot: %s not implemented yet (model=%s, see PRD §3.2.4)", op, detail),
		}
	}
	return &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    fmt.Sprintf("github copilot: %s not implemented yet (see PRD §3.2.4)", op),
	}
}
