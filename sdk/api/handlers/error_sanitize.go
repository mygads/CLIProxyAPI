package handlers

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const customerGatewayBusyMessage = "The requested model/provider is currently experiencing high traffic. Please try again later."

// internalKeywords are substrings indicating an internal infrastructure detail
// has leaked into an error message and must be redacted before reaching customers.
var internalKeywords = []string{
	"litellm",
	"midstreamfallback",
	"all credentials for model",
	"cooling down",
	"openaiexception",
	"openalexception",
	"apiconnectionerror",
	"/v0/management/",
	"mtr/",
	"via provider",
	"openai_compatible",
	"original exception",
	"cliproxy",
}

var thinkingTagPattern = regexp.MustCompile(`(?is)<thinking>\s*.*?\s*</thinking>\s*`)

func containsInternalLeak(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, kw := range internalKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// safeMessageForStatus returns a generic customer-facing message for the
// given HTTP status code. No internal details are exposed.
func safeMessageForStatus(status int) string {
	return customerGatewayBusyMessage
}

// sanitizeErrorText replaces a raw error string with a safe message if it
// contains internal infrastructure leaks; otherwise returns it unchanged.
// Only sanitize when necessary — user-facing errors like "unknown provider for
// model X" are safe to show and help debugging typos.
func sanitizeErrorText(errText string, status int) string {
	if containsInternalLeak(errText) {
		return safeMessageForStatus(status)
	}
	return errText
}

// sanitizeUpstreamErrorJSON replaces every public error body with a safe
// OpenAI-compatible envelope. The original errText remains available to the
// request logger/error metadata before this body is sent to the customer.
func sanitizeUpstreamErrorJSON(body []byte, status int) []byte {
	return buildFallbackErrorJSON(status)
}

func looksLikeSSEChunk(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	return bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte(":")) ||
		bytes.Contains(trimmed, []byte("\ndata:")) ||
		bytes.Contains(trimmed, []byte("\nevent:")) ||
		bytes.Contains(trimmed, []byte("\n:"))
}

func stripThinkingTags(text string) (string, bool) {
	if text == "" {
		return text, false
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "<thinking>") || !strings.Contains(lower, "</thinking>") {
		return text, false
	}
	cleaned := thinkingTagPattern.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)
	return cleaned, cleaned != text
}

func sanitizePublicSSEChunk(chunk []byte, publicModel string) []byte {
	lines := strings.Split(string(chunk), "\n")
	var result strings.Builder
	modified := false

	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(trimmed, ":"):
			if strings.TrimSpace(strings.TrimPrefix(trimmed, ":")) != "" {
				result.WriteString(": keep-alive\n")
				modified = true
				continue
			}
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "" || data == "[DONE]" {
				result.WriteString(line)
				result.WriteString("\n")
				continue
			}
			if json.Valid([]byte(data)) {
				var payload any
				if err := json.Unmarshal([]byte(data), &payload); err == nil {
					if sanitizePublicJSONInPlace(payload, publicModel) {
						rewritten, err := json.Marshal(payload)
						if err == nil {
							result.WriteString("data: ")
							result.Write(rewritten)
							result.WriteString("\n")
							modified = true
							continue
						}
					}
				}
			} else if cleaned, changed := stripThinkingTags(data); changed {
				result.WriteString("data: ")
				result.WriteString(cleaned)
				result.WriteString("\n")
				modified = true
				continue
			}
		}

		result.WriteString(line)
		result.WriteString("\n")
	}

	if !modified {
		return chunk
	}
	out := result.String()
	if len(out) > 0 && out[len(out)-1] == '\n' && (len(chunk) == 0 || chunk[len(chunk)-1] != '\n') {
		out = out[:len(out)-1]
	}
	return []byte(out)
}

// sanitizeJSONValueInPlace walks the parsed JSON value and replaces leaky
// strings. Returns true if any modifications were made.
func sanitizeJSONValueInPlace(v any, status int) bool {
	switch typed := v.(type) {
	case map[string]any:
		modified := false
		for key, value := range typed {
			lowerKey := strings.ToLower(key)
			// Drop fields that always leak internals.
			if lowerKey == "provider" || lowerKey == "model" {
				if s, ok := value.(string); ok && containsInternalLeak(s) {
					delete(typed, key)
					modified = true
					continue
				}
				// Conservative: drop "provider" entirely; keep "model" if
				// it doesn't look internal (e.g. just "gpt-4").
				if lowerKey == "provider" {
					delete(typed, key)
					modified = true
					continue
				}
			}
			if s, ok := value.(string); ok {
				if containsInternalLeak(s) {
					typed[key] = safeMessageForStatus(status)
					modified = true
					continue
				}
			}
			if sanitizeJSONValueInPlace(value, status) {
				modified = true
			}
		}
		return modified
	case []any:
		modified := false
		for i, value := range typed {
			if s, ok := value.(string); ok && containsInternalLeak(s) {
				typed[i] = safeMessageForStatus(status)
				modified = true
				continue
			}
			if sanitizeJSONValueInPlace(value, status) {
				modified = true
			}
		}
		return modified
	}
	return false
}

func buildFallbackErrorJSON(status int) []byte {
	errType := "server_error"
	code := "upstream_error"
	switch {
	case status == 429:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case status == 401 || status == 403:
		errType = "authentication_error"
		code = "auth_error"
	case status == 404:
		errType = "invalid_request_error"
		code = "not_found"
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": safeMessageForStatus(status),
			"type":    errType,
			"code":    code,
		},
	}
	out, _ := json.Marshal(payload)
	return out
}

// SanitizePublicResponse rewrites a successful public response before it is
// sent to customers. Combo routing may execute a hidden upstream model; this
// function forces any response model fields back to the requested public model
// and removes provider-specific reasoning_content fields.
func SanitizePublicResponse(body []byte, publicModel string) []byte {
	if len(body) == 0 {
		return body
	}
	if looksLikeSSEChunk(body) {
		return sanitizePublicSSEChunk(body, publicModel)
	}
	if !bytes.Contains(body, []byte(`"model"`)) &&
		!bytes.Contains(body, []byte(`"reasoning_content"`)) &&
		!bytes.Contains(body, []byte(`"encrypted_content"`)) &&
		!bytes.Contains(body, []byte(`"error"`)) &&
		!bytes.Contains(bytes.ToLower(body), []byte("<thinking>")) {
		return body
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	changed := sanitizePublicJSONInPlace(payload, publicModel)
	if !changed {
		return body
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func sanitizePublicJSONInPlace(value any, publicModel string) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for key, nested := range typed {
			lowerKey := strings.ToLower(key)
			if lowerKey == "error" {
				typed[key] = map[string]any{
					"message": safeMessageForStatus(0),
					"type":    "server_error",
					"code":    "upstream_error",
				}
				changed = true
				continue
			}
			if lowerKey == "reasoning_content" || lowerKey == "encrypted_content" {
				delete(typed, key)
				changed = true
				continue
			}
			if (lowerKey == "content" || lowerKey == "text") && nested != nil {
				if current, ok := nested.(string); ok {
					if cleaned, modified := stripThinkingTags(current); modified {
						typed[key] = cleaned
						changed = true
						continue
					}
				}
			}
			if lowerKey == "model" && publicModel != "" {
				if current, ok := nested.(string); ok && current != publicModel {
					typed[key] = publicModel
					changed = true
					continue
				}
			}
			if sanitizePublicJSONInPlace(nested, publicModel) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, nested := range typed {
			if sanitizePublicJSONInPlace(nested, publicModel) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}
