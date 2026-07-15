package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToGeminiMapsMaxTokens(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{"max_tokens", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":30}`, 30},
		{"max_completion_tokens", `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":40}`, 40},
		{"max_tokens preferred", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":30,"max_completion_tokens":40}`, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ConvertOpenAIRequestToGemini("gemini-test", []byte(tt.body), false)
			if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != tt.want {
				t.Fatalf("maxOutputTokens = %d, want %d; output=%s", got, tt.want, out)
			}
		})
	}
}

func TestConvertOpenAIRequestToGeminiCleansToolSchema(t *testing.T) {
	input := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"search","parameters":{
			"type":"object","title":"Search","properties":{"country":{"type":"string"}},
			"required":["country","stale"]
		}}}]
	}`)
	out := ConvertOpenAIRequestToGemini("gemini-test", input, false)
	schema := gjson.GetBytes(out, "tools.0.functionDeclarations.0.parametersJsonSchema")
	if schema.Get("title").Exists() {
		t.Fatalf("schema title was not removed: %s", schema.Raw)
	}
	required := schema.Get("required").Array()
	if len(required) != 1 || required[0].String() != "country" {
		t.Fatalf("required was not sanitized: %s", schema.Raw)
	}
}

func TestConvertOpenAIRequestToGeminiStripsTrailingAssistantPrefill(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"prefill"}]}`)
	out := ConvertOpenAIRequestToGemini("gemini-test", input, false)
	contents := gjson.GetBytes(out, "contents").Array()
	if len(contents) != 1 || contents[0].Get("role").String() != "user" {
		t.Fatalf("unexpected contents after prefill removal: %s", gjson.GetBytes(out, "contents").Raw)
	}
}
