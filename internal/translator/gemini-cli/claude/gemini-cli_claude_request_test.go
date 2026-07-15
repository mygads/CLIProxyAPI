package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCLIStructuredToolResult(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"json-call-1","name":"json","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"json-call-1","content":[
			{"type":"text","text":"alpha"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
		]}]}
	]}`)
	out := ConvertClaudeRequestToCLI("gemini-test", input, false)
	if got := gjson.GetBytes(out, "request.contents.1.parts.0.functionResponse.response.result.text").String(); got != "alpha" {
		t.Fatalf("structured result lost: got=%q output=%s", got, out)
	}
	img := gjson.GetBytes(out, "request.contents.1.parts.1.inlineData")
	if img.Get("mime_type").String() != "image/png" || img.Get("data").String() != "aGVsbG8=" {
		t.Fatalf("image was not separated from tool result: %s", out)
	}
}

func TestConvertClaudeRequestToCLI_ToolChoice_SpecificTool(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hi"}
				]
			}
		],
		"tools": [
			{
				"name": "json",
				"description": "A JSON tool",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		],
		"tool_choice": {"type": "tool", "name": "json"}
	}`)

	output := ConvertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	if got := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.mode").String(); got != "ANY" {
		t.Fatalf("Expected request.toolConfig.functionCallingConfig.mode 'ANY', got '%s'", got)
	}
	allowed := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "json" {
		t.Fatalf("Expected allowedFunctionNames ['json'], got %s", gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Raw)
	}
}

func TestConvertClaudeRequestToCLI_StripsClaudeCodeAttribution(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},
			{"type": "text", "text": "User system prompt"}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`)

	output := ConvertClaudeRequestToCLI("gemini-3-flash-preview", inputJSON, false)

	parts := gjson.GetBytes(output, "request.systemInstruction.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 system part after attribution strip, got %d: %s", len(parts), gjson.GetBytes(output, "request.systemInstruction.parts").Raw)
	}
	if got := parts[0].Get("text").String(); got != "User system prompt" {
		t.Fatalf("Unexpected system part: %q", got)
	}
}
