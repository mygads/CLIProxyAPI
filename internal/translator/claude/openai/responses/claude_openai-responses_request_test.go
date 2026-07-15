package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaudeSanitizesToolCallIDs(t *testing.T) {
	input := []byte(`{"input":[
		{"type":"function_call","call_id":"call.with space:1","name":"Read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call.with space:1","output":"ok"}
	]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", input, false)
	toolUseID := gjson.GetBytes(out, "messages.0.content.0.id").String()
	toolResultID := gjson.GetBytes(out, "messages.1.content.0.tool_use_id").String()
	if toolUseID != "call_with_space_1" || toolResultID != toolUseID {
		t.Fatalf("tool IDs not consistently sanitized: use=%q result=%q; output=%s", toolUseID, toolResultID, out)
	}
}

func TestConvertOpenAIResponsesRequestToClaudeKeepsToolUseAdjacentToResult(t *testing.T) {
	input := []byte(`{"input":[
		{"type":"function_call","call_id":"call_1","name":"js","arguments":"{}"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking first."}]},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", input, false)
	root := gjson.ParseBytes(out)
	if root.Get("messages.0.content").String() != "Checking first." ||
		root.Get("messages.1.content.0.type").String() != "tool_use" ||
		root.Get("messages.2.content.0.type").String() != "tool_result" {
		t.Fatalf("tool use/result are not adjacent after assistant text: %s", out)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReasoningItemToThinkingBlock(t *testing.T) {
	signature := "claude_sig_request"
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[{"type":"summary_text","text":"internal reasoning"}]
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"visible answer"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	assistant := root.Get("messages.0")
	if got := assistant.Get("role").String(); got != "assistant" {
		t.Fatalf("first message role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.signature").String(); got != signature {
		t.Fatalf("thinking signature = %q, want %q", got, signature)
	}
	if got := assistant.Get("content.0.thinking").String(); got != "internal reasoning" {
		t.Fatalf("thinking text = %q, want internal reasoning", got)
	}
	if got := assistant.Get("content.1.type").String(); got != "text" {
		t.Fatalf("second content type = %q, want text. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.1.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SignatureOnlyReasoningFlushesBeforeUser(t *testing.T) {
	signature := "claude_sig_only"
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + signature + `",
				"summary":[]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	thinking := root.Get("messages.0.content.0")
	if got := thinking.Get("type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := thinking.Get("signature").String(); got != signature {
		t.Fatalf("thinking signature = %q, want %q", got, signature)
	}
	if got := thinking.Get("thinking").String(); got != "" {
		t.Fatalf("thinking text = %q, want empty", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}
