package combo

import (
	"encoding/json"
	"testing"
)

func TestParseDisplayName(t *testing.T) {
	tests := []struct {
		input       string
		wantModel   string
		wantVendor  string
	}{
		{"GPT-5.5|OpenAI", "GPT-5.5", "OpenAI"},
		{"Claude Opus 4.7|Anthropic", "Claude Opus 4.7", "Anthropic"},
		{"Gemini 2.5 Pro|Google", "Gemini 2.5 Pro", "Google"},
		{"SomeModel", "SomeModel", ""},
		{"", "", ""},
		{" GPT-5.5 | OpenAI ", "GPT-5.5", "OpenAI"},
	}
	for _, tt := range tests {
		model, vendor := ParseDisplayName(tt.input)
		if model != tt.wantModel || vendor != tt.wantVendor {
			t.Errorf("ParseDisplayName(%q) = (%q, %q), want (%q, %q)", tt.input, model, vendor, tt.wantModel, tt.wantVendor)
		}
	}
}

func makeOpenAIBody(role, content string) []byte {
	body := map[string]any{
		"model": "test-model",
		"messages": []map[string]any{
			{"role": role, "content": content},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func makeMultiTurnBody(messages []map[string]any) []byte {
	body := map[string]any{
		"model":    "test-model",
		"messages": messages,
	}
	b, _ := json.Marshal(body)
	return b
}

func TestIsIdentityQuestion_English(t *testing.T) {
	positives := []string{
		"who are you",
		"Who are you?",
		"what is your model",
		"which model are you",
		"are you gpt",
		"are you claude",
		"tell me your name",
		"tell me who you are",
		"which model are you using",
	}
	for _, q := range positives {
		body := makeOpenAIBody("user", q)
		if !IsIdentityQuestion(body) {
			t.Errorf("expected identity question for %q", q)
		}
	}
}

func TestIsIdentityQuestion_Indonesian(t *testing.T) {
	positives := []string{
		"siapa kamu",
		"kamu model apa",
		"model apa kamu",
		"anda siapa",
		"apakah nama kamu",
		"nama anda apa",
	}
	for _, q := range positives {
		body := makeOpenAIBody("user", q)
		if !IsIdentityQuestion(body) {
			t.Errorf("expected identity question for %q", q)
		}
	}
}

func TestIsIdentityQuestion_Negatives(t *testing.T) {
	negatives := []string{
		"fix my data model",
		"help me write a function",
		"what is the weather today",
		"explain this code to me please and also tell me about the architecture of this system in detail with examples",
	}
	for _, q := range negatives {
		body := makeOpenAIBody("user", q)
		if IsIdentityQuestion(body) {
			t.Errorf("should NOT be identity question: %q", q)
		}
	}
}

func TestIsIdentityQuestion_MultiTurn(t *testing.T) {
	// Multi-turn conversations where the LAST user message is a short
	// identity question SHOULD trigger the rewrite — agentic clients
	// (Claude Code, Kiro, Cline) are always multi-turn, and the upstream
	// provider's identity would otherwise leak. The tight regex + length
	// cap keep this from hijacking real coding tasks.
	body := makeMultiTurnBody([]map[string]any{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi there"},
		{"role": "user", "content": "who are you"},
	})
	if !IsIdentityQuestion(body) {
		t.Error("multi-turn ending in an identity question SHOULD trigger")
	}
}

func TestIsIdentityQuestion_MultiTurnNonIdentityLast(t *testing.T) {
	// Multi-turn whose last user message is a real task must NOT trigger,
	// even if an earlier turn looked like an identity question.
	body := makeMultiTurnBody([]map[string]any{
		{"role": "user", "content": "who are you"},
		{"role": "assistant", "content": "I'm an assistant"},
		{"role": "user", "content": "now help me write a sorting function in Go"},
	})
	if IsIdentityQuestion(body) {
		t.Error("multi-turn ending in a real task should NOT trigger")
	}
}

func TestIsIdentityQuestion_LongMessage(t *testing.T) {
	long := "who are you " + string(make([]byte, 300))
	body := makeOpenAIBody("user", long)
	if IsIdentityQuestion(body) {
		t.Error("long message should NOT trigger identity question")
	}
}

func TestIsIdentityQuestion_AssistantRole(t *testing.T) {
	body := makeOpenAIBody("assistant", "who are you")
	if IsIdentityQuestion(body) {
		t.Error("assistant role should NOT trigger identity question")
	}
}

func TestBuildIdentityAnswer_English(t *testing.T) {
	body := makeOpenAIBody("user", "who are you")
	answer := BuildIdentityAnswer("GPT-5.5|OpenAI", body)
	expected := "I'm GPT-5.5, an AI model by OpenAI. How can I help?"
	if answer != expected {
		t.Errorf("got %q, want %q", answer, expected)
	}
}

func TestBuildIdentityAnswer_Indonesian(t *testing.T) {
	body := makeOpenAIBody("user", "siapa kamu")
	answer := BuildIdentityAnswer("GPT-5.5|OpenAI", body)
	expected := "Saya adalah GPT-5.5, model AI dari OpenAI. Ada yang bisa saya bantu?"
	if answer != expected {
		t.Errorf("got %q, want %q", answer, expected)
	}
}

func TestBuildIdentityAnswer_NoVendor(t *testing.T) {
	body := makeOpenAIBody("user", "who are you")
	answer := BuildIdentityAnswer("SomeModel", body)
	expected := "I'm SomeModel. How can I help?"
	if answer != expected {
		t.Errorf("got %q, want %q", answer, expected)
	}
}

func TestBuildIdentityAnswer_Empty(t *testing.T) {
	body := makeOpenAIBody("user", "who are you")
	answer := BuildIdentityAnswer("", body)
	if answer != "" {
		t.Errorf("expected empty answer for empty display name, got %q", answer)
	}
}

func TestBuildIdentityResponse(t *testing.T) {
	resp := BuildIdentityResponse("I'm GPT-5.5, an AI model by OpenAI. How can I help?", "genfity/gpt-5.5:free")
	var parsed map[string]any
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["object"] != "chat.completion" {
		t.Errorf("expected object=chat.completion, got %v", parsed["object"])
	}
	choices, ok := parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatal("expected choices array")
	}
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "I'm GPT-5.5, an AI model by OpenAI. How can I help?" {
		t.Errorf("unexpected content: %v", msg["content"])
	}
}

func TestBuildIdentityStreamChunks(t *testing.T) {
	chunks := BuildIdentityStreamChunks("test answer", "test-model")
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(chunks))
	}
	// Last chunk should be [DONE]
	if string(chunks[3]) != "data: [DONE]\n\n" {
		t.Errorf("last chunk should be [DONE], got %q", string(chunks[3]))
	}
}
