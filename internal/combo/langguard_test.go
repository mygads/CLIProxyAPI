package combo

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsWesternBrandedDisplayName(t *testing.T) {
	western := []string{
		"claude-opus-4.8|Anthropic",
		"gpt-5.5|OpenAI",
		"gpt-5.3-codex|OpenAI",
		"Gemini 3.1 Pro | Google",
		"Gemini 3.5 Flash|Gemini AI",
		"claude|Anthropic",
		"chatgpt|OpenAI",
	}
	for _, d := range western {
		if !IsWesternBrandedDisplayName(d) {
			t.Errorf("expected %q to be Western-branded", d)
		}
	}

	chinese := []string{
		"glm-5.1|ZAI",
		"Qwen3.7-Max|Alibaba",
		"kimi-k2.6|Moonshotai",
		"minimax-m2.5|BytePlus",
		"deepseek-v4-pro|Deepseek",
		"mimo-v2.5|Xiaomi",
		"", // empty display name (route combos) → no guard
	}
	for _, d := range chinese {
		if IsWesternBrandedDisplayName(d) {
			t.Errorf("expected %q NOT to be Western-branded", d)
		}
	}
}

func TestInjectLanguageGuard_Claude(t *testing.T) {
	// String system.
	in := []byte(`{"system":"You are helpful.","messages":[{"role":"user","content":"hi"}]}`)
	out := InjectLanguageGuard("Claude", in, "claude-opus-4.8|Anthropic")
	sys := gjson.GetBytes(out, "system").String()
	if !strings.Contains(sys, "You are helpful.") || !strings.Contains(sys, "CRITICAL OUTPUT-LANGUAGE RULE") {
		t.Fatalf("claude string system not appended: %s", sys)
	}

	// Array system.
	inArr := []byte(`{"system":[{"type":"text","text":"sys a"}],"messages":[]}`)
	outArr := InjectLanguageGuard("Claude", inArr, "claude|Anthropic")
	parts := gjson.GetBytes(outArr, "system").Array()
	if len(parts) != 2 || !strings.Contains(parts[1].Get("text").String(), "OUTPUT-LANGUAGE") {
		t.Fatalf("claude array system not appended: %s", string(outArr))
	}

	// Missing system.
	inNone := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	outNone := InjectLanguageGuard("Claude", inNone, "claude|Anthropic")
	if !strings.Contains(gjson.GetBytes(outNone, "system").String(), "OUTPUT-LANGUAGE") {
		t.Fatalf("claude missing system not created: %s", string(outNone))
	}
}

func TestInjectLanguageGuard_OpenAI(t *testing.T) {
	// Existing string system message.
	in := []byte(`{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	out := InjectLanguageGuard("OpenAI", in, "gpt-5.5|OpenAI")
	sys := gjson.GetBytes(out, "messages.0.content").String()
	if !strings.Contains(sys, "be brief") || !strings.Contains(sys, "OUTPUT-LANGUAGE") {
		t.Fatalf("openai system append failed: %s", string(out))
	}
	if gjson.GetBytes(out, "messages.1.content").String() != "hi" {
		t.Fatalf("openai user message clobbered: %s", string(out))
	}

	// No system message → prepend one, preserving order.
	inNoSys := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	outNoSys := InjectLanguageGuard("OpenAI", inNoSys, "gpt-5.5|OpenAI")
	if gjson.GetBytes(outNoSys, "messages.0.role").String() != "system" {
		t.Fatalf("openai prepend system failed: %s", string(outNoSys))
	}
	if gjson.GetBytes(outNoSys, "messages.1.content").String() != "hi" {
		t.Fatalf("openai user message lost after prepend: %s", string(outNoSys))
	}
}

func TestInjectLanguageGuard_Gemini(t *testing.T) {
	in := []byte(`{"system_instruction":{"parts":[{"text":"sys"}]},"contents":[]}`)
	out := InjectLanguageGuard("Gemini", in, "Gemini 3.1 Pro | Google")
	parts := gjson.GetBytes(out, "system_instruction.parts").Array()
	if len(parts) != 2 || !strings.Contains(parts[1].Get("text").String(), "OUTPUT-LANGUAGE") {
		t.Fatalf("gemini system_instruction append failed: %s", string(out))
	}

	// camelCase key variant (Gemini CLI clients).
	inCamel := []byte(`{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[]}`)
	outCamel := InjectLanguageGuard("GeminiCLI", inCamel, "Gemini 3.5 Flash|Gemini AI")
	pc := gjson.GetBytes(outCamel, "systemInstruction.parts").Array()
	if len(pc) != 2 {
		t.Fatalf("gemini camelCase append failed: %s", string(outCamel))
	}
}

func TestInjectLanguageGuard_Responses(t *testing.T) {
	in := []byte(`{"instructions":"original","input":[]}`)
	out := InjectLanguageGuard("OpenaiResponse", in, "gpt-5.4|OpenAI")
	instr := gjson.GetBytes(out, "instructions").String()
	if !strings.Contains(instr, "original") || !strings.Contains(instr, "OUTPUT-LANGUAGE") {
		t.Fatalf("responses instructions append failed: %s", instr)
	}
}

func TestInjectLanguageGuard_NoOpForChineseBrand(t *testing.T) {
	in := []byte(`{"system":"hi","messages":[]}`)
	out := InjectLanguageGuard("Claude", in, "glm-5.1|ZAI")
	if string(out) != string(in) {
		t.Fatalf("expected no-op for Chinese-branded combo, got: %s", string(out))
	}
}

func TestInjectLanguageGuard_NoOpForEmptyDisplay(t *testing.T) {
	in := []byte(`{"system":"hi","messages":[]}`)
	if string(InjectLanguageGuard("Claude", in, "")) != string(in) {
		t.Fatal("expected no-op for empty display name")
	}
}

func TestInjectLanguageGuard_Idempotent(t *testing.T) {
	in := []byte(`{"system":"hi","messages":[]}`)
	once := InjectLanguageGuard("Claude", in, "claude|Anthropic")
	twice := InjectLanguageGuard("Claude", once, "claude|Anthropic")
	if string(once) != string(twice) {
		t.Fatalf("injection not idempotent:\nonce:  %s\ntwice: %s", string(once), string(twice))
	}
	if strings.Count(string(twice), "CRITICAL OUTPUT-LANGUAGE RULE") != 1 {
		t.Fatalf("directive appears more than once: %s", string(twice))
	}
}

func TestInjectLanguageGuard_UnknownFormatFailsOpen(t *testing.T) {
	in := []byte(`{"weird":"shape"}`)
	if string(InjectLanguageGuard("SomethingElse", in, "claude|Anthropic")) != string(in) {
		t.Fatal("expected fail-open no-op for unknown handler type")
	}
}
