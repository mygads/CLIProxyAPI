package handlers

import (
	"encoding/json"
	"strings"
)

// isEmptyCompletionResponse reports whether a recognized OpenAI- or
// Anthropic-compatible completion completed successfully at HTTP level but did
// not contain any client-visible text, tool call, or refusal. Reasoning-only
// output is deliberately not considered visible completion output.
func isEmptyCompletionResponse(responseBody []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return len(strings.TrimSpace(string(responseBody))) == 0
	}

	if choices, ok := payload["choices"].([]any); ok {
		if len(choices) == 0 {
			return true
		}
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			if hasVisibleValue(message["content"]) ||
				hasVisibleValue(message["refusal"]) ||
				lenToolCalls(message["tool_calls"]) > 0 ||
				message["function_call"] != nil {
				return false
			}
		}
		return true
	}

	if content, ok := payload["content"].([]any); ok {
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			typeName, _ := block["type"].(string)
			switch typeName {
			case "text":
				if hasVisibleValue(block["text"]) {
					return false
				}
			case "tool_use", "server_tool_use", "web_search_tool_result":
				return false
			}
		}
		return true
	}

	// Do not classify unrelated endpoints (embeddings, images, videos, model
	// metadata, OpenAI Responses, etc.) as empty completions.
	return false
}

func hasVisibleValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		for _, item := range typed {
			if hasVisibleValue(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if key == "text" || key == "content" || key == "refusal" {
				if hasVisibleValue(item) {
					return true
				}
			}
		}
	}
	return false
}
