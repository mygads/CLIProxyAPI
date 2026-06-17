package handlers

import "encoding/json"

func shouldFallbackMalformedToolCallResponse(requestBody, responseBody []byte) bool {
	if !requestExpectsToolCalls(requestBody) {
		return false
	}

	var payload map[string]any
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return false
	}

	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}

	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		finishReason, _ := choice["finish_reason"].(string)
		if finishReason != "tool_calls" {
			continue
		}
		message, _ := choice["message"].(map[string]any)
		if lenToolCalls(message["tool_calls"]) == 0 {
			return true
		}
	}
	return false
}

func requestExpectsToolCalls(requestBody []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		return false
	}

	toolChoice, _ := payload["tool_choice"].(string)
	if toolChoice == "none" {
		return false
	}

	return lenToolCalls(payload["tools"]) > 0 || lenToolCalls(payload["functions"]) > 0
}

func lenToolCalls(value any) int {
	switch typed := value.(type) {
	case []any:
		return len(typed)
	default:
		return 0
	}
}
