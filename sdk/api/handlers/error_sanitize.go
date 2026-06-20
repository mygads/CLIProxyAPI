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

// thinkingTagPattern matches a complete reasoning block in the content field.
// Upstreams disagree on the tag spelling: most use <thinking>…</thinking>, but
// some reasoning models (MiniMax-M*, MiMo) emit <think>…</think>. Match both so
// raw chain-of-thought — which may be in Chinese — never reaches the customer.
var thinkingTagPattern = regexp.MustCompile(`(?is)<think(?:ing)?>\s*.*?\s*</think(?:ing)?>\s*`)

type publicStreamSanitizer struct {
	publicModel    string
	insideThinking bool
}

func newPublicStreamSanitizer(publicModel string) *publicStreamSanitizer {
	return &publicStreamSanitizer{publicModel: publicModel}
}

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
	hasOpen := strings.Contains(lower, "<thinking>") || strings.Contains(lower, "<think>")
	hasClose := strings.Contains(lower, "</thinking>") || strings.Contains(lower, "</think>")
	if !hasOpen || !hasClose {
		return text, false
	}
	cleaned := thinkingTagPattern.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)
	return cleaned, cleaned != text
}

func (s *publicStreamSanitizer) stripThinkingStreamText(text string) (string, bool) {
	if s == nil || text == "" {
		return text, false
	}
	original := text
	remaining := text
	var out strings.Builder
	changed := false

	for len(remaining) > 0 {
		lower := strings.ToLower(remaining)
		if s.insideThinking {
			end, endLen := firstTagIndex(lower, "</thinking>", "</think>")
			if end < 0 {
				changed = true
				remaining = ""
				break
			}
			remaining = remaining[end+endLen:]
			s.insideThinking = false
			changed = true
			continue
		}

		start, startLen := firstTagIndex(lower, "<thinking>", "<think>")
		if start < 0 {
			out.WriteString(remaining)
			break
		}
		out.WriteString(remaining[:start])
		remaining = remaining[start+startLen:]
		s.insideThinking = true
		changed = true
	}

	cleaned := out.String()
	return cleaned, changed || cleaned != original
}

// firstTagIndex returns the earliest index at which any of the given tag
// spellings appears in lower, along with the matched tag's length. Returns
// (-1, 0) when none are present. Used so the stream sanitizer accepts both
// <thinking> and the shorter <think> (MiniMax/MiMo) spellings.
func firstTagIndex(lower string, tags ...string) (int, int) {
	best, bestLen := -1, 0
	for _, t := range tags {
		if idx := strings.Index(lower, t); idx >= 0 && (best < 0 || idx < best) {
			best, bestLen = idx, len(t)
		}
	}
	return best, bestLen
}

func sanitizePublicSSEChunk(chunk []byte, publicModel string) []byte {
	return sanitizePublicSSEChunkWithState(chunk, publicModel, nil)
}

func sanitizePublicSSEChunkWithState(chunk []byte, publicModel string, streamState *publicStreamSanitizer) []byte {
	lines := strings.Split(string(chunk), "\n")
	var result strings.Builder
	modified := false

	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(trimmed, ":"):
			if strings.TrimSpace(strings.TrimPrefix(trimmed, ":")) != "" {
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
					if sanitizePublicJSONValueInPlace(payload, publicModel, streamState) {
						rewritten, err := json.Marshal(payload)
						if err == nil {
							if streamState != nil && !payloadHasVisiblePublicContent(payload) {
								modified = true
								continue
							}
							result.WriteString("data: ")
							result.Write(rewritten)
							result.WriteString("\n")
							modified = true
							continue
						}
					}
				}
			} else {
				var (
					cleaned string
					changed bool
				)
				if streamState != nil {
					cleaned, changed = streamState.stripThinkingStreamText(data)
				} else {
					cleaned, changed = stripThinkingTags(data)
				}
				if changed {
					modified = true
					if cleaned == "" {
						continue
					}
					result.WriteString("data: ")
					result.WriteString(cleaned)
					result.WriteString("\n")
					continue
				}
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
	return sanitizePublicResponseWithState(body, publicModel, nil)
}

func sanitizePublicResponseWithState(body []byte, publicModel string, streamState *publicStreamSanitizer) []byte {
	if len(body) == 0 {
		return body
	}
	if looksLikeSSEChunk(body) {
		return sanitizePublicSSEChunkWithState(body, publicModel, streamState)
	}
	if !bytes.Contains(body, []byte(`"model"`)) &&
		!bytes.Contains(body, []byte(`"reasoning_content"`)) &&
		!bytes.Contains(body, []byte(`"encrypted_content"`)) &&
		!bytes.Contains(body, []byte(`"error"`)) &&
		!bytes.Contains(bytes.ToLower(body), []byte("<think")) {
		return body
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	changed := sanitizePublicJSONValueInPlace(payload, publicModel, streamState)
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
	return sanitizePublicJSONValueInPlace(value, publicModel, nil)
}

func sanitizePublicJSONValueInPlace(value any, publicModel string, streamState *publicStreamSanitizer) bool {
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
					var (
						cleaned  string
						modified bool
					)
					if streamState != nil {
						cleaned, modified = streamState.stripThinkingStreamText(current)
					} else {
						cleaned, modified = stripThinkingTags(current)
					}
					if modified {
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
			if sanitizePublicJSONValueInPlace(nested, publicModel, streamState) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, nested := range typed {
			if sanitizePublicJSONValueInPlace(nested, publicModel, streamState) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func payloadHasVisiblePublicContent(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			lowerKey := strings.ToLower(key)
			switch lowerKey {
			case "content", "text":
				if current, ok := nested.(string); ok && strings.TrimSpace(current) != "" {
					return true
				}
			case "type":
				if current, ok := nested.(string); ok {
					current = strings.TrimSpace(current)
					if strings.HasPrefix(current, "response.") || current == "response" || current == "error" {
						return true
					}
				}
			case "finish_reason":
				switch v := nested.(type) {
				case string:
					if strings.TrimSpace(v) != "" {
						return true
					}
				case nil:
				default:
					return true
				}
			case "tool_calls", "function_call", "error":
				return true
			}
			if payloadHasVisiblePublicContent(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if payloadHasVisiblePublicContent(nested) {
				return true
			}
		}
	}
	return false
}

func publicChunkHasVisibleContent(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false
	}
	if looksLikeSSEChunk(trimmed) {
		return true
	}
	if !json.Valid(trimmed) {
		return true
	}
	var payload any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return true
	}
	return payloadHasVisiblePublicContent(payload)
}
