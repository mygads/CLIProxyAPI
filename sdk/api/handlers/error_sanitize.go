package handlers

import (
	"bytes"
	"encoding/json"
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
func sanitizeErrorText(errText string, status int) string {
	return safeMessageForStatus(status)
}

// sanitizeUpstreamErrorJSON replaces every public error body with a safe
// OpenAI-compatible envelope. The original errText remains available to the
// request logger/error metadata before this body is sent to the customer.
func sanitizeUpstreamErrorJSON(body []byte, status int) []byte {
	return buildFallbackErrorJSON(status)
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
	if !bytes.Contains(body, []byte(`"model"`)) && !bytes.Contains(body, []byte(`"reasoning_content"`)) && !bytes.Contains(body, []byte(`"error"`)) {
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
			if lowerKey == "reasoning_content" {
				delete(typed, key)
				changed = true
				continue
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
