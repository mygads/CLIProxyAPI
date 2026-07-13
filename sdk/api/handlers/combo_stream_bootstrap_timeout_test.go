package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// hangBeforeFirstByteStreamExecutor never emits a byte for the "hang" model
// (it blocks until ctx cancels), and completes immediately for the "fast"
// model. It proves that a NON-LAST combo candidate which hangs before its
// first byte is abandoned by the bootstrap watchdog and the loop falls through
// to the next healthy candidate — instead of returning an empty 200.
type hangBeforeFirstByteStreamExecutor struct{}

func (e *hangBeforeFirstByteStreamExecutor) Identifier() string { return "codex" }

func (e *hangBeforeFirstByteStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *hangBeforeFirstByteStreamExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk)
	if req.Model == "fastprov/fast" {
		go func() {
			ch <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"content":"ok"},"index":0}]}`)}
			close(ch)
		}()
		return &coreexecutor.StreamResult{Chunks: ch}, nil
	}
	// hang model: never emit a byte; block until the bootstrap watchdog
	// cancels ctx, then close.
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *hangBeforeFirstByteStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *hangBeforeFirstByteStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *hangBeforeFirstByteStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

// TestExecuteStreamWithAuthManager_BootstrapTimeoutFallsThrough proves that a
// non-last combo candidate that hangs before its first byte is abandoned after
// the bootstrap timeout and the loop serves the next (fast) candidate — the
// client must receive the fast candidate's bytes, not an empty 200.
func TestExecuteStreamWithAuthManager_BootstrapTimeoutFallsThrough(t *testing.T) {
	executor := &hangBeforeFirstByteStreamExecutor{}
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

	metrics := &fakeComboMetricsRecorder{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		ComboAttemptTimeoutSeconds: 1, // shrink bootstrap timeout for the test
	}, manager)
	handler.Combos = &fakeComboResolver{chains: map[string][]ComboCandidate{
		"combo-bootstrap": {
			{Model: "hangprov/hang"},
			{Model: "fastprov/fast", IsLast: true},
		},
	}}
	handler.ComboMetrics = metrics

	start := time.Now()
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "combo-bootstrap", []byte(`{"model":"combo-bootstrap","messages":[{"role":"user","content":"hi"}],"stream":true}`), "")
	if dataChan == nil || errChan == nil {
		t.Fatalf("expected non-nil channels")
	}

	var got []byte
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	var gotErr *interfaces.ErrorMessage
	for msg := range errChan {
		if msg != nil {
			gotErr = msg
		}
	}
	elapsed := time.Since(start)

	// Must fall through to the fast candidate: client gets its bytes and no
	// error is surfaced.
	if len(got) == 0 {
		t.Fatalf("expected fast candidate bytes after bootstrap timeout, got empty (elapsed=%v, err=%+v)", elapsed, gotErr)
	}
	if gotErr != nil {
		t.Fatalf("expected no error after successful fallback, got %+v", gotErr)
	}
	// Should fall through shortly after the 1s bootstrap timeout, well below
	// any real client deadline.
	if elapsed > 5*time.Second {
		t.Fatalf("bootstrap timeout not bounding the hang: took %v", elapsed)
	}

	// The hang candidate (entry 0) must be recorded as a timeout failure, and
	// the fast candidate (entry 1) as a success.
	var sawTimeout, sawSuccess bool
	for _, r := range metrics.records {
		if r.entryIndex == 0 && !r.success && r.triggerReason == "timeout" {
			sawTimeout = true
		}
		if r.entryIndex == 1 && r.success {
			sawSuccess = true
		}
	}
	if !sawTimeout {
		t.Errorf("expected entry 0 recorded as timeout failure; records=%+v", metrics.records)
	}
	if !sawSuccess {
		t.Errorf("expected entry 1 recorded as success; records=%+v", metrics.records)
	}
}
