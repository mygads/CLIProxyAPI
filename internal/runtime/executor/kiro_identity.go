package executor

// kiro_identity.go — Provider-side identity rewrite for the Kiro upstream.
//
// Why: Kiro is fronted by Anthropic Claude but its system prompt makes it
// always answer "Saya adalah Kiro, sebuah AI-powered development environment"
// when asked who/what it is or which model it runs. Customers paying for
// access through genfity-ai-gateway expect the model to identify as the
// Genfity-published name (e.g. genfity/auto -> "auto from genfity",
// genfity/claude-opus-4.7 -> "claude-opus-4.7 from anthropic via Genfity").
//
// How: Before forwarding the request to Kiro we sniff the LAST user
// message. If it looks like a short identity question we short-circuit
// and emit a deterministic response from the executor — no upstream
// call, no risk of the Kiro system prompt overriding us. We gate on the
// last message only (not "exactly one user turn"): agentic clients
// always send a system prompt + tool defs + prior turns, so a single-turn
// gate never fired for them and Kiro's identity leaked.
//
// Scope: Kiro only. Other providers are unaffected. The check is
// intentionally conservative — the tight regex + 280-char cap mean
// real code-related questions go to the real model untouched.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/combo"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"

	"github.com/google/uuid"
)

// identityModelLabel translates Kiro upstream model ids to the public
// label that should appear in the answer. Returns pipe-separated
// "ModelName|Vendor" format for consistency with combo identity system.
func identityModelLabel(requested, upstream string) (modelID string, vendor string) {
	r := strings.ToLower(strings.TrimSpace(requested))
	u := strings.ToLower(strings.TrimSpace(upstream))

	// Strip prefix from gateway-style ids ("genfity/auto" -> "auto",
	// "kiro/auto" -> "auto", "kr/claude-opus-4-7" -> "claude-opus-4-7").
	if i := strings.LastIndex(r, "/"); i >= 0 {
		r = r[i+1:]
	}

	pick := r
	if pick == "" {
		pick = u
	}

	// Strip :free / :notools suffixes
	if i := strings.Index(pick, ":"); i >= 0 {
		pick = pick[:i]
	}

	switch pick {
	case "auto":
		return "Genfity Auto", "Genfity"
	case "claude-opus-4-7", "claude-opus-4.7":
		return "Claude Opus 4.7", "Anthropic"
	case "claude-sonnet-4-6", "claude-sonnet-4.6":
		return "Claude Sonnet 4.6", "Anthropic"
	case "claude-sonnet-4-5", "claude-sonnet-4.5":
		return "Claude Sonnet 4.5", "Anthropic"
	case "claude-haiku-4-5", "claude-haiku-4.5":
		return "Claude Haiku 4.5", "Anthropic"
	}

	// Unknown — surface the pick as-is with a generic vendor.
	return pick, "Genfity"
}

// identityQuestionRegex is the single source of truth for identity-question
// detection, shared with the combo package so the Kiro short-circuit and the
// generic combo intercept never drift apart (a past divergence let phrasings
// match one gate but not the other). See combo.IdentityQuestionRegex.
var identityQuestionRegex = combo.IdentityQuestionRegex

// kiroIdentityRewrite returns a non-empty answer when the request
// should be short-circuited with a Genfity-flavored identity response.
// Returns "" to let the request flow normally to Kiro upstream.
func kiroIdentityRewrite(openaiBody []byte, requestedModel, upstreamModel string) string {
	messages := gjson.GetBytes(openaiBody, "messages").Array()
	if len(messages) == 0 {
		return ""
	}

	// Gate on the LAST user message only. We intentionally do NOT require
	// the whole conversation to be a single user turn: agentic clients
	// (Claude Code, Kiro client, Cline) always send a system prompt + tool
	// defs + prior turns, so a single-turn gate never fired for them and
	// Kiro's "Saya Kiro, AI-powered development environment" leaked through
	// to customers who paid for a Genfity-published model. The tight regex
	// + 280-char cap keep this from hijacking real tasks that merely
	// mention "model"; a genuine "model apa kamu?" is short and should be
	// answered with the Genfity identity no matter how deep the chat is.
	last := messages[len(messages)-1]
	if last.Get("role").String() != "user" {
		return ""
	}
	text := last.Get("content").String()
	if text == "" {
		// Multi-modal content array — concatenate the text parts.
		var b strings.Builder
		last.Get("content").ForEach(func(_ gjson.Result, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(part.Get("text").String())
			}
			return true
		})
		text = b.String()
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 280 {
		// Long messages are almost always real tasks (code, docs).
		// A genuine "model apa kamu?" is short.
		return ""
	}

	if !identityQuestionRegex.MatchString(text) {
		return ""
	}

	modelID, vendor := identityModelLabel(requestedModel, upstreamModel)
	// Indonesian gets the Indonesian answer when the trigger phrase was
	// Indonesian; English otherwise. Detection is the same regex —
	// peek at common Indonesian markers in the matched text.
	lower := strings.ToLower(text)
	indo := strings.Contains(lower, "kamu") || strings.Contains(lower, "anda") ||
		strings.Contains(lower, "siapa") || strings.Contains(lower, "model apa") ||
		strings.Contains(lower, "namamu") || strings.Contains(lower, "nama anda")

	if indo {
		return fmt.Sprintf("Saya adalah %s, model AI dari %s. Ada yang bisa saya bantu?", modelID, vendor)
	}
	return fmt.Sprintf("I'm %s, an AI model by %s. How can I help?", modelID, vendor)
}

// requestedModelFromOpts pulls the customer-facing model id out of the
// executor options metadata. The conductor always populates this with
// the public model name (e.g. "genfity/auto"). Returns "" when missing.
func requestedModelFromOpts(opts cliproxyexecutor.Options) string {
	if opts.Metadata == nil {
		return ""
	}
	v, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// buildKiroIdentityResponse fabricates an OpenAI chat.completion JSON
// payload that looks identical to a real Kiro response so the
// downstream translator (TranslateNonStream) and usage parser don't
// need special-casing. Tokens are billed at minimum (1 prompt, N
// completion estimated from the answer length / 4) so the customer
// ledger reflects a real, tiny request.
func buildKiroIdentityResponse(answer string, upstreamModel string) []byte {
	completionTokens := int64(len(answer) / 4)
	if completionTokens < 1 {
		completionTokens = 1
	}
	out := map[string]any{
		"id":      "chatcmpl-" + uuid.New().String(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   upstreamModel,
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

// buildKiroIdentityStreamChunks fabricates the SSE chunks an OpenAI
// streaming consumer expects. We emit:
//   - one role chunk
//   - one content chunk with the full answer
//   - one terminal [DONE] chunk
//
// Keeping the protocol minimal avoids weird interactions with the
// downstream translator. SSE bytes already include "data: " prefix and
// the trailing blank line, matching what TranslateStream emits for the
// real path.
func buildKiroIdentityStreamChunks(answer string, upstreamModel string) [][]byte {
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
			"model":   upstreamModel,
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

// emitKiroIdentityNonStream wraps buildKiroIdentityResponse for the
// non-streaming Execute path: it runs the inverse translator so the
// caller's preferred schema (anthropic/openai/etc.) is honored. Returns
// the response payload + canned headers.
func emitKiroIdentityNonStream(ctx context.Context, from, to sdktranslator.Format, reqModel string, originalRequest, openaiRequest, fakeAssembled []byte) ([]byte, http.Header) {
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, reqModel, originalRequest, openaiRequest, fakeAssembled, &param)
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("X-Genfity-Identity-Rewrite", "1")
	return out, hdr
}

// emitKiroIdentityStream wraps buildKiroIdentityStreamChunks for the
// ExecuteStream path. Each fake SSE chunk goes through the inverse
// translator individually so the downstream client sees frames in its
// native schema.
func emitKiroIdentityStream(ctx context.Context, from, to sdktranslator.Format, reqModel string, originalRequest, openaiRequest []byte, fakeChunks [][]byte) []cliproxyexecutor.StreamChunk {
	var param any
	emitted := make([]cliproxyexecutor.StreamChunk, 0, len(fakeChunks))
	for _, raw := range fakeChunks {
		// TranslateStream operates on already-prefixed SSE lines and
		// returns one or more lines in the target format. The fake
		// frames are plain `data: {...}\n\n`; pass them through to
		// keep schema parity.
		lines := sdktranslator.TranslateStream(ctx, to, from, reqModel, originalRequest, openaiRequest, bytes.Clone(raw), &param)
		for _, ln := range lines {
			emitted = append(emitted, cliproxyexecutor.StreamChunk{Payload: ln})
		}
	}
	return emitted
}
