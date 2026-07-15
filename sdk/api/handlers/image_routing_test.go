package handlers

import "testing"

// fakeImageRouter is a minimal ImageRouteResolver for tests.
type fakeImageRouter struct {
	enabled bool
	routed  map[string]bool
	chain   []string
}

func (f *fakeImageRouter) Enabled() bool { return f.enabled }
func (f *fakeImageRouter) IsRoutedCombo(name string) bool {
	return f.enabled && f.routed[name]
}
func (f *fakeImageRouter) ChainModels() []string { return f.chain }

func TestRequestHasImage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"openai text only", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false},
		{"openai string content", `{"messages":[{"role":"user","content":"hello"}]}`, false},
		{"openai image_url", `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, true},
		{"anthropic image", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}]}`, true},
		{"openai responses input_image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:x"}]}]}`, true},
		{"empty", ``, false},
		{"no messages", `{"model":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestHasImage([]byte(tc.body)); got != tc.want {
				t.Fatalf("requestHasImage=%v want %v", got, tc.want)
			}
		})
	}
}

func TestMaybeImageRerouteGuards(t *testing.T) {
	imgBody := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`)
	textBody := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	newHandler := func(router ImageRouteResolver) *BaseAPIHandler {
		return &BaseAPIHandler{
			ImageRouter: router,
			// Combos resolver so chain models that are plain leaves resolve.
			Combos: &fakeComboResolver{chains: map[string][]ComboCandidate{}},
		}
	}

	t.Run("nil router -> no reroute", func(t *testing.T) {
		h := newHandler(nil)
		if _, ok := h.maybeImageReroute("genfity/opus", imgBody); ok {
			t.Fatal("nil router must not reroute")
		}
	})

	t.Run("disabled -> no reroute", func(t *testing.T) {
		h := newHandler(&fakeImageRouter{enabled: false, routed: map[string]bool{"genfity/opus": true}, chain: []string{"vision/a"}})
		if _, ok := h.maybeImageReroute("genfity/opus", imgBody); ok {
			t.Fatal("disabled router must not reroute")
		}
	})

	t.Run("non-routed combo -> no reroute", func(t *testing.T) {
		h := newHandler(&fakeImageRouter{enabled: true, routed: map[string]bool{"genfity/other": true}, chain: []string{"vision/a"}})
		if _, ok := h.maybeImageReroute("genfity/opus", imgBody); ok {
			t.Fatal("non-routed combo must not reroute")
		}
	})

	t.Run("text-only request -> no reroute", func(t *testing.T) {
		h := newHandler(&fakeImageRouter{enabled: true, routed: map[string]bool{"genfity/opus": true}, chain: []string{"vision/a"}})
		if _, ok := h.maybeImageReroute("genfity/opus", textBody); ok {
			t.Fatal("text-only request must not reroute")
		}
	})

	t.Run("image + routed combo -> reroute to chain", func(t *testing.T) {
		h := newHandler(&fakeImageRouter{enabled: true, routed: map[string]bool{"genfity/opus": true}, chain: []string{"vision/a", "mk/mk/auto"}})
		attempts, ok := h.maybeImageReroute("genfity/opus", imgBody)
		if !ok {
			t.Fatal("expected reroute")
		}
		if len(attempts) != 2 || attempts[0].Model != "vision/a" || attempts[1].Model != "mk/mk/auto" {
			t.Fatalf("chain not resolved in order: %+v", attempts)
		}
		if !attempts[len(attempts)-1].IsLast {
			t.Fatal("final attempt must be marked IsLast")
		}
		if attempts[0].IsLast {
			t.Fatal("non-final attempt must not be IsLast")
		}
	})

	t.Run("chain dedups duplicate leaves", func(t *testing.T) {
		h := newHandler(&fakeImageRouter{enabled: true, routed: map[string]bool{"genfity/opus": true}, chain: []string{"vision/a", "vision/a", "mk/mk/auto"}})
		attempts, ok := h.maybeImageReroute("genfity/opus", imgBody)
		if !ok || len(attempts) != 2 {
			t.Fatalf("expected 2 deduped attempts, got %+v (ok=%v)", attempts, ok)
		}
	})
}
