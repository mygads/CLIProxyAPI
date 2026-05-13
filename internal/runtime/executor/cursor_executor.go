package executor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CursorExecutor is a skeleton for the Cursor IDE (`cu/`) provider.
// The full Cursor implementation requires connect-go / protobuf to
// speak agent.v1.AgentService/Run (~2000 LOC of schema + streaming),
// which is tracked for a follow-up PRD v2 phase.
//
// The skeleton exists so:
//   1. Token storage + registration works — operators can paste a
//      Cursor access token via the management UI right now.
//   2. The `cu/` prefix resolves in scheduler without crashing; users
//      get a clear 501 explaining what's missing rather than a 5xx.
//   3. Dropping in the real executor is a contained change — no other
//      wiring needed.
type CursorExecutor struct {
	cfg *config.Config
}

// NewCursorExecutor builds the skeleton. config is carried for future
// use but is not consumed today.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg}
}

// Identifier is the provider key; matches auth.Provider written by the
// (future) management handler.
func (e *CursorExecutor) Identifier() string { return "cursor" }

// PrepareRequest is a no-op — there is no HTTP request path yet.
func (e *CursorExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	return nil
}

// HttpRequest returns a clean 501 so debug probes get a structured
// failure instead of an opaque crash.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "cursor: executor not implemented; see docs/PRD-V2-COMPLETION-ROADMAP.md §Phase 2A",
	}
}

// Execute is a 501 stub so callers get a clear error instead of a
// panic. When the real executor lands it will slot into this method.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "cursor: chat completions not yet implemented (needs connect-protobuf executor); see docs/PRD-V2-COMPLETION-ROADMAP.md §Phase 2A",
	}
}

// ExecuteStream is a 501 stub — same reason as Execute.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "cursor: streaming not yet implemented (needs connect-protobuf executor); see docs/PRD-V2-COMPLETION-ROADMAP.md §Phase 2A",
	}
}

// Refresh is a no-op — Cursor access tokens are long-lived.
func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("cursor: refresh called with nil auth")
	}
	return auth, nil
}

// CountTokens is not supported by Cursor; callers get a clean 501.
func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &cliproxyauth.Error{
		HTTPStatus: http.StatusNotImplemented,
		Message:    "cursor: CountTokens not supported",
	}
}
