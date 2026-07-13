package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// fakeComboMetricsRecorder captures Record calls for inspection.
type fakeComboMetricsRecorder struct {
	records []recordedComboAttempt
}

type recordedComboAttempt struct {
	comboName     string
	entryIndex    int
	success       bool
	latency       time.Duration
	triggerReason string
}

func (f *fakeComboMetricsRecorder) Record(comboName string, entryIndex int, success bool, latency time.Duration, triggerReason string) {
	f.records = append(f.records, recordedComboAttempt{
		comboName:     comboName,
		entryIndex:    entryIndex,
		success:       success,
		latency:       latency,
		triggerReason: triggerReason,
	})
}

// errExecutor always returns an error so combo fallback loops can be exercised.
type errExecutor struct{}

func (e *errExecutor) Identifier() string { return "codex" }

func (e *errExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "rate_limited", Message: "rate limited", HTTPStatus: http.StatusTooManyRequests}
}

func (e *errExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{Code: "rate_limited", Message: "rate limited", HTTPStatus: http.StatusTooManyRequests}
}

func (e *errExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *errExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *errExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// TestExecuteWithAuthManager_RecordsComboMetrics verifies that each combo
// attempt is recorded with the correct success/failure and trigger reason.
func TestExecuteWithAuthManager_RecordsComboMetrics(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&errExecutor{})

	auth := &coreauth.Auth{ID: "auth-err", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "fail"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	metrics := &fakeComboMetricsRecorder{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		ComboAttemptTimeoutSeconds: 1,
	}, manager)
	handler.Combos = &fakeComboResolver{chains: map[string][]ComboCandidate{
		"combo-metrics": {
			{Model: "codex/fail1"},
			{Model: "codex/fail2", IsLast: true},
		},
	}}
	handler.ComboMetrics = metrics

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "combo-metrics", []byte(`{"model":"combo-metrics","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg == nil {
		t.Fatal("expected error from failing combo")
	}

	if len(metrics.records) != 2 {
		t.Fatalf("expected 2 recorded attempts, got %d", len(metrics.records))
	}
	for i, r := range metrics.records {
		if r.comboName != "combo-metrics" {
			t.Errorf("record %d: comboName=%q, want combo-metrics", i, r.comboName)
		}
		if r.entryIndex != i {
			t.Errorf("record %d: entryIndex=%d, want %d", i, r.entryIndex, i)
		}
		if r.success {
			t.Errorf("record %d: expected failure, got success", i)
		}
		if r.triggerReason == "" {
			t.Errorf("record %d: expected non-empty triggerReason", i)
		}
	}
}

// TestExecuteWithAuthManager_SingleModelDoesNotRecordComboMetrics ensures
// plain (non-combo) requests do not pollute the combo metrics sink.
func TestExecuteWithAuthManager_SingleModelDoesNotRecordComboMetrics(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&errExecutor{})

	auth := &coreauth.Auth{ID: "auth-err", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "fail"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	metrics := &fakeComboMetricsRecorder{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler.ComboMetrics = metrics

	_, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "codex/fail", []byte(`{"model":"codex/fail","messages":[{"role":"user","content":"hi"}]}`), "")
	if errMsg == nil {
		t.Fatal("expected error from failing model")
	}

	if len(metrics.records) != 0 {
		t.Fatalf("expected no combo metrics for single-model request, got %d", len(metrics.records))
	}
}
