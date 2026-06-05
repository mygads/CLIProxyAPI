package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

type nonStreamingSSEChoiceState struct {
	Index        int
	Role         string
	Content      strings.Builder
	FinishReason any
}

// normalizeNonStreamingPayload collapses accidental SSE payloads returned by
// some upstream "non-stream" executions back into a normal OpenAI chat
// completion JSON body. If the SSE body carries an upstream error event, it is
// promoted to an ErrorMessage so combo fallback can try the next candidate.
func normalizeNonStreamingPayload(payload []byte) ([]byte, *interfaces.ErrorMessage) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || !looksLikeSSEChunk(trimmed) {
		return payload, nil
	}

	events := extractSSEDataEvents(trimmed)
	if len(events) == 0 {
		return payload, nil
	}

	states := map[int]*nonStreamingSSEChoiceState{}
	var (
		id      string
		model   string
		object  string
		created any
		usage   any
	)

	for _, event := range events {
		var parsed map[string]any
		if err := json.Unmarshal(event, &parsed); err != nil {
			continue
		}

		if errVal, ok := parsed["error"]; ok && errVal != nil {
			status := nonStreamingSSEErrorStatus(errVal)
			return nil, &interfaces.ErrorMessage{
				StatusCode: status,
				Error:      errors.New(string(event)),
			}
		}

		if cur, ok := parsed["id"].(string); ok && strings.TrimSpace(cur) != "" {
			id = cur
		}
		if cur, ok := parsed["model"].(string); ok && strings.TrimSpace(cur) != "" {
			model = cur
		}
		if cur, ok := parsed["object"].(string); ok && strings.TrimSpace(cur) != "" {
			object = cur
		}
		if cur, ok := parsed["created"]; ok && cur != nil {
			created = cur
		}
		if cur, ok := parsed["usage"]; ok && cur != nil {
			usage = cur
		}

		rawChoices, ok := parsed["choices"].([]any)
		if !ok {
			continue
		}
		for _, rawChoice := range rawChoices {
			choiceMap, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			idx := intValue(choiceMap["index"])
			state := states[idx]
			if state == nil {
				state = &nonStreamingSSEChoiceState{Index: idx}
				states[idx] = state
			}
			if finish, exists := choiceMap["finish_reason"]; exists && finish != nil {
				state.FinishReason = finish
			}
			accumulateNonStreamingChoiceMessage(state, choiceMap["delta"])
			accumulateNonStreamingChoiceMessage(state, choiceMap["message"])
		}
	}

	if len(states) == 0 {
		return payload, nil
	}

	indexes := make([]int, 0, len(states))
	for idx := range states {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)

	choices := make([]map[string]any, 0, len(indexes))
	for _, idx := range indexes {
		state := states[idx]
		role := strings.TrimSpace(state.Role)
		if role == "" {
			role = "assistant"
		}
		choice := map[string]any{
			"index": idx,
			"message": map[string]any{
				"role":    role,
				"content": state.Content.String(),
			},
			"finish_reason": state.FinishReason,
		}
		choices = append(choices, choice)
	}

	if strings.TrimSpace(object) == "" {
		object = "chat.completion"
	} else if strings.HasSuffix(object, ".chunk") {
		object = strings.TrimSuffix(object, ".chunk")
	}

	out := map[string]any{
		"id":      firstNonEmptyString(id, "chatcmpl-normalized"),
		"object":  object,
		"model":   model,
		"choices": choices,
	}
	if created != nil {
		out["created"] = created
	}
	if usage != nil {
		out["usage"] = usage
	}

	normalized, err := json.Marshal(out)
	if err != nil {
		return payload, nil
	}
	return normalized, nil
}

func extractSSEDataEvents(payload []byte) [][]byte {
	lines := strings.Split(string(payload), "\n")
	var (
		events  [][]byte
		current []string
	)

	flush := func() {
		if len(current) == 0 {
			return
		}
		event := strings.TrimSpace(strings.Join(current, "\n"))
		current = current[:0]
		if event == "" || event == "[DONE]" {
			return
		}
		events = append(events, []byte(event))
	}

	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		current = append(current, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	flush()
	return events
}

func accumulateNonStreamingChoiceMessage(state *nonStreamingSSEChoiceState, raw any) {
	if state == nil {
		return
	}
	messageMap, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if role, ok := messageMap["role"].(string); ok && strings.TrimSpace(role) != "" {
		state.Role = role
	}
	if content, ok := messageMap["content"].(string); ok {
		state.Content.WriteString(content)
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	default:
		return 0
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func nonStreamingSSEErrorStatus(errVal any) int {
	payload, ok := errVal.(map[string]any)
	if !ok {
		return http.StatusBadGateway
	}
	code, _ := payload["code"].(string)
	errType, _ := payload["type"].(string)
	message, _ := payload["message"].(string)
	lower := strings.ToLower(strings.TrimSpace(code + " " + errType + " " + message))
	switch {
	case strings.Contains(lower, "rate_limit"),
		strings.Contains(lower, "quota"),
		strings.Contains(lower, "cooldown"),
		strings.Contains(lower, "high traffic"),
		strings.Contains(lower, "too many requests"):
		return http.StatusTooManyRequests
	case strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "invalid_api_key"),
		strings.Contains(lower, "auth_error"),
		strings.Contains(lower, "authentication"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "payment_required"),
		strings.Contains(lower, "insufficient_quota"):
		return http.StatusPaymentRequired
	case strings.Contains(lower, "model_not_supported"),
		strings.Contains(lower, "model_not_allowed"),
		strings.Contains(lower, "model unavailable"),
		strings.Contains(lower, "unsupported model"),
		strings.Contains(lower, "not available in current public model catalog"),
		strings.Contains(lower, "not available for your plan"),
		strings.Contains(lower, "not available for your account"),
		strings.Contains(lower, "invalid_request"),
		strings.Contains(lower, "invalid_argument"),
		strings.Contains(lower, "failed_precondition"):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}
