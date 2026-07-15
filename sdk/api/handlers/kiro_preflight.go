package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// Live Kiro rejects multi-megabyte OpenCode histories with
	// CONTENT_LENGTH_EXCEEDS_THRESHOLD. Keep enough headroom for translator
	// expansion and route larger, otherwise-valid public payloads elsewhere.
	maxKiroPayloadBytes = 1 << 20
	maxKiroMessages     = 200
	maxKiroTools        = 64
	maxKiroToolSchema   = 64 << 10
	maxKiroAllSchemas   = 256 << 10
)

var kiroToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type kiroCompatibilityIssue struct {
	Reason          string
	MessageCount    int
	ToolCount       int
	ToolNamesSample []string
}

func isKiroServer3Candidate(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "server3/kr/")
}

// kiroPayloadCompatibilityIssue is a read-only candidate preflight. It does
// not change or reject a payload that is valid at the public API boundary; it
// only prevents known-incompatible shapes from being sent to Kiro, allowing
// the combo loop to continue to its next provider without poisoning Kiro's
// cooldown/health score.
func kiroPayloadCompatibilityIssue(rawJSON []byte) *kiroCompatibilityIssue {
	issue := &kiroCompatibilityIssue{}
	if len(rawJSON) > maxKiroPayloadBytes {
		issue.Reason = "request_too_large"
		return issue
	}

	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		issue.Reason = "invalid_json"
		return issue
	}
	messages, _ := payload["messages"].([]any)
	issue.MessageCount = len(messages)
	if len(messages) > maxKiroMessages {
		issue.Reason = "too_many_messages"
		return issue
	}

	tools, _ := payload["tools"].([]any)
	issue.ToolCount = len(tools)
	declared := make(map[string]struct{}, len(tools))
	allSchemaBytes := 0
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			issue.Reason = "invalid_tool_declaration"
			return issue
		}
		definition := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			definition = fn
		}
		name, _ := definition["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			// Hosted OpenAI tools are not translated into Kiro function specs.
			if toolType, _ := tool["type"].(string); toolType != "" && toolType != "function" {
				continue
			}
			issue.Reason = "missing_tool_name"
			return issue
		}
		issue.ToolNamesSample = append(issue.ToolNamesSample, name)
		if !kiroToolNamePattern.MatchString(name) {
			issue.Reason = "unsupported_tool_name"
			return issue
		}
		if _, duplicate := declared[name]; duplicate {
			issue.Reason = "duplicate_tool_name"
			return issue
		}
		declared[name] = struct{}{}

		schema := definition["parameters"]
		if schema == nil {
			schema = definition["input_schema"]
		}
		if schema != nil {
			schemaObject, ok := schema.(map[string]any)
			if !ok {
				issue.Reason = "invalid_tool_schema"
				return issue
			}
			if rootType, _ := schemaObject["type"].(string); rootType != "" && rootType != "object" {
				issue.Reason = "unsupported_tool_schema_root"
				return issue
			}
			encoded, _ := json.Marshal(schemaObject)
			if len(encoded) > maxKiroToolSchema {
				issue.Reason = "tool_schema_too_large"
				return issue
			}
			allSchemaBytes += len(encoded)
		}
	}
	sort.Strings(issue.ToolNamesSample)
	if len(issue.ToolNamesSample) > 8 {
		issue.ToolNamesSample = issue.ToolNamesSample[:8]
	}
	if len(tools) > maxKiroTools {
		issue.Reason = "too_many_tools"
		return issue
	}
	if allSchemaBytes > maxKiroAllSchemas {
		issue.Reason = "tool_schemas_too_large"
		return issue
	}

	type callState struct {
		name      string
		messageNo int
		result    bool
	}
	calls := make(map[string]*callState)
	registerCall := func(id, name string, arguments any, messageNo int) string {
		id = strings.TrimSpace(id)
		name = strings.TrimSpace(name)
		if id == "" {
			return "missing_tool_call_id"
		}
		if _, duplicate := calls[id]; duplicate {
			return "duplicate_tool_call_id"
		}
		if _, ok := declared[name]; !ok || name == "" {
			return "undeclared_tool_call"
		}
		if arguments != nil {
			switch typed := arguments.(type) {
			case string:
				var object map[string]any
				if strings.TrimSpace(typed) == "" || json.Unmarshal([]byte(typed), &object) != nil || object == nil {
					return "invalid_tool_arguments"
				}
			case map[string]any:
			default:
				return "invalid_tool_arguments"
			}
		}
		calls[id] = &callState{name: name, messageNo: messageNo}
		return ""
	}
	registerResult := func(id string, messageNo int) string {
		call, ok := calls[strings.TrimSpace(id)]
		if !ok || call.messageNo >= messageNo {
			return "unmatched_tool_result"
		}
		if call.result {
			return "duplicate_tool_result"
		}
		call.result = true
		return ""
	}

	for messageNo, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		role, _ := message["role"].(string)
		if role == "assistant" {
			if toolCalls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range toolCalls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					if reason := registerCall(textField(call, "id"), textField(fn, "name"), fn["arguments"], messageNo); reason != "" {
						issue.Reason = reason
						return issue
					}
				}
			}
		}
		if role == "tool" {
			if reason := registerResult(textField(message, "tool_call_id"), messageNo); reason != "" {
				issue.Reason = reason
				return issue
			}
		}
		blocks, _ := message["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			switch textField(block, "type") {
			case "tool_use":
				if reason := registerCall(textField(block, "id"), textField(block, "name"), block["input"], messageNo); reason != "" {
					issue.Reason = reason
					return issue
				}
			case "tool_result":
				if reason := registerResult(textField(block, "tool_use_id"), messageNo); reason != "" {
					issue.Reason = reason
					return issue
				}
			}
		}
	}
	for _, call := range calls {
		if !call.result {
			issue.Reason = "missing_tool_result"
			return issue
		}
	}
	return nil
}

func textField(object map[string]any, key string) string {
	if object == nil {
		return ""
	}
	text, _ := object[key].(string)
	return text
}

func incompatibleKiroPayloadError(model, reason string) error {
	return fmt.Errorf("incompatible_payload: candidate %q skipped (%s)", model, reason)
}
