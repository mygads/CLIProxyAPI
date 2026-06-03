package combo

import (
	"strings"
	"testing"
)

func TestValidate_rejectsBareModelInEntry(t *testing.T) {
	c := &Combo{
		Name: "genfity-2.1",
		Entries: []Entry{
			{Priority: 1, Model: "claude-opus-4-7"}, // bare, no prefix
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for bare model, got nil")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("expected prefix hint in error, got: %v", err)
	}
}

func TestValidate_allowsSlashInName(t *testing.T) {
	c := &Combo{
		Name: "team/my-combo",
		Entries: []Entry{
			{Priority: 1, Model: "cc/claude-opus-4-7"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("slash in combo name should be allowed, got: %v", err)
	}
}

func TestValidate_rejectsDuplicateEntries(t *testing.T) {
	c := &Combo{
		Name: "dup",
		Entries: []Entry{
			{Priority: 1, Model: "cc/claude-opus-4-7"},
			{Priority: 2, Model: "cc/claude-opus-4-7"},
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for duplicate model")
	}
}

func TestValidate_defaultsStatus(t *testing.T) {
	c := &Combo{
		Name: "ok",
		Entries: []Entry{
			{Priority: 1, Model: "cc/claude-opus-4-7"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Status != StatusActive {
		t.Errorf("expected default status %q, got %q", StatusActive, c.Status)
	}
}

func TestResolve_fallbackReturnsPriorityOrder(t *testing.T) {
	r := NewRegistry()
	err := r.Upsert(&Combo{
		Name:        "test",
		LoadBalance: false,
		Entries: []Entry{
			{Priority: 2, Model: "cx/gpt-5.5"},
			{Priority: 1, Model: "cc/claude-opus-4-7"},
			{Priority: 3, Model: "glm/glm-4.6"},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	candidates, ok := r.Resolve("test")
	if !ok {
		t.Fatal("combo not resolved")
	}
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	if candidates[0].Model != "cc/claude-opus-4-7" {
		t.Errorf("entry #0: expected cc/..., got %q", candidates[0].Model)
	}
	if candidates[1].Model != "cx/gpt-5.5" {
		t.Errorf("entry #1: expected cx/..., got %q", candidates[1].Model)
	}
	if candidates[2].Model != "glm/glm-4.6" {
		t.Errorf("entry #2: expected glm/..., got %q", candidates[2].Model)
	}
	if candidates[0].IsLast {
		t.Error("first candidate should not be IsLast")
	}
	if !candidates[2].IsLast {
		t.Error("last candidate should be IsLast")
	}
}

func TestResolve_loadBalanceRotatesFirstEntry(t *testing.T) {
	r := NewRegistry()
	err := r.Upsert(&Combo{
		Name:        "rr",
		LoadBalance: true,
		Entries: []Entry{
			{Priority: 1, Model: "a/one"},
			{Priority: 2, Model: "b/two"},
			{Priority: 3, Model: "c/three"},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	firstHeads := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		candidates, ok := r.Resolve("rr")
		if !ok {
			t.Fatal("combo not resolved")
		}
		firstHeads = append(firstHeads, candidates[0].Model)
	}
	expected := []string{"b/two", "c/three", "a/one", "b/two", "c/three", "a/one"}
	for i, got := range firstHeads {
		if got != expected[i] {
			t.Errorf("request %d: expected first head %q, got %q", i, expected[i], got)
		}
	}
}


func TestResolve_disabledComboSkipped(t *testing.T) {
	r := NewRegistry()
	_ = r.Upsert(&Combo{
		Name:   "off",
		Status: StatusDisabled,
		Entries: []Entry{
			{Priority: 1, Model: "cc/claude-opus-4-7"},
		},
	})
	if _, ok := r.Resolve("off"); ok {
		t.Error("disabled combo should not resolve")
	}
}

func TestShouldFallback_retriableStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, tc := range cases {
		got := ShouldFallback(tc.status, nil, nil)
		if got != tc.want {
			t.Errorf("status %d: want %v, got %v", tc.status, tc.want, got)
		}
	}
}

func TestShouldFallback_triggerKeywordMatch(t *testing.T) {
	body := []byte(`{"error":{"message":"You have exceeded your current quota"}}`)

	if !ShouldFallback(429, body, []string{"quota_exceeded"}) {
		// doesn't match the literal substring
	} else {
		t.Error("exact keyword 'quota_exceeded' should not match 'exceeded your current quota'")
	}

	if !ShouldFallback(429, body, []string{"exceeded"}) {
		t.Error("keyword 'exceeded' should match body")
	}
	if !ShouldFallback(429, body, []string{"EXCEEDED"}) {
		t.Error("keyword match should be case-insensitive")
	}
	if ShouldFallback(200, body, []string{"exceeded"}) {
		t.Error("non-retriable status should not fall through even if keyword matches")
	}
}

func TestShouldFallback_400ModelNotFound(t *testing.T) {
	cases := []struct {
		name     string
		body     []byte
		triggers []string
		want     bool
	}{
		{
			name: "public model catalog error",
			body: []byte(`{"error":{"code":"invalid_request_error","message":"Model \"kimi-k2.6\" is not available in current public model catalog.","type":"invalid_request_error"}}`),
			want: true,
		},
		{
			name: "model not found",
			body: []byte(`{"error":{"message":"model_not_found","type":"not_found_error"}}`),
			want: true,
		},
		{
			name: "validation error should NOT retry",
			body: []byte(`{"error":{"message":"Invalid JSON: unexpected end of input"}}`),
			want: false,
		},
		{
			name:     "model catalog error with non-matching trigger",
			body:     []byte(`Model "x" is not available in current public model catalog.`),
			triggers: []string{"quota_exceeded"},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldFallback(400, tc.body, tc.triggers)
			if got != tc.want {
				t.Errorf("status 400: want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestRegistry_upsertReplacesAndDeleteClears(t *testing.T) {
	r := NewRegistry()
	_ = r.Upsert(&Combo{
		Name: "x",
		Entries: []Entry{
			{Priority: 1, Model: "cc/a"},
		},
	})
	_ = r.Upsert(&Combo{
		Name: "x",
		Entries: []Entry{
			{Priority: 1, Model: "cx/b"},
		},
	})
	got, _ := r.Get("x")
	if got.Entries[0].Model != "cx/b" {
		t.Errorf("expected replaced entry, got %q", got.Entries[0].Model)
	}

	r.Delete("x")
	if r.Has("x") {
		t.Error("combo should be gone after Delete")
	}
}

func TestRegistry_getReturnsClone(t *testing.T) {
	r := NewRegistry()
	_ = r.Upsert(&Combo{
		Name: "mutate",
		Entries: []Entry{
			{Priority: 1, Model: "cc/a"},
		},
	})
	got, _ := r.Get("mutate")
	got.Entries[0].Model = "tampered"

	again, _ := r.Get("mutate")
	if again.Entries[0].Model != "cc/a" {
		t.Error("Get must return a clone, not a live pointer")
	}
}
