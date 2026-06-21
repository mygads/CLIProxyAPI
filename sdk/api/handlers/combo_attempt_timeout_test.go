package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// modelRoutedExecutor hangs (until ctx cancels) for the "hang" model and
// returns a fast payload for the "fast" model. It lets the combo per-attempt
// timeout test prove that a stalled head entry is abandoned and the loop falls
// through to the next candidate.
type modelRoutedExecutor struct {
	mu        sync.Mutex
	hangCalls int
	fastCalls int
}

func (e *modelRoutedExecutor) Identifier() string { return "codex" }

func (e *modelRoutedExecutor) Execute(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	if req.Model == "fastprov/fast" {
		e.mu.Lock()
		e.fastCalls++
		e.mu.Unlock()
		return coreexecutor.Response{Payload: []byte(`{"id":"x","object":"chat.completion","model":"fastprov/fast","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)}, nil
	}
	// hang model: block until the per-attempt timeout cancels ctx.
	e.mu.Lock()
	e.hangCalls++
	e.mu.Unlock()
	<-ctx.Done()
	return coreexecutor.Response{}, &coreauth.Error{Code: "context_canceled", Message: ctx.Err().Error()}
}

func (e *modelRoutedExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *modelRoutedExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelRoutedExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *modelRoutedExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

// TestExecuteWithAuthManager_PerAttemptTimeoutFallsThrough proves that a combo
// whose head entry hangs forever is abandoned after comboAttemptTimeout and the
// loop succeeds on the next (fast) candidate, instead of pinning the request
// until the gateway client timeout.
func TestExecuteWithAuthManager_PerAttemptTimeoutFallsThrough(t *testing.T) {
	prev := comboAttemptTimeout
	comboAttemptTimeout = 200 * time.Millisecond
	t.Cleanup(func() { comboAttemptTimeout = prev })

	executor := &modelRoutedExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	authHang := &coreauth.Auth{ID: "auth-hang", Provider: "codex", Status: coreauth.StatusActive}
	authFast := &coreauth.Auth{ID: "auth-fast", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), authHang); err != nil {
		t.Fatalf("register hang: %v", err)
	}
	if _, err := manager.Register(context.Background(), authFast); err != nil {
		t.Fatalf("register fast: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authHang.ID, authHang.Provider, []*registry.ModelInfo{{ID: "hangprov/hang"}})
	registry.GetGlobalRegistry().RegisterClient(authFast.ID, authFast.Provider, []*registry.ModelInfo{{ID: "fastprov/fast"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authHang.ID)
		registry.GetGlobalRegistry().UnregisterClient(authFast.ID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler.Combos = &fakeComboResolver{chains: map[string][]ComboCandidate{
		"combo-timeout": {
			{Model: "hangprov/hang"},
			{Model: "fastprov/fast", IsLast: true},
		},
	}}

	start := time.Now()
	resp, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "combo-timeout", []byte(`{"model":"combo-timeout","messages":[{"role":"user","content":"hi"}]}`), "")
	elapsed := time.Since(start)

	if errMsg != nil {
		t.Fatalf("expected fallback success, got error: %+v", errMsg)
	}
	if string(resp) == "" {
		t.Fatal("expected non-empty response from fast fallback")
	}
	if executor.hangCalls != 1 {
		t.Errorf("expected hang model tried exactly once, got %d", executor.hangCalls)
	}
	if executor.fastCalls != 1 {
		t.Errorf("expected fast model served once, got %d", executor.fastCalls)
	}
	// Should fall through shortly after the (tiny) per-attempt timeout, far
	// below any real client deadline.
	if elapsed > 2*time.Second {
		t.Errorf("fallback took too long (%v) — per-attempt timeout not bounding the hang", elapsed)
	}
}
