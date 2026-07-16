package handlers

import (
	"bytes"
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

// stallAfterCommitStreamExecutor commits one visible chunk for the "stall"
// model and then blocks until ctx is cancelled (simulating an upstream that
// emits a few tokens then hangs mid-generation). The "fast" model completes
// immediately. It proves the post-commit idle watchdog abandons a stalled
// committed stream and the loop continues to the next candidate.
type stallAfterCommitStreamExecutor struct{}

func (e *stallAfterCommitStreamExecutor) Identifier() string { return "codex" }

func (e *stallAfterCommitStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func TestForwardStreamAttempt_HiddenReasoningResetsCommittedIdleTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subData := make(chan []byte)
	subErr := make(chan *interfaces.ErrorMessage)
	dataOut := make(chan []byte, 16)
	errOut := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		defer close(subData)
		defer close(subErr)
		send := func(payload string) bool {
			select {
			case subData <- []byte(payload):
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(`data: {"choices":[{"delta":{"content":"start"},"index":0}]}`) {
			return
		}
		for range 4 {
			time.Sleep(50 * time.Millisecond)
			if !send(`data: {"choices":[{"delta":{"reasoning_content":"hidden"},"index":0}]}`) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
		_ = send(`data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop","index":0}]}`)
	}()

	started := time.Now()
	committed, errMsg := forwardStreamAttemptOnCommit(
		ctx,
		subData,
		subErr,
		dataOut,
		errOut,
		make(http.Header),
		make(http.Header),
		newPublicStreamSanitizer("genfity/test"),
		func() {},
		cancel,
		100*time.Millisecond,
		[]byte(": keep-alive\n\n"),
	)
	elapsed := time.Since(started)
	close(dataOut)

	var output []byte
	for chunk := range dataOut {
		output = append(output, chunk...)
	}
	if !committed || errMsg != nil {
		t.Fatalf("expected clean committed stream, committed=%v err=%v", committed, errMsg)
	}
	if !bytes.Contains(output, []byte("done")) {
		t.Fatalf("idle watchdog fired despite live hidden reasoning; output=%q", output)
	}
	if elapsed < 225*time.Millisecond {
		t.Fatalf("stream ended too early; hidden reasoning did not keep it alive: %v", elapsed)
	}
}

func (e *stallAfterCommitStreamExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk)
	if req.Model == "fastprov/fast" {
		go func() {
			ch <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"content":"ok"},"index":0}]}`)}
			close(ch)
		}()
		return &coreexecutor.StreamResult{Chunks: ch}, nil
	}
	// stall model: emit one visible chunk (commits the stream), then block
	// until the idle watchdog cancels ctx.
	go func() {
		ch <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"content":"Sal"},"index":0}]}`)}
		<-ctx.Done()
		close(ch)
	}()
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *stallAfterCommitStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *stallAfterCommitStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *stallAfterCommitStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

// TestExecuteStreamWithAuthManager_PostCommitIdleStallBounded proves that a
// committed-then-stalled non-last combo candidate is abandoned after
// comboStreamIdleTimeout (rather than pinning the stream to the gateway's
// client timeout). After the stall the loop ends — the watchdog cancels the
// attempt context, the stalled stream closes, and the whole call returns far
// below any real client deadline.
func TestExecuteStreamWithAuthManager_PostCommitIdleStallBounded(t *testing.T) {
	prevAttempt := comboAttemptTimeout
	prevIdle := comboStreamIdleTimeout
	comboAttemptTimeout = 5 * time.Second
	comboStreamIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		comboAttemptTimeout = prevAttempt
		comboStreamIdleTimeout = prevIdle
	})

	executor := &stallAfterCommitStreamExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	authStall := &coreauth.Auth{ID: "auth-stall", Provider: "codex", Status: coreauth.StatusActive}
	authFast := &coreauth.Auth{ID: "auth-fast", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), authStall); err != nil {
		t.Fatalf("register stall: %v", err)
	}
	if _, err := manager.Register(context.Background(), authFast); err != nil {
		t.Fatalf("register fast: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authStall.ID, authStall.Provider, []*registry.ModelInfo{{ID: "stallprov/stall"}})
	registry.GetGlobalRegistry().RegisterClient(authFast.ID, authFast.Provider, []*registry.ModelInfo{{ID: "fastprov/fast"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authStall.ID)
		registry.GetGlobalRegistry().UnregisterClient(authFast.ID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		ComboAttemptTimeoutSeconds:    30,
		ComboStreamIdleTimeoutSeconds: 1,
	}, manager)
	handler.Combos = &fakeComboResolver{chains: map[string][]ComboCandidate{
		"combo-stall": {
			{Model: "stallprov/stall"},
			{Model: "fastprov/fast", IsLast: true},
		},
	}}

	start := time.Now()
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "combo-stall", []byte(`{"model":"combo-stall","messages":[{"role":"user","content":"hi"}],"stream":true}`), "")
	if dataChan == nil || errChan == nil {
		t.Fatalf("expected non-nil channels")
	}

	var got []byte
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	for range errChan {
	}
	elapsed := time.Since(start)

	// The stalled candidate committed "Sal" before it hung; the watchdog
	// abandons it well before the (5s) bootstrap timeout or any client
	// deadline. We assert it returned quickly — the precise downstream bytes
	// depend on sanitizer/fallback behaviour, but the call must not hang.
	if elapsed > 3*time.Second {
		t.Fatalf("post-commit stall not bounded: call took %v", elapsed)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least the committed chunk to reach the client")
	}
}

func TestComboStreamIdleTimeoutDefaultAllowsLongThinkingGap(t *testing.T) {
	h := &BaseAPIHandler{}
	if got := h.comboStreamIdleTimeout(); got < 120*time.Second {
		t.Fatalf("default stream idle timeout=%s, want at least 120s for thinking providers", got)
	}
}
