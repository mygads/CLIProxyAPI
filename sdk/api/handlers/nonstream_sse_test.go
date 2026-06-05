package handlers

import (
	"strings"
	"testing"
)

func TestNormalizeNonStreamingPayload_ConvertsSSESuccessToJSON(t *testing.T) {
	raw := []byte("\n" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}],\"created\":1780664615,\"id\":\"genflowai-processing\",\"model\":\"genflowai/claude-opus-4.8\",\"object\":\"chat.completion.chunk\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\",\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}],\"created\":1780664616,\"id\":\"chatcmpl-1780664616754\",\"model\":\"genflowai/claude-opus-4.8\",\"object\":\"chat.completion.chunk\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"created\":1780664616,\"id\":\"chatcmpl-1780664616754\",\"model\":\"genflowai/claude-opus-4.8\",\"object\":\"chat.completion.chunk\",\"usage\":{\"completion_tokens\":1,\"prompt_tokens\":1440,\"total_tokens\":1441}}\n\n" +
		"data: [DONE]\n\n")

	out, errMsg := normalizeNonStreamingPayload(raw)
	if errMsg != nil {
		t.Fatalf("unexpected normalize error: %+v", errMsg)
	}
	text := string(out)
	if strings.Contains(text, "data:") {
		t.Fatalf("expected JSON payload, got SSE: %s", text)
	}
	if !strings.Contains(text, `"object":"chat.completion"`) {
		t.Fatalf("expected chat.completion object, got %s", text)
	}
	if !strings.Contains(text, `"content":"ok"`) {
		t.Fatalf("expected collapsed assistant content, got %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("expected terminal finish_reason, got %s", text)
	}
	if !strings.Contains(text, `"total_tokens":1441`) {
		t.Fatalf("expected usage to survive normalization, got %s", text)
	}
}

func TestNormalizeNonStreamingPayload_PromotesSSEErrorToCandidateFailure(t *testing.T) {
	raw := []byte("\n" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}],\"created\":1780664615,\"id\":\"genflowai-processing\",\"model\":\"genflowai/gpt-5.5\",\"object\":\"chat.completion.chunk\"}\n\n" +
		"data: {\"error\":{\"code\":\"upstream_error\",\"message\":\"The requested model/provider is currently experiencing high traffic. Please try again later.\",\"type\":\"server_error\"}}\n\n" +
		"data: [DONE]\n\n")

	out, errMsg := normalizeNonStreamingPayload(raw)
	if errMsg == nil {
		t.Fatal("expected semantic SSE error to become candidate failure")
	}
	if out != nil {
		t.Fatalf("expected no payload on semantic SSE error, got %s", out)
	}
	if errMsg.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 for high-traffic fallback", errMsg.StatusCode)
	}
	if errMsg.Error == nil || !strings.Contains(errMsg.Error.Error(), "high traffic") {
		t.Fatalf("expected raw semantic error body to survive for fallback classification, got %+v", errMsg)
	}
}

func TestNormalizeNonStreamingPayload_IgnoresRegularJSON(t *testing.T) {
	raw := []byte(`{"id":"x","object":"chat.completion","model":"genfity/kimi-k2.6","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	out, errMsg := normalizeNonStreamingPayload(raw)
	if errMsg != nil {
		t.Fatalf("unexpected error on regular JSON: %+v", errMsg)
	}
	if string(out) != string(raw) {
		t.Fatalf("regular JSON should be unchanged, got %s", out)
	}
}
