package handlers

import (
	"encoding/json"
	"strings"
)

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
	switch {
	case status == 429:
		return "Rate limit exceeded. Please retry shortly."
	case status == 503:
		return "Service temporarily unavailable. Please retry shortly."
	case status >= 500:
		return "An upstream error occurred. Please retry your request."
	case status == 401 || status == 403:
		return "Authentication or permission error."
	case status == 404:
		return "The requested resource was not found."
	default:
		return "An error occurred while processing your request."
	}
}

// sanitizeErrorText replaces a raw error string with a safe message if it
// contains internal infrastructure leaks; otherwise returns it unchanged.
func sanitizeErrorText(errText string, status int) string {
	if containsInternalLeak(errText) {
		return safeMessageForStatus(status)
	}
	return errText
}

// sanitizeUpstreamErrorJSON parses an upstream JSON error body, redacts
// internal details (provider names, model names, litellm/CLIProxy mentions,
// cooldown specifics), and returns a safe JSON body. The shape of the
// outermost object is preserved when possible so SDK clients still see a
// `{"error":{"message":...}}` envelope.
func sanitizeUpstreamErrorJSON(body []byte, status int) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		// Not parseable JSON — fall back to a fully synthetic safe body.
		return buildFallbackErrorJSON(status)
	}

	if !sanitizeJSONValueInPlace(payload, status) {
		// Nothing leaked — return the body unchanged.
		return body
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return buildFallbackErrorJSON(status)
	}
	return out
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
