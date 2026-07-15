package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestKiroPayloadCompatibilityValidToolHistory(t *testing.T) {
	raw := []byte(`{
      "model":"genfity/test",
      "tools":[{"type":"function","function":{"name":"search_docs","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}],
      "messages":[
        {"role":"assistant","tool_calls":[{"id":"call_1","function":{"name":"search_docs","arguments":"{\"q\":\"kiro\"}"}}]},
        {"role":"tool","tool_call_id":"call_1","content":"ok"},
        {"role":"user","content":"continue"}
      ]}`)
	if issue := kiroPayloadCompatibilityIssue(raw); issue != nil {
		t.Fatalf("unexpected issue: %#v", issue)
	}
}

func TestExecuteWithAuthManagerSkipsIncompatibleKiroAndUsesNextProvider(t *testing.T) {
	executor := &modelRoutedExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	authFast := &coreauth.Auth{ID: "auth-kiro-preflight-fast", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), authFast); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authFast.ID, authFast.Provider, []*registry.ModelInfo{{ID: "fastprov/fast"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authFast.ID) })

	tools := make([]map[string]any, 0, 86)
	for i := 0; i < 86; i++ {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": fmt.Sprintf("tool_%d", i)}})
	}
	raw, _ := json.Marshal(map[string]any{
		"model":    "combo-kiro-preflight",
		"tools":    tools,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	recorder := &fakeComboMetricsRecorder{}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h.ComboMetrics = recorder
	h.Combos = &fakeComboResolver{chains: map[string][]ComboCandidate{
		"combo-kiro-preflight": {
			{Model: "server3/kr/auto-thinking-agentic"},
			{Model: "fastprov/fast", IsLast: true},
		},
	}}
	resp, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "openai", "combo-kiro-preflight", raw, "")
	if errMsg != nil || len(resp) == 0 || executor.fastCalls != 1 || executor.hangCalls != 0 {
		t.Fatalf("resp=%s err=%#v fast=%d unexpected=%d", resp, errMsg, executor.fastCalls, executor.hangCalls)
	}
	if len(recorder.records) < 2 || recorder.records[0].triggerReason != "incompatible_payload" || !recorder.records[1].success {
		t.Fatalf("metrics=%#v", recorder.records)
	}
}

func TestKiroPayloadCompatibilityKnownProductionLimits(t *testing.T) {
	tools := make([]map[string]any, 0, 86)
	for i := 0; i < 86; i++ {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       fmt.Sprintf("tool_%d", i),
				"parameters": map[string]any{"type": "object"},
			},
		})
	}
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "tools": tools})
	issue := kiroPayloadCompatibilityIssue(raw)
	if issue == nil || issue.Reason != "too_many_tools" || issue.ToolCount != 86 || len(issue.ToolNamesSample) != 8 {
		t.Fatalf("issue=%#v", issue)
	}

	large := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxKiroPayloadBytes) + `"}]}`)
	issue = kiroPayloadCompatibilityIssue(large)
	if issue == nil || issue.Reason != "request_too_large" {
		t.Fatalf("large issue=%#v", issue)
	}
}

func TestKiroPayloadCompatibilityRejectsHistoricalSchemaMismatch(t *testing.T) {
	raw := []byte(`{
      "tools":[{"name":"Glob","input_schema":{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}}],
      "messages":[
        {"role":"assistant","content":[{"type":"tool_use","id":"toolu_bad","name":"Glob","input":{}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bad","is_error":true,"content":"pattern is required"}]}
      ]}`)
	issue := kiroPayloadCompatibilityIssue(raw)
	if issue == nil || issue.Reason != "tool_arguments_schema_mismatch" || issue.ToolName != "Glob" || issue.Detail != "arguments.pattern is required" {
		t.Fatalf("issue=%+v", issue)
	}
}

func TestKiroPayloadCompatibilityRejectsToolShapesKiroCannotRepresent(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		reason string
	}{
		{
			name:   "invalid Kiro name",
			raw:    `{"tools":[{"type":"function","function":{"name":"web search"}}],"messages":[{"role":"user","content":"hi"}]}`,
			reason: "unsupported_tool_name",
		},
		{
			name:   "duplicate call ID",
			raw:    `{"tools":[{"type":"function","function":{"name":"search"}}],"messages":[{"role":"assistant","tool_calls":[{"id":"same","function":{"name":"search","arguments":"{}"}},{"id":"same","function":{"name":"search","arguments":"{}"}}]}]}`,
			reason: "duplicate_tool_call_id",
		},
		{
			name:   "undeclared tool",
			raw:    `{"tools":[{"type":"function","function":{"name":"search"}}],"messages":[{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"shell","arguments":"{}"}}]}]}`,
			reason: "undeclared_tool_call",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue := kiroPayloadCompatibilityIssue([]byte(tc.raw))
			if issue == nil || issue.Reason != tc.reason {
				t.Fatalf("issue=%#v want=%s", issue, tc.reason)
			}
		})
	}
}

func TestRecordIncompatiblePayloadSkipDoesNotCooldownCandidate(t *testing.T) {
	recorder := &fakeComboMetricsRecorder{}
	h := &BaseAPIHandler{ComboMetrics: recorder}
	const comboName = "genfity/test"
	const candidate = "server3/kr/auto-thinking-agentic"
	h.recordIncompatiblePayloadSkip(nil, comboName, 0, candidate, true, &kiroCompatibilityIssue{Reason: "too_many_tools", ToolCount: 86})
	if !h.comboCandidateAvailable(comboName, candidate) {
		t.Fatal("incompatible payload incorrectly cooled a healthy candidate")
	}
	if len(recorder.records) != 1 || recorder.records[0].triggerReason != "incompatible_payload" {
		t.Fatalf("metrics=%#v", recorder.records)
	}
}
