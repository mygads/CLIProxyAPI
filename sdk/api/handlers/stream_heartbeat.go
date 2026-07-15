package handlers

import (
	"encoding/json"
	"time"

	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
)

// OpenAIStreamHeartbeat returns a protocol-valid, contentless chat-completion
// chunk. Some OpenAI-compatible clients parse every SSE data frame as JSON and
// choke on comment heartbeats such as ": keep-alive".
func OpenAIStreamHeartbeat(model string) []byte {
	payload := map[string]any{
		"id":      "chatcmpl-keepalive",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": nil,
		}},
	}
	data, _ := json.Marshal(payload)
	return data
}

// ClaudeStreamHeartbeat is Anthropic's native SSE heartbeat event.
func ClaudeStreamHeartbeat() []byte {
	return []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
}

// streamHeartbeatChunk returns a heartbeat in the same representation used by
// the handler's data channel: OpenAI carries raw JSON (the HTTP handler adds the
// data: envelope), while Claude carries complete SSE events.
func streamHeartbeatChunk(handlerType, model string) []byte {
	switch handlerType {
	case OpenAI:
		return OpenAIStreamHeartbeat(model)
	case Claude:
		return ClaudeStreamHeartbeat()
	default:
		return []byte(": keep-alive\n\n")
	}
}
