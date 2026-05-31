package combo

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// IdentityQuestionRegex matches common shapes of "who/what are you" /
// "which model powers you" in English and Indonesian. Broad enough to
// catch natural phrasings (a tight single-phrase regex let "what model
// are you?", "kamu kiro ya?", "kamu jalan di atas model apa?" leak the
// upstream identity), but still anchored on an identity referent so
// casual mentions of "model" in code-help requests ("fix my data model",
// "buatkan model apa yang cocok") don't match. Paired with the 280-char
// cap in IsIdentityQuestion, false positives stay rare.
var IdentityQuestionRegex = regexp.MustCompile(`(?i)(?:` +
	// ── English ──
	`\bwho\s+(?:are|r)\s+(?:you|u)\b|` +
	`\bwhat(?:'?s| is| are)?\s+your\s+(?:name|model|identity)\b|` +
	`\bwhat\s+(?:ai\s+|llm\s+)?model\s+(?:are|r)\s+(?:you|u)\b|` +
	`\bwhich\s+(?:underlying\s+|base\s+|actual\s+|real\s+)?(?:model|ai|llm)\b|` +
	`\b(?:underlying|base|actual|real)\s+(?:model|llm)\b|` +
	`\bmodel\s+(?:powers|power|runs|run|drives|drive|behind|backing)\s+(?:you|u)\b|` +
	`\b(?:you|u)(?:'re| are|r)?\s+(?:powered|running|built|based|driven|backed)\s+(?:on|by)\b|` +
	`\bare\s+you\s+(?:really\s+|actually\s+)?(?:based\s+on\s+|built\s+on\s+|running\s+on\s+|powered\s+by\s+)?(?:gpt|claude|gemini|kiro|llama|chatgpt|kimi|qwen|deepseek|glm|minimax|mimo|anthropic|openai|moonshot)\b|` +
	`\btell\s+me\s+(?:your\s+name|who\s+you\s+are|which\s+model|what\s+model|the\s+(?:underlying\s+)?model)\b|` +
	// ── Indonesian ──
	`\bsiapa(?:kah)?\s+(?:kamu|anda|kau|lu|km)\b|` +
	`\b(?:kamu|anda|km|kau|lu)\s+(?:siapa|kiro|gpt|claude|chatgpt|gemini|kimi|qwen|deepseek)\b|` +
	`\bmodel\s+(?:apa|asli|sebenarnya|dibalik|di\s+balik)\b|` +
	`\b(?:pakai|pake|gunakan|menggunakan|jalan|berjalan|dibangun|berbasis|basis)\s+(?:di\s+atas\s+)?model\b|` +
	`\b(?:kamu|anda|km|kau|lu)\b[^.?!]{0,25}?\bmodel\s+apa\b|` +
	`\bapa(?:kah)?\s+(?:nama|model|jenis)\s*(?:mu|kamu|anda)\b|` +
	`\b(?:nama|model)\s+(?:mu|kamu|anda)\b` +
	`)`)

// ParseDisplayName splits a pipe-separated display name into model name
// and vendor. Format: "ModelName|Vendor". If no pipe, the entire string
// is the model name and vendor is empty.
func ParseDisplayName(displayName string) (modelName, vendor string) {
	parts := strings.SplitN(displayName, "|", 2)
	modelName = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		vendor = strings.TrimSpace(parts[1])
	}
	return
}

// IsIdentityQuestion checks whether an OpenAI-format request body's LAST
// message is a single-turn-style identity question. Returns true when:
//   - The last message is from the user
//   - The message text is short (<280 chars) and matches the identity regex
//
// We intentionally do NOT require the whole conversation to be a single
// user turn. Agentic clients (Claude Code, Kiro, Cline, etc.) always send
// a system prompt + tool definitions + prior turns, so a single-turn gate
// never fired for them — and the upstream provider's identity ("I'm
// Kiro…", "I'm Kimi…") leaked through to the customer. The tight regex
// plus the 280-char cap keep this from hijacking real coding tasks that
// merely mention the word "model"; a genuine "model apa kamu?" is short
// and matches regardless of how deep the conversation is.
func IsIdentityQuestion(openaiBody []byte) bool {
	messages := gjson.GetBytes(openaiBody, "messages").Array()
	if len(messages) == 0 {
		return false
	}

	last := messages[len(messages)-1]
	if last.Get("role").String() != "user" {
		return false
	}

	text := extractUserText(last)
	if text == "" || len(text) > 280 {
		return false
	}

	return IdentityQuestionRegex.MatchString(text)
}

// IsIndonesian returns true when the text contains common Indonesian markers.
func IsIndonesian(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "kamu") || strings.Contains(lower, "anda") ||
		strings.Contains(lower, "siapa") || strings.Contains(lower, "model apa") ||
		strings.Contains(lower, "namamu") || strings.Contains(lower, "nama anda")
}

// BuildIdentityAnswer constructs the identity answer string from a
// pipe-separated display name. Detects language from the request body.
func BuildIdentityAnswer(displayName string, openaiBody []byte) string {
	modelName, vendor := ParseDisplayName(displayName)
	if modelName == "" {
		return ""
	}

	// Detect language from last user message
	messages := gjson.GetBytes(openaiBody, "messages").Array()
	indo := false
	if len(messages) > 0 {
		last := messages[len(messages)-1]
		text := extractUserText(last)
		indo = IsIndonesian(text)
	}

	if vendor == "" {
		if indo {
			return fmt.Sprintf("Saya adalah %s. Ada yang bisa saya bantu?", modelName)
		}
		return fmt.Sprintf("I'm %s. How can I help?", modelName)
	}

	if indo {
		return fmt.Sprintf("Saya adalah %s, model AI dari %s. Ada yang bisa saya bantu?", modelName, vendor)
	}
	return fmt.Sprintf("I'm %s, an AI model by %s. How can I help?", modelName, vendor)
}

// BuildIdentityResponse fabricates an OpenAI chat.completion JSON payload.
func BuildIdentityResponse(answer string, model string) []byte {
	completionTokens := int64(len(answer) / 4)
	if completionTokens < 1 {
		completionTokens = 1
	}
	out := map[string]any{
		"id":      "chatcmpl-" + uuid.New().String(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": answer,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": completionTokens,
			"total_tokens":      1 + completionTokens,
		},
	}
	body, _ := json.Marshal(out)
	return body
}

// BuildIdentityStreamChunks fabricates SSE chunks for a streaming identity response.
func BuildIdentityStreamChunks(answer string, model string) [][]byte {
	id := "chatcmpl-" + uuid.New().String()
	created := time.Now().Unix()

	frame := func(delta map[string]any, finishReason any) []byte {
		choice := map[string]any{
			"index": 0,
			"delta": delta,
		}
		if finishReason != nil {
			choice["finish_reason"] = finishReason
		} else {
			choice["finish_reason"] = nil
		}
		obj := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{choice},
		}
		raw, _ := json.Marshal(obj)
		return []byte("data: " + string(raw) + "\n\n")
	}

	return [][]byte{
		frame(map[string]any{"role": "assistant"}, nil),
		frame(map[string]any{"content": answer}, nil),
		frame(map[string]any{}, "stop"),
		[]byte("data: [DONE]\n\n"),
	}
}

func extractUserText(msg gjson.Result) string {
	text := msg.Get("content").String()
	if text != "" {
		return strings.TrimSpace(text)
	}
	// Multi-modal content array
	var b strings.Builder
	msg.Get("content").ForEach(func(_ gjson.Result, part gjson.Result) bool {
		if part.Get("type").String() == "text" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(part.Get("text").String())
		}
		return true
	})
	return strings.TrimSpace(b.String())
}
