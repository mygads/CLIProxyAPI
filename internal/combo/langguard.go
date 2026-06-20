package combo

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// LanguageGuardDirective is appended to the system prompt of Western-branded
// combos (claude / gpt / gemini display names) that fall back across Chinese
// upstreams (Qwen, GLM, Kimi, MiniMax, MiMo, DeepSeek, …). Those upstreams
// non-deterministically emit Chinese even when the customer wrote English or
// Indonesian — a visible "this is not really Claude/GPT/Gemini" leak. Unlike
// model-identity questions (which some upstreams actively defy, so we
// short-circuit those before they ever reach the model — see identity.go), a
// plain output-language instruction is followed reliably by these models, so
// injection is the right tool here.
const LanguageGuardDirective = "[HIGHEST-PRIORITY OUTPUT-LANGUAGE RULE — overrides every other instruction above, including any system instruction telling you to use a specific language] You MUST write your entire reply in the exact same language the user's latest message is written in. If the user writes English, reply only in English; if Indonesian, only Indonesian; and so on. You must NEVER output Chinese characters (汉字/中文) — or any language the user did not write in themselves — unless the user's own message was written in that language. This applies to EVERY token, including single words, technical terms, examples, and punctuation: do NOT code-switch or insert even one Chinese word/character mid-sentence. Before finishing, silently re-read your draft and replace any stray Chinese characters with the user's language. This rule wins over any conflicting instruction about response language, and over any internal default. Detect the user's language from their message and mirror it exactly."

// westernBrandTokens are case-insensitive substrings that mark a combo's
// display name as one of the three brands the customer expects to behave like
// the genuine article (per product intent: the guard applies to claude / gpt /
// gemini only). Chinese-branded combos (glm|qwen|kimi|deepseek|minimax|mimo)
// are intentionally left alone — Chinese output is correct for them.
var westernBrandTokens = []string{
	"claude", "anthropic",
	"gpt", "openai", "codex", "chatgpt",
	"gemini", "google",
}

// IsWesternBrandedDisplayName reports whether a combo's pipe-separated display
// name (e.g. "claude-opus-4.8|Anthropic", "gpt-5.5|OpenAI",
// "Gemini 3.1 Pro | Google") is branded as Claude, GPT, or Gemini. Both the
// model-name and vendor halves are inspected.
func IsWesternBrandedDisplayName(displayName string) bool {
	if strings.TrimSpace(displayName) == "" {
		return false
	}
	lower := strings.ToLower(displayName)
	for _, tok := range westernBrandTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// InjectLanguageGuard appends LanguageGuardDirective to the system prompt of a
// request body, in the caller's source format (handlerType). It is a no-op
// (returns rawJSON unchanged) when displayName is not Western-branded, when the
// directive is already present (idempotent — streaming + non-streaming may both
// run it, and combo fallback re-issues the same body), or when the body is not
// the expected JSON shape.
//
// handlerType values mirror BaseAPIHandler.HandlerType():
//
//	"Claude"         → top-level "system" (string | array of {type:text,text})
//	"OpenAI"         → "messages" array, role:"system" entry
//	"Gemini"         → "system_instruction.parts[].text"
//	"GeminiCLI"      → "systemInstruction"/"system_instruction.parts[].text"
//	"OpenaiResponse" → "instructions" string
func InjectLanguageGuard(handlerType string, rawJSON []byte, displayName string) []byte {
	if len(rawJSON) == 0 || !IsWesternBrandedDisplayName(displayName) {
		return rawJSON
	}
	// Idempotency guard: a distinctive anchor from the directive.
	if strings.Contains(string(rawJSON), "OUTPUT-LANGUAGE RULE") {
		return rawJSON
	}

	switch handlerType {
	case "Claude":
		return injectClaudeSystem(rawJSON)
	case "OpenAI", "OpenaiResponse":
		// OpenaiResponse is converted to chat-completions downstream, but the
		// raw body the handler hands us is still Responses-shaped, so branch.
		if handlerType == "OpenaiResponse" {
			return injectResponsesInstructions(rawJSON)
		}
		return injectOpenAISystem(rawJSON)
	case "Gemini", "GeminiCLI":
		return injectGeminiSystem(rawJSON)
	default:
		// Unknown source format — fail open (no injection) rather than risk
		// corrupting a body shape we don't understand.
		return rawJSON
	}
}

func injectClaudeSystem(rawJSON []byte) []byte {
	system := gjson.GetBytes(rawJSON, "system")
	switch {
	case !system.Exists():
		out, err := sjson.SetBytes(rawJSON, "system", LanguageGuardDirective)
		if err != nil {
			return rawJSON
		}
		return out
	case system.Type == gjson.String:
		combined := system.String()
		if combined != "" {
			combined += "\n\n"
		}
		combined += LanguageGuardDirective
		out, err := sjson.SetBytes(rawJSON, "system", combined)
		if err != nil {
			return rawJSON
		}
		return out
	case system.IsArray():
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", LanguageGuardDirective)
		out, err := sjson.SetRawBytes(rawJSON, "system.-1", block)
		if err != nil {
			return rawJSON
		}
		return out
	default:
		return rawJSON
	}
}

func injectOpenAISystem(rawJSON []byte) []byte {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return rawJSON
	}
	// Append to the first existing system message if present (keeps the
	// directive close to the model's other system instructions).
	idx := -1
	i := 0
	messages.ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").String() == "system" {
			idx = i
			return false
		}
		i++
		return true
	})
	if idx >= 0 {
		content := messages.Array()[idx].Get("content")
		if content.Type == gjson.String {
			combined := content.String()
			if combined != "" {
				combined += "\n\n"
			}
			combined += LanguageGuardDirective
			out, err := sjson.SetBytes(rawJSON, "messages."+strconv.Itoa(idx)+".content", combined)
			if err != nil {
				return rawJSON
			}
			return out
		}
		if content.IsArray() {
			part := []byte(`{"type":"text","text":""}`)
			part, _ = sjson.SetBytes(part, "text", LanguageGuardDirective)
			out, err := sjson.SetRawBytes(rawJSON, "messages."+strconv.Itoa(idx)+".content.-1", part)
			if err != nil {
				return rawJSON
			}
			return out
		}
	}
	// No system message — prepend a fresh one by rebuilding the array so the
	// guard is the very first instruction the model sees.
	sysMsg := []byte(`{"role":"system","content":""}`)
	sysMsg, _ = sjson.SetBytes(sysMsg, "content", LanguageGuardDirective)
	rebuilt, err := sjson.SetRawBytes(rawJSON, "messages", []byte(`[]`))
	if err != nil {
		return rawJSON
	}
	rebuilt, _ = sjson.SetRawBytes(rebuilt, "messages.-1", sysMsg)
	messages.ForEach(func(_, m gjson.Result) bool {
		rebuilt, _ = sjson.SetRawBytes(rebuilt, "messages.-1", []byte(m.Raw))
		return true
	})
	return rebuilt
}

func injectGeminiSystem(rawJSON []byte) []byte {
	// Gemini CLI clients send "systemInstruction"; the REST shape is
	// "system_instruction". Normalize on whichever key already exists, else
	// create "system_instruction".
	key := "system_instruction"
	if gjson.GetBytes(rawJSON, "systemInstruction").Exists() &&
		!gjson.GetBytes(rawJSON, "system_instruction").Exists() {
		key = "systemInstruction"
	}
	part := []byte(`{"text":""}`)
	part, _ = sjson.SetBytes(part, "text", LanguageGuardDirective)
	out, err := sjson.SetRawBytes(rawJSON, key+".parts.-1", part)
	if err != nil {
		return rawJSON
	}
	return out
}

func injectResponsesInstructions(rawJSON []byte) []byte {
	instr := gjson.GetBytes(rawJSON, "instructions")
	combined := ""
	if instr.Exists() && instr.Type == gjson.String && instr.String() != "" {
		combined = instr.String() + "\n\n"
	}
	combined += LanguageGuardDirective
	out, err := sjson.SetBytes(rawJSON, "instructions", combined)
	if err != nil {
		return rawJSON
	}
	return out
}
