package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type emptyResponseFallbackExecutor struct{}

func (e *emptyResponseFallbackExecutor) Identifier() string { return "codex" }

func (e *emptyResponseFallbackExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	if strings.Contains(req.Model, "semantic-empty") {
		return coreexecutor.Response{Payload: []byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"hidden"},"finish_reason":"length","index":0}]}`)}, nil
	}
	if strings.Contains(req.Model, "empty") {
		return coreexecutor.Response{}, nil
	}
	return coreexecutor.Response{Payload: []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}]}`)}, nil
}

func (e *emptyResponseFallbackExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 2)
	if strings.Contains(req.Model, "role-then-content") {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}]}`)}
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":null,"index":0}]}`)}
	} else if strings.Contains(req.Model, "role-only") {
		// A provider may emit its opening role frame and then close or stall.
		// This is protocol metadata, not usable completion output, so it must
		// not commit the combo to this candidate.
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}]}`)}
	} else if strings.Contains(req.Model, "empty") {
		// A whitespace payload passes the auth manager's raw bootstrap gate,
		// then is removed by the public stream sanitizer. This reproduces the
		// production HTTP-200-with-empty-body failure mode.
		chunks <- coreexecutor.StreamChunk{Payload: []byte("   \n")}
	} else {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"choices":[{"delta":{"content":"ok"},"index":0}]}`)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *emptyResponseFallbackExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *emptyResponseFallbackExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *emptyResponseFallbackExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented"}
}

func newEmptyResponseFallbackHandler(t *testing.T, chain []ComboCandidate) (*BaseAPIHandler, *fakeComboMetricsRecorder) {
	t.Helper()
	executor := &emptyResponseFallbackExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-empty-response-fallback", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	models := make([]*registry.ModelInfo, 0, len(chain))
	for _, candidate := range chain {
		models = append(models, &registry.ModelInfo{ID: candidate.Model})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	metrics := &fakeComboMetricsRecorder{}
	handler := NewBaseAPIHandlers(nil, manager)
	handler.Combos = &fakeComboResolver{chains: map[string][]ComboCandidate{"combo-empty": chain}}
	handler.ComboMetrics = metrics
	return handler, metrics
}

func TestExecuteStreamWithAuthManager_EmptyResponseFallsThrough(t *testing.T) {
	handler, metrics := newEmptyResponseFallbackHandler(t, []ComboCandidate{
		{Model: "emptyprov/empty"},
		{Model: "fastprov/fast", IsLast: true},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "combo-empty", []byte(`{"model":"combo-empty","messages":[{"role":"user","content":"hi"}],"stream":true}`), "")
	var got []byte
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("expected healthy fallback, got error: %+v", errMsg)
		}
	}
	if !strings.Contains(string(got), "ok") {
		t.Fatalf("expected fallback payload, got %q", got)
	}
	if len(metrics.records) != 2 || metrics.records[0].success || metrics.records[0].triggerReason != "empty_response" || !metrics.records[1].success {
		t.Fatalf("unexpected metrics: %+v", metrics.records)
	}
}

func TestExecuteStreamWithAuthManager_LastEmptyResponseReturnsBadGateway(t *testing.T) {
	handler, _ := newEmptyResponseFallbackHandler(t, []ComboCandidate{
		{Model: "emptyprov/empty-one"},
		{Model: "emptyprov/empty-two", IsLast: true},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "combo-empty", []byte(`{"model":"combo-empty","messages":[{"role":"user","content":"hi"}],"stream":true}`), "")
	for chunk := range dataChan {
		if len(chunk) > 0 {
			t.Fatalf("expected no payload, got %q", chunk)
		}
	}
	var gotErr *interfaces.ErrorMessage
	for errMsg := range errChan {
		if errMsg != nil {
			gotErr = errMsg
		}
	}
	if gotErr == nil || gotErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 empty-response error, got %+v", gotErr)
	}
}

func TestExecuteWithAuthManager_EmptyResponseFallsThrough(t *testing.T) {
	handler, metrics := newEmptyResponseFallbackHandler(t, []ComboCandidate{
		{Model: "emptyprov/empty"},
		{Model: "fastprov/fast", IsLast: true},
	})

	resp, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "combo-empty", []byte(`{"model":"combo-empty","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg != nil {
		t.Fatalf("expected healthy fallback, got error: %+v", errMsg)
	}
	if !strings.Contains(string(resp), "ok") {
		t.Fatalf("expected fallback payload, got %q", resp)
	}
	if len(metrics.records) != 2 || metrics.records[0].success || !metrics.records[1].success {
		t.Fatalf("unexpected metrics: %+v", metrics.records)
	}
}

func TestExecuteWithAuthManager_SemanticallyEmptyResponseFallsThrough(t *testing.T) {
	handler, metrics := newEmptyResponseFallbackHandler(t, []ComboCandidate{
		{Model: "emptyprov/semantic-empty"},
		{Model: "fastprov/fast", IsLast: true},
	})

	resp, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "combo-empty", []byte(`{"model":"combo-empty","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg != nil || !strings.Contains(string(resp), "ok") {
		t.Fatalf("expected healthy fallback, resp=%q err=%+v", resp, errMsg)
	}
	if len(metrics.records) != 2 || metrics.records[0].success || metrics.records[0].triggerReason != "empty_response" || !metrics.records[1].success {
		t.Fatalf("unexpected metrics: %+v", metrics.records)
	}
}

func TestExecuteStreamWithAuthManager_TwoContentlessCandidatesFallThroughToThird(t *testing.T) {
	handler, metrics := newEmptyResponseFallbackHandler(t, []ComboCandidate{
		{Model: "metaprov/role-only"},
		{Model: "emptyprov/empty"},
		{Model: "fastprov/fast", IsLast: true},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "combo-empty", []byte(`{"model":"combo-empty","messages":[{"role":"user","content":"hi"}],"stream":true}`), "")
	var got []byte
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("expected third candidate to succeed, got error: %+v", errMsg)
		}
	}
	if !strings.Contains(string(got), "ok") {
		t.Fatalf("expected third-candidate payload, got %q", got)
	}
	if strings.Contains(string(got), `"role":"assistant"`) {
		t.Fatalf("contentless prelude from failed candidate leaked downstream: %q", got)
	}
	if len(metrics.records) != 3 || metrics.records[0].success || metrics.records[1].success || !metrics.records[2].success {
		t.Fatalf("unexpected metrics: %+v", metrics.records)
	}
	if metrics.records[0].triggerReason != "empty_response" || metrics.records[1].triggerReason != "empty_response" {
		t.Fatalf("expected both contentless candidates to be classified empty_response: %+v", metrics.records)
	}
}

func TestExecuteStreamWithAuthManager_PreludeIsReleasedWithRealContent(t *testing.T) {
	handler, metrics := newEmptyResponseFallbackHandler(t, []ComboCandidate{
		{Model: "metaprov/role-then-content"},
		{Model: "fastprov/fast", IsLast: true},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "combo-empty", []byte(`{"model":"combo-empty","messages":[{"role":"user","content":"hi"}],"stream":true}`), "")
	var got []byte
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("expected first candidate to succeed, got error: %+v", errMsg)
		}
	}
	if !strings.Contains(string(got), `"role":"assistant"`) || !strings.Contains(string(got), `"content":"ok"`) {
		t.Fatalf("expected buffered prelude and content, got %q", got)
	}
	if len(metrics.records) != 1 || !metrics.records[0].success {
		t.Fatalf("unexpected metrics: %+v", metrics.records)
	}
}
