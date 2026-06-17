package handlers

import "testing"

func TestShouldFallbackMalformedToolCallResponse(t *testing.T) {
	req := []byte(`{"model":"combo","messages":[{"role":"user","content":"call tool"}],"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`)
	resp := []byte(`{"id":"x","object":"chat.completion","model":"server2/claude-sonnet-4.6-auto","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}`)

	if !shouldFallbackMalformedToolCallResponse(req, resp) {
		t.Fatal("expected malformed tool response to trigger combo fallback")
	}
}

func TestShouldFallbackMalformedToolCallResponse_AllowsValidToolCalls(t *testing.T) {
	req := []byte(`{"model":"combo","messages":[{"role":"user","content":"call tool"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	resp := []byte(`{"id":"x","object":"chat.completion","model":"route","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)

	if shouldFallbackMalformedToolCallResponse(req, resp) {
		t.Fatal("valid tool_calls response must not trigger fallback")
	}
}

func TestShouldFallbackMalformedToolCallResponse_IgnoresNonToolRequests(t *testing.T) {
	req := []byte(`{"model":"combo","messages":[{"role":"user","content":"hello"}]}`)
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}`)

	if shouldFallbackMalformedToolCallResponse(req, resp) {
		t.Fatal("non-tool request must not trigger fallback")
	}
}
