package handlers

import (
	"encoding/json"
	"testing"
)

func TestIsEmptyCompletionResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"openai empty content", `{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`, true},
		{"openai reasoning only", `{"choices":[{"message":{"content":"","reasoning_content":"hidden"},"finish_reason":"length"}]}`, true},
		{"openai text", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`, false},
		{"openai tool call", `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"x","type":"function"}]},"finish_reason":"tool_calls"}]}`, false},
		{"openai refusal", `{"choices":[{"message":{"content":null,"refusal":"no"},"finish_reason":"stop"}]}`, false},
		{"anthropic empty", `{"type":"message","content":[],"stop_reason":"max_tokens"}`, true},
		{"anthropic thinking only", `{"type":"message","content":[{"type":"thinking","thinking":"hidden"}],"stop_reason":"max_tokens"}`, true},
		{"anthropic text", `{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`, false},
		{"anthropic tool", `{"type":"message","content":[{"type":"tool_use","id":"x","name":"search","input":{}}],"stop_reason":"tool_use"}`, false},
		{"unrelated endpoint", `{"data":[{"embedding":[0.1]}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEmptyCompletionResponse([]byte(tt.body)); got != tt.want {
				t.Fatalf("isEmptyCompletionResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamHeartbeatFormats(t *testing.T) {
	openAI := OpenAIStreamHeartbeat("genfity/test")
	var parsed map[string]any
	if err := json.Unmarshal(openAI, &parsed); err != nil {
		t.Fatalf("OpenAI heartbeat must be valid JSON: %v", err)
	}
	if got := string(ClaudeStreamHeartbeat()); got != "event: ping\ndata: {\"type\":\"ping\"}\n\n" {
		t.Fatalf("unexpected Anthropic heartbeat: %q", got)
	}
}
