package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

// fakeComboResolver is a tiny stand-in for the production registry that
// lets the fallback tests feed a canned combo chain into BaseAPIHandler
// without importing internal/combo (which would cycle).
type fakeComboResolver struct {
	chains       map[string][]ComboCandidate
	displayNames map[string]string
}

func (f *fakeComboResolver) Has(name string) bool {
	_, ok := f.chains[name]
	return ok
}

func (f *fakeComboResolver) FirstCandidate(name string) string {
	entries, ok := f.chains[name]
	if !ok || len(entries) == 0 {
		return ""
	}
	return entries[0].Model
}

func (f *fakeComboResolver) Candidates(name string) ([]ComboCandidate, bool) {
	entries, ok := f.chains[name]
	if !ok {
		return nil, false
	}
	out := make([]ComboCandidate, len(entries))
	copy(out, entries)
	return out, true
}

func (f *fakeComboResolver) DisplayName(name string) string {
	if f.displayNames == nil {
		return ""
	}
	return f.displayNames[name]
}

func TestResolveModelAttempts_nonComboReturnsSingleEntry(t *testing.T) {
	h := &BaseAPIHandler{Combos: &fakeComboResolver{chains: map[string][]ComboCandidate{
		"genfity-2.1": {
			{Model: "cc/claude-opus-4-7"},
			{Model: "cx/gpt-5.5", IsLast: true},
		},
	}}}
	got := h.resolveModelAttempts("cc/claude-opus-4-7")
	if len(got) != 1 {
		t.Fatalf("expected 1 attempt for non-combo, got %d", len(got))
	}
	if got[0].Model != "cc/claude-opus-4-7" || !got[0].IsLast {
		t.Fatalf("unexpected attempt %+v", got[0])
	}
}

func TestResolveModelAttempts_comboExpandsFullChain(t *testing.T) {
	h := &BaseAPIHandler{Combos: &fakeComboResolver{chains: map[string][]ComboCandidate{
		"genfity-2.1": {
			{Model: "cc/claude-opus-4-7", TriggerOn: []string{"quota"}},
			{Model: "cx/gpt-5.5", IsLast: true},
		},
	}}}
	got := h.resolveModelAttempts("genfity-2.1")
	if len(got) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(got))
	}
	if got[0].Model != "cc/claude-opus-4-7" || got[0].IsLast {
		t.Errorf("attempt 0: %+v", got[0])
	}
	if got[1].Model != "cx/gpt-5.5" || !got[1].IsLast {
		t.Errorf("attempt 1: %+v", got[1])
	}
	if len(got[0].TriggerOn) != 1 || got[0].TriggerOn[0] != "quota" {
		t.Errorf("trigger_on lost: %#v", got[0].TriggerOn)
	}
}

func TestResolveModelAttempts_nilResolverIsSingleAttempt(t *testing.T) {
	h := &BaseAPIHandler{}
	got := h.resolveModelAttempts("anything")
	if len(got) != 1 || got[0].Model != "anything" {
		t.Fatalf("expected single attempt with original model, got %#v", got)
	}
}

func TestComboShouldFallback_retriableStatusOnly(t *testing.T) {
	// PRD-blacklist: combo fallback fires for everything that is *not*
	// a clear user-shape error. The legacy whitelist that only fired on
	// 429/5xx hid healthy candidates whenever the head entry returned
	// 401/402/403/404/410 etc.
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{0, "transport blew up", true},  // unknown — be permissive
		{200, "ok", false},               // success
		{204, "", false},                 // success no-content
		{400, `{"error":{"type":"invalid_request_error"}}`, false}, // user payload bug
		{400, "invalid_argument: bad shape", false},
		{400, `{"error":{"code":"model_not_supported"}}`, true}, // provider rejecting model
		{400, `{"error":{"code":"model_not_allowed"}}`, true},
		{400, "The requested model is not supported.", true},
		{400, "The requested model is not allowed.", true},
		{400, "weird 400 shape with no signal", true}, // unknown 400 — try next
		{401, "unauthorized", true},
		{402, "payment required", true},
		{403, "insufficient_quota", true},
		{404, "not found", true},
		{408, "request timeout", true},
		{410, "gone", true},
		{422, "request shape error", false}, // user error
		{429, "rate limited", true},
		{500, "internal", true},
		{502, "bad gateway", true},
		{503, "service unavailable", true},
		{504, "gateway timeout", true},
	}
	for _, tc := range cases {
		errMsg := &interfaces.ErrorMessage{StatusCode: tc.status, Error: errors.New(tc.body)}
		got := comboShouldFallback(errMsg, nil)
		if got != tc.want {
			t.Errorf("status=%d body=%q: want %v got %v", tc.status, tc.body, tc.want, got)
		}
	}
}

func TestComboShouldFallback_triggerKeywordMatch(t *testing.T) {
	errWith := func(body string) *interfaces.ErrorMessage {
		return &interfaces.ErrorMessage{StatusCode: 429, Error: errors.New(body)}
	}

	if comboShouldFallback(errWith("hello"), []string{"quota_exceeded"}) {
		t.Error("trigger did not match body; should NOT fall through")
	}
	if !comboShouldFallback(errWith("you have exceeded your quota"), []string{"quota"}) {
		t.Error("trigger matched body; should fall through")
	}
	if !comboShouldFallback(errWith("ERROR: QUOTA hit"), []string{"quota"}) {
		t.Error("case-insensitive match missed")
	}
	if comboShouldFallback(errWith("quota issue"), []string{"", "  "}) {
		t.Error("empty triggers should be skipped, leaving no match")
	}
}

// Confirms the model_not_supported path: even though 400 is normally a
// user payload error, when the body matches a "model not supported"
// pattern the request CAN be served by another combo entry and the
// loop must continue. This is the exact scenario that broke
// genfity/gpt-5.5 — first entry's credential is dead, returning 400
// model_not_supported, but a healthy entry exists later in the chain.
func TestComboShouldFallback_modelNotSupportedFallsThrough(t *testing.T) {
	bodies := []string{
		`{"error":{"code":"model_not_supported","message":"The requested model is not supported."}}`,
		`{"error":{"code":"model_not_allowed","message":"The requested model is not allowed."}}`,
		"The requested model is not supported.",
		"The requested model is not allowed.",
		"unsupported model: gpt-5.5",
		"model unavailable",
		"This model is not available for your account",
	}
	for _, body := range bodies {
		msg := &interfaces.ErrorMessage{StatusCode: 400, Error: errors.New(body)}
		if !comboShouldFallback(msg, nil) {
			t.Errorf("expected fallback for 400 %q; got false", body)
		}
	}
}

func TestComboShouldFallback_nilErrorMessage(t *testing.T) {
	if comboShouldFallback(nil, []string{"anything"}) {
		t.Error("nil error message must not trigger fallback")
	}
}

// Ensures the interface is still satisfiable so external callers that
// depend on ComboResolver can compile against the expanded shape.
func TestComboResolver_interfaceSatisfied(t *testing.T) {
	var _ ComboResolver = &fakeComboResolver{}
}

// Compile-time wiring smoke test: make sure the Execute methods are still
// reachable on an empty handler. We don't call them (no AuthManager wired)
// but the fact that this file builds catches refactor regressions.
var (
	_ = func(h *BaseAPIHandler) {
		_, _, _ = h.ExecuteWithAuthManager(context.Background(), "openai", "x/y", nil, "")
	}
	_ = func() http.Header { return nil }
)
