package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
)

const (
	maxKiroMessages   = 200
	maxKiroTools      = 64
	maxKiroToolSchema = 64 << 10
	maxKiroAllSchemas = 256 << 10

	// Amazon Q Developer supports up to 20 images per message and 3.75 MB
	// per decoded image. Raw JSON byte length is deliberately not used as a
	// proxy for model context: base64 expands images by roughly one third,
	// while a 1M-token text context can legitimately exceed the old 1 MiB
	// guard by several times.
	maxKiroImages     = 20
	maxKiroImageBytes = 3_750_000
)

var kiroToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type kiroCompatibilityIssue struct {
	Reason          string
	Detail          string
	ToolName        string
	MessageCount    int
	ToolCount       int
	ToolNamesSample []string
	PayloadBytes    int
	ImageCount      int
	MaxImageBytes   int
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
	issue := &kiroCompatibilityIssue{PayloadBytes: len(rawJSON)}

	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		issue.Reason = "invalid_json"
		return issue
	}
	if imageIssue := kiroImageCompatibilityIssue(payload); imageIssue != nil {
		imageIssue.PayloadBytes = len(rawJSON)
		return imageIssue
	}
	messages, _ := payload["messages"].([]any)
	issue.MessageCount = len(messages)
	if len(messages) > maxKiroMessages {
		issue.Reason = "too_many_messages"
		return issue
	}

	tools, _ := payload["tools"].([]any)
	issue.ToolCount = len(tools)
	declared := make(map[string]map[string]any, len(tools))
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

		schema := definition["parameters"]
		if schema == nil {
			schema = definition["input_schema"]
		}
		var schemaObject map[string]any
		if schema != nil {
			var ok bool
			schemaObject, ok = schema.(map[string]any)
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
		declared[name] = schemaObject
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
		schema, ok := declared[name]
		if !ok || name == "" {
			return "undeclared_tool_call"
		}
		args := map[string]any{}
		if arguments != nil {
			switch typed := arguments.(type) {
			case string:
				if strings.TrimSpace(typed) == "" || json.Unmarshal([]byte(typed), &args) != nil || args == nil {
					return "invalid_tool_arguments"
				}
			case map[string]any:
				args = typed
			default:
				return "invalid_tool_arguments"
			}
		}
		if err := validateKiroToolArguments(args, schema, "arguments"); err != nil {
			issue.ToolName = name
			issue.Detail = err.Error()
			return "tool_arguments_schema_mismatch"
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

func kiroImageCompatibilityIssue(payload map[string]any) *kiroCompatibilityIssue {
	issue := &kiroCompatibilityIssue{}
	inspectContent := func(rawContent any) bool {
		parts, _ := rawContent.([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part == nil {
				continue
			}
			var encoded string
			switch strings.ToLower(textField(part, "type")) {
			case "image_url", "input_image":
				if imageURL, ok := part["image_url"].(map[string]any); ok {
					encoded = base64FromDataURI(textField(imageURL, "url"))
				} else if imageURL, ok := part["image_url"].(string); ok {
					encoded = base64FromDataURI(imageURL)
				}
			case "image":
				source, _ := part["source"].(map[string]any)
				if strings.EqualFold(textField(source, "type"), "base64") {
					encoded = textField(source, "data")
				}
			}
			if encoded == "" {
				continue
			}
			issue.ImageCount++
			decodedBytes, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
			if err != nil {
				issue.Reason = "invalid_image_data"
				issue.Detail = fmt.Sprintf("image %d is not valid base64", issue.ImageCount)
				return false
			}
			if decodedBytes > int64(issue.MaxImageBytes) {
				issue.MaxImageBytes = int(decodedBytes)
			}
			if decodedBytes > maxKiroImageBytes {
				issue.Reason = "image_too_large"
				issue.Detail = fmt.Sprintf("image %d is %d bytes; maximum is %d", issue.ImageCount, decodedBytes, maxKiroImageBytes)
				return false
			}
			if issue.ImageCount > maxKiroImages {
				issue.Reason = "too_many_images"
				issue.Detail = fmt.Sprintf("request has more than %d embedded images", maxKiroImages)
				return false
			}
		}
		return true
	}
	inspectItems := func(rawItems any) bool {
		items, _ := rawItems.([]any)
		for _, rawItem := range items {
			item, _ := rawItem.(map[string]any)
			if item != nil && !inspectContent(item["content"]) {
				return false
			}
		}
		return true
	}
	if !inspectItems(payload["messages"]) || !inspectItems(payload["input"]) {
		return issue
	}
	return nil
}

func base64FromDataURI(value string) string {
	value = strings.TrimSpace(value)
	comma := strings.Index(value, ",")
	if !strings.HasPrefix(strings.ToLower(value), "data:image/") || comma < 0 {
		return ""
	}
	metadata := strings.ToLower(value[:comma])
	if !strings.Contains(metadata, ";base64") {
		return ""
	}
	return value[comma+1:]
}

func validateKiroToolArguments(value any, schema map[string]any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if enumValues, ok := schema["enum"].([]any); ok && len(enumValues) > 0 {
		matched := false
		for _, candidate := range enumValues {
			if fmt.Sprint(candidate) == fmt.Sprint(value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not an allowed enum value", path)
		}
	}
	if types := kiroSchemaTypes(schema["type"]); len(types) > 0 {
		matched := false
		for _, expected := range types {
			if kiroValueMatchesType(value, expected) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s has the wrong JSON type", path)
		}
	}
	if object, ok := value.(map[string]any); ok {
		if required, ok := schema["required"].([]any); ok {
			for _, rawName := range required {
				name, _ := rawName.(string)
				if name != "" {
					if _, exists := object[name]; !exists {
						return fmt.Errorf("%s.%s is required", path, name)
					}
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, child := range object {
			rawChildSchema, declared := properties[name]
			if !declared {
				if allow, ok := schema["additionalProperties"].(bool); ok && !allow {
					return fmt.Errorf("%s.%s is not declared", path, name)
				}
				continue
			}
			childSchema, _ := rawChildSchema.(map[string]any)
			if err := validateKiroToolArguments(child, childSchema, path+"."+name); err != nil {
				return err
			}
		}
	}
	if array, ok := value.([]any); ok {
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, item := range array {
				if err := validateKiroToolArguments(item, itemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func kiroSchemaTypes(raw any) []string {
	switch typed := raw.(type) {
	case string:
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func kiroValueMatchesType(value any, expected string) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case json.Number, float64, float32, int, int32, int64:
			return true
		}
	case "integer":
		switch number := value.(type) {
		case json.Number:
			_, err := number.Int64()
			return err == nil
		case float64:
			return math.Trunc(number) == number
		case float32:
			return float32(math.Trunc(float64(number))) == number
		case int, int32, int64:
			return true
		}
	case "null":
		return value == nil
	}
	return false
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
