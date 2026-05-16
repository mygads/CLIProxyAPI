// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

const (
	codexBuiltinImageModelID      = "gpt-image-2"
	xaiBuiltinImageModelID        = "grok-imagine-image"
	xaiBuiltinImageQualityModelID = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID        = "grok-imagine-video"
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	GeminiCLI   []*ModelInfo `json:"gemini-cli"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
	XAI         []*ModelInfo `json:"xai"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetGeminiModels returns the standard Gemini model definitions.
func GetGeminiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Gemini)
}

// GetGeminiVertexModels returns Gemini model definitions for Vertex AI.
func GetGeminiVertexModels() []*ModelInfo {
	return cloneModelInfos(getModels().Vertex)
}

// GetGeminiCLIModels returns Gemini model definitions for the Gemini CLI.
func GetGeminiCLIModels() []*ModelInfo {
	return cloneModelInfos(getModels().GeminiCLI)
}

// GetAIStudioModels returns model definitions for AI Studio.
func GetAIStudioModels() []*ModelInfo {
	return cloneModelInfos(getModels().AIStudio)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexFree))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexTeam))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPlus))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPro))
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetGitHubCopilotModels returns the GitHub Copilot model catalog.
// Model IDs mirror OmniRoute's providerRegistry.ts `github` block
// (2026-05). GitHub Copilot brokers third-party models, so Claude and
// Gemini IDs here are the Copilot-side aliases, not the upstream vendor
// IDs — do not substitute e.g. "claude-opus-4-7" for "claude-opus-4.7".
func GetGitHubCopilotModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "gpt-5-mini", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "GPT-5 Mini"},
		{ID: "gpt-5.3-codex", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "GPT-5.3 Codex"},
		{ID: "gpt-5.4-mini", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "GPT-5.4 Mini"},
		{ID: "gpt-5.4", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "GPT-5.4"},
		{ID: "gpt-5.5", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "GPT-5.5"},
		{ID: "claude-haiku-4.5", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Claude Haiku 4.5"},
		{ID: "claude-sonnet-4.5", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Claude Sonnet 4.5"},
		{ID: "claude-sonnet-4.6", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Claude Sonnet 4.6"},
		{ID: "claude-opus-4-5-20251101", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Claude Opus 4.5 (Full ID)"},
		{ID: "claude-opus-4.6", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Claude Opus 4.6"},
		{ID: "claude-opus-4.7", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Claude Opus 4.7"},
		{ID: "gemini-3.1-pro-preview", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Gemini 3.1 Pro"},
		{ID: "gemini-3-flash-preview", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Gemini 3 Flash"},
		{ID: "oswe-vscode-prime", Object: "model", Created: 1714521600, OwnedBy: "github", Type: "openai", DisplayName: "Raptor Mini"},
	}
}

// GetKiroModels returns the Kiro (CodeWhisperer) model catalog. The full
// list is sourced live from `AmazonCodeWhispererService.ListAvailableModels`
// (see internal/api/handlers/management/kiro_quota.go); this static list is
// the fallback used when the dynamic fetch is not available — for example
// during /v1/models requests that arrive before the credential has been
// probed. Keep it in sync with the upstream catalog observed on
// 2026-05-15. The model string is forwarded unchanged into CodeWhisperer
// as currentMessage.userInputMessage.modelId — do not canonicalize dots
// to dashes or the upstream returns ValidationException.
func GetKiroModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "auto", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Auto (Server Picks)"},
		{ID: "claude-opus-4.7", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Opus 4.7"},
		{ID: "claude-opus-4.6", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Opus 4.6"},
		{ID: "claude-sonnet-4.6", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Sonnet 4.6"},
		{ID: "claude-opus-4.5", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Opus 4.5"},
		{ID: "claude-sonnet-4.5", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Sonnet 4.5"},
		{ID: "claude-sonnet-4", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Sonnet 4"},
		{ID: "claude-haiku-4.5", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Claude Haiku 4.5"},
		{ID: "deepseek-3.2", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "DeepSeek v3.2"},
		{ID: "minimax-m2.5", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "MiniMax M2.5"},
		{ID: "minimax-m2.1", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "MiniMax M2.1"},
		{ID: "glm-5", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "GLM 5"},
		{ID: "qwen3-coder-next", Object: "model", Created: 1714521600, OwnedBy: "kiro", Type: "openai", DisplayName: "Qwen3 Coder Next"},
	}
}

// GetQwenModels returns the Qwen Code model catalog. Model IDs mirror
// OmniRoute's providerRegistry.ts `qwen` block (2026-05).
func GetQwenModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "qwen3-coder-plus", Object: "model", Created: 1714521600, OwnedBy: "qwen", Type: "openai", DisplayName: "Qwen3 Coder Plus"},
		{ID: "qwen3-coder-flash", Object: "model", Created: 1714521600, OwnedBy: "qwen", Type: "openai", DisplayName: "Qwen3 Coder Flash"},
		{ID: "vision-model", Object: "model", Created: 1714521600, OwnedBy: "qwen", Type: "openai", DisplayName: "Qwen3 Vision Model"},
		{ID: "coder-model", Object: "model", Created: 1714521600, OwnedBy: "qwen", Type: "openai", DisplayName: "Qwen3.6 (Coder Model)"},
	}
}

// GetClineModels returns the Cline Bot model catalog. Model IDs mirror
// OmniRoute's providerRegistry.ts `cline` block (2026-05). Cline uses
// vendor-prefixed IDs (anthropic/… , openai/… , google/…) because it
// routes internally to multiple upstreams — keep the slash as-is.
func GetClineModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "moonshotai/kimi-k2.6", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "Kimi K2.6 (Free)"},
		{ID: "anthropic/claude-sonnet-4.6", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "Claude Sonnet 4.6"},
		{ID: "anthropic/claude-opus-4.7", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "Claude Opus 4.7"},
		{ID: "google/gemini-3.1-pro-preview", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "Gemini 3.1 Pro"},
		{ID: "google/gemini-3-flash-preview", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "Gemini 3 Flash"},
		{ID: "openai/gpt-5.5", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "GPT-5.5"},
		{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "DeepSeek V4 Flash"},
		{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 1714521600, OwnedBy: "cline", Type: "openai", DisplayName: "DeepSeek V4 Pro"},
	}
}

// GetKiloCodeModels returns the KiloCode model catalog. Model IDs
// mirror OmniRoute's providerRegistry.ts `kilocode` block (2026-05).
// KiloCode brokers OpenRouter-style vendor-prefixed IDs; keep the
// slash as-is so the upstream routes correctly.
func GetKiloCodeModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "openrouter/free", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Free Models Router"},
		{ID: "qwen/qwen3.6-plus", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Qwen3.6 Plus"},
		{ID: "qwen/qwen3.5-397b-a17b", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Qwen3.5 397B A17B"},
		{ID: "openai/gpt-5.5", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "GPT-5.5"},
		{ID: "openai/gpt-5.4-mini", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "GPT-5.4 Mini"},
		{ID: "anthropic/claude-opus-4.7", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Claude Opus 4.7"},
		{ID: "anthropic/claude-sonnet-4.6", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Claude Sonnet 4.6"},
		{ID: "anthropic/claude-haiku-4.5", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Claude Haiku 4.5"},
		{ID: "google/gemini-3.1-pro-preview", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Gemini 3.1 Pro"},
		{ID: "google/gemini-3-flash-preview", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Gemini 3 Flash"},
		{ID: "google/gemini-3.1-flash-lite", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Gemini 3.1 Flash Lite"},
		{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "DeepSeek V4 Pro"},
		{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "DeepSeek V4 Flash"},
		{ID: "moonshotai/kimi-k2.6", Object: "model", Created: 1714521600, OwnedBy: "kilocode", Type: "openai", DisplayName: "Kimi K2.6"},
	}
}

// GetCursorModels returns a curated subset of Cursor model IDs. The
// real catalog on api2.cursor.sh has ~60 entries covering every Codex
// / Claude / Gemini variant; we expose the common entry points the
// Cursor IDE actually surfaces. Operators can add custom IDs via the
// management UI (Cursor accepts any string the upstream recognizes).
//
// NOTE: Executor body for Cursor is a 501 stub until PRD v2 Phase 2A
// follow-up lands the connect-protobuf implementation.
func GetCursorModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "auto", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Auto (Server Picks)"},
		{ID: "composer-2", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Composer 2"},
		{ID: "composer-2-fast", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Composer 2 Fast"},
		{ID: "gpt-5.5", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "GPT 5.5"},
		{ID: "gpt-5.5-high", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "GPT 5.5 High"},
		{ID: "gpt-5.4", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "GPT 5.4"},
		{ID: "gpt-5.3-codex", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "GPT 5.3 Codex"},
		{ID: "claude-opus-4-7-thinking-high", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Claude Opus 4.7 Thinking High"},
		{ID: "claude-4.6-opus-high-thinking", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Claude 4.6 Opus High Thinking"},
		{ID: "claude-4.6-sonnet-medium", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Claude 4.6 Sonnet Medium"},
		{ID: "gemini-3.1-pro-preview", Object: "model", Created: 1714521600, OwnedBy: "cursor", Type: "openai", DisplayName: "Gemini 3.1 Pro"},
	}
}

// --- OpenAI-compatible API-key providers (Phase 2C) ---
//
// The following catalogs cover providers that ride on the generic
// `openai_compat_executor.go`. Each has its own prefix for strict
// routing: `groq/`, `xai/`, `mistral/`, etc. Model IDs mirror
// OmniRoute's providerRegistry.ts entries (2026-05). When a provider
// adds new models, bump the list here — no Go code changes needed.

// GetGroqModels returns the Groq API-key model catalog. Groq serves
// fast inference for open-weight models.
func GetGroqModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "llama-3.3-70b-versatile", Object: "model", Created: 1714521600, OwnedBy: "groq", Type: "openai", DisplayName: "Llama 3.3 70B Versatile"},
		{ID: "llama-3.1-8b-instant", Object: "model", Created: 1714521600, OwnedBy: "groq", Type: "openai", DisplayName: "Llama 3.1 8B Instant"},
		{ID: "mixtral-8x7b-32768", Object: "model", Created: 1714521600, OwnedBy: "groq", Type: "openai", DisplayName: "Mixtral 8x7B"},
		{ID: "deepseek-r1-distill-llama-70b", Object: "model", Created: 1714521600, OwnedBy: "groq", Type: "openai", DisplayName: "DeepSeek R1 Distill Llama 70B"},
		{ID: "qwen-qwq-32b", Object: "model", Created: 1714521600, OwnedBy: "groq", Type: "openai", DisplayName: "Qwen QwQ 32B"},
	}
}

// GetXAIModels returns the xAI (Grok) API-key model catalog.
func GetXAIModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "grok-4", Object: "model", Created: 1714521600, OwnedBy: "xai", Type: "openai", DisplayName: "Grok 4"},
		{ID: "grok-4-mini", Object: "model", Created: 1714521600, OwnedBy: "xai", Type: "openai", DisplayName: "Grok 4 Mini"},
		{ID: "grok-3", Object: "model", Created: 1714521600, OwnedBy: "xai", Type: "openai", DisplayName: "Grok 3"},
		{ID: "grok-3-mini", Object: "model", Created: 1714521600, OwnedBy: "xai", Type: "openai", DisplayName: "Grok 3 Mini"},
		{ID: "grok-2-vision", Object: "model", Created: 1714521600, OwnedBy: "xai", Type: "openai", DisplayName: "Grok 2 Vision"},
	}
}

// GetMistralModels returns the Mistral API-key model catalog.
func GetMistralModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "mistral-large-latest", Object: "model", Created: 1714521600, OwnedBy: "mistral", Type: "openai", DisplayName: "Mistral Large"},
		{ID: "mistral-medium-latest", Object: "model", Created: 1714521600, OwnedBy: "mistral", Type: "openai", DisplayName: "Mistral Medium"},
		{ID: "mistral-small-latest", Object: "model", Created: 1714521600, OwnedBy: "mistral", Type: "openai", DisplayName: "Mistral Small"},
		{ID: "codestral-latest", Object: "model", Created: 1714521600, OwnedBy: "mistral", Type: "openai", DisplayName: "Codestral"},
		{ID: "mistral-nemo", Object: "model", Created: 1714521600, OwnedBy: "mistral", Type: "openai", DisplayName: "Mistral Nemo"},
		{ID: "pixtral-large-latest", Object: "model", Created: 1714521600, OwnedBy: "mistral", Type: "openai", DisplayName: "Pixtral Large"},
	}
}

// GetCloudflareAIModels returns the Cloudflare Workers AI catalog.
func GetCloudflareAIModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "@cf/meta/llama-3.3-70b-instruct-fp8-fast", Object: "model", Created: 1714521600, OwnedBy: "cloudflare", Type: "openai", DisplayName: "Llama 3.3 70B (CF)"},
		{ID: "@cf/meta/llama-3.1-8b-instruct-fast", Object: "model", Created: 1714521600, OwnedBy: "cloudflare", Type: "openai", DisplayName: "Llama 3.1 8B Fast"},
		{ID: "@cf/mistralai/mistral-small-3.1-24b-instruct", Object: "model", Created: 1714521600, OwnedBy: "cloudflare", Type: "openai", DisplayName: "Mistral Small 3.1"},
		{ID: "@cf/qwen/qwq-32b", Object: "model", Created: 1714521600, OwnedBy: "cloudflare", Type: "openai", DisplayName: "Qwen QwQ 32B (CF)"},
		{ID: "@cf/deepseek-ai/deepseek-r1-distill-qwen-32b", Object: "model", Created: 1714521600, OwnedBy: "cloudflare", Type: "openai", DisplayName: "DeepSeek R1 Distill 32B"},
	}
}

// GetDeepSeekModels returns the DeepSeek dedicated-prefix catalog.
func GetDeepSeekModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "deepseek-chat", Object: "model", Created: 1714521600, OwnedBy: "deepseek", Type: "openai", DisplayName: "DeepSeek Chat"},
		{ID: "deepseek-reasoner", Object: "model", Created: 1714521600, OwnedBy: "deepseek", Type: "openai", DisplayName: "DeepSeek Reasoner"},
		{ID: "deepseek-v4-pro", Object: "model", Created: 1714521600, OwnedBy: "deepseek", Type: "openai", DisplayName: "DeepSeek V4 Pro"},
		{ID: "deepseek-v4-flash", Object: "model", Created: 1714521600, OwnedBy: "deepseek", Type: "openai", DisplayName: "DeepSeek V4 Flash"},
	}
}

// GetOpenRouterModels returns a curated subset of OpenRouter's catalog.
// OpenRouter brokers hundreds of models — this is the top 15 most used.
// Operators can add custom model IDs via the management UI; openai-compat
// passes them through unchanged.
func GetOpenRouterModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "openai/gpt-5.5", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "GPT 5.5 (OR)"},
		{ID: "openai/gpt-5.4", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "GPT 5.4 (OR)"},
		{ID: "anthropic/claude-opus-4.7", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Claude Opus 4.7 (OR)"},
		{ID: "anthropic/claude-sonnet-4.6", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Claude Sonnet 4.6 (OR)"},
		{ID: "google/gemini-3.1-pro-preview", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Gemini 3.1 Pro (OR)"},
		{ID: "meta-llama/llama-3.3-70b-instruct", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Llama 3.3 70B"},
		{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "DeepSeek V4 Pro (OR)"},
		{ID: "mistralai/mistral-large-latest", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Mistral Large (OR)"},
		{ID: "qwen/qwen3.6-plus", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Qwen3.6 Plus (OR)"},
		{ID: "moonshotai/kimi-k2.6", Object: "model", Created: 1714521600, OwnedBy: "openrouter", Type: "openai", DisplayName: "Kimi K2.6 (OR)"},
	}
}

// GetFireworksModels returns the Fireworks.ai catalog.
func GetFireworksModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "accounts/fireworks/models/llama-v3p3-70b-instruct", Object: "model", Created: 1714521600, OwnedBy: "fireworks", Type: "openai", DisplayName: "Llama 3.3 70B"},
		{ID: "accounts/fireworks/models/deepseek-r1", Object: "model", Created: 1714521600, OwnedBy: "fireworks", Type: "openai", DisplayName: "DeepSeek R1"},
		{ID: "accounts/fireworks/models/qwen3-coder-480b-a35b-instruct", Object: "model", Created: 1714521600, OwnedBy: "fireworks", Type: "openai", DisplayName: "Qwen3 Coder 480B"},
		{ID: "accounts/fireworks/models/mixtral-8x22b-instruct", Object: "model", Created: 1714521600, OwnedBy: "fireworks", Type: "openai", DisplayName: "Mixtral 8x22B"},
	}
}

// GetCerebrasModels returns the Cerebras Cloud catalog.
func GetCerebrasModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "llama3.3-70b", Object: "model", Created: 1714521600, OwnedBy: "cerebras", Type: "openai", DisplayName: "Llama 3.3 70B (Cerebras)"},
		{ID: "llama3.1-8b", Object: "model", Created: 1714521600, OwnedBy: "cerebras", Type: "openai", DisplayName: "Llama 3.1 8B (Cerebras)"},
		{ID: "qwen-3-235b-a22b-instruct-2507", Object: "model", Created: 1714521600, OwnedBy: "cerebras", Type: "openai", DisplayName: "Qwen3 235B"},
		{ID: "deepseek-r1-distill-llama-70b", Object: "model", Created: 1714521600, OwnedBy: "cerebras", Type: "openai", DisplayName: "DeepSeek R1 Distill 70B"},
	}
}

// GetCohereModels returns the Cohere API-key catalog.
func GetCohereModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "command-r-plus", Object: "model", Created: 1714521600, OwnedBy: "cohere", Type: "openai", DisplayName: "Command R+"},
		{ID: "command-r", Object: "model", Created: 1714521600, OwnedBy: "cohere", Type: "openai", DisplayName: "Command R"},
		{ID: "command-a-03-2025", Object: "model", Created: 1714521600, OwnedBy: "cohere", Type: "openai", DisplayName: "Command A"},
	}
}

// GetTogetherModels returns the Together.ai catalog.
func GetTogetherModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "meta-llama/Llama-3.3-70B-Instruct-Turbo", Object: "model", Created: 1714521600, OwnedBy: "together", Type: "openai", DisplayName: "Llama 3.3 70B Turbo"},
		{ID: "deepseek-ai/DeepSeek-R1-Distill-Llama-70B-Free", Object: "model", Created: 1714521600, OwnedBy: "together", Type: "openai", DisplayName: "DeepSeek R1 70B (Free)"},
		{ID: "Qwen/Qwen3-235B-A22B-Instruct-2507-tput", Object: "model", Created: 1714521600, OwnedBy: "together", Type: "openai", DisplayName: "Qwen3 235B"},
	}
}

// GetOllamaModels returns a sentinel "*" entry so /v1/models announces
// ollama support. Ollama is self-hosted with user-supplied model IDs;
// the real catalog depends on which models the operator pulled.
func GetOllamaModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "llama3.3", Object: "model", Created: 1714521600, OwnedBy: "ollama", Type: "openai", DisplayName: "Llama 3.3 (Ollama)"},
		{ID: "deepseek-r1", Object: "model", Created: 1714521600, OwnedBy: "ollama", Type: "openai", DisplayName: "DeepSeek R1 (Ollama)"},
		{ID: "qwq", Object: "model", Created: 1714521600, OwnedBy: "ollama", Type: "openai", DisplayName: "QwQ (Ollama)"},
	}
}

// GetPerplexityModels returns the Perplexity API catalog.
func GetPerplexityModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "sonar", Object: "model", Created: 1714521600, OwnedBy: "perplexity", Type: "openai", DisplayName: "Sonar"},
		{ID: "sonar-pro", Object: "model", Created: 1714521600, OwnedBy: "perplexity", Type: "openai", DisplayName: "Sonar Pro"},
		{ID: "sonar-reasoning", Object: "model", Created: 1714521600, OwnedBy: "perplexity", Type: "openai", DisplayName: "Sonar Reasoning"},
		{ID: "sonar-reasoning-pro", Object: "model", Created: 1714521600, OwnedBy: "perplexity", Type: "openai", DisplayName: "Sonar Reasoning Pro"},
	}
}

// GetGLMModels returns the Zhipu GLM dedicated-prefix catalog.
func GetGLMModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "glm-4.6", Object: "model", Created: 1714521600, OwnedBy: "glm", Type: "openai", DisplayName: "GLM-4.6"},
		{ID: "glm-4.5", Object: "model", Created: 1714521600, OwnedBy: "glm", Type: "openai", DisplayName: "GLM-4.5"},
		{ID: "glm-4.5-flash", Object: "model", Created: 1714521600, OwnedBy: "glm", Type: "openai", DisplayName: "GLM-4.5 Flash"},
	}
}

// GetKimiAPIKeyModels returns the Kimi API-key (non-OAuth) catalog.
// Distinct from the Kimi Coding OAuth provider which uses a different prefix.
func GetKimiAPIKeyModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "kimi-k2-0711-preview", Object: "model", Created: 1714521600, OwnedBy: "kimi", Type: "openai", DisplayName: "Kimi K2"},
		{ID: "kimi-k2-turbo-preview", Object: "model", Created: 1714521600, OwnedBy: "kimi", Type: "openai", DisplayName: "Kimi K2 Turbo"},
	}
}

// GetSiliconflowModels returns the SiliconFlow catalog.
func GetSiliconflowModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "deepseek-ai/DeepSeek-V3", Object: "model", Created: 1714521600, OwnedBy: "siliconflow", Type: "openai", DisplayName: "DeepSeek V3"},
		{ID: "Qwen/Qwen2.5-72B-Instruct", Object: "model", Created: 1714521600, OwnedBy: "siliconflow", Type: "openai", DisplayName: "Qwen 2.5 72B"},
	}
}

// GetHyperbolicModels returns the Hyperbolic catalog.
func GetHyperbolicModels() []*ModelInfo {
	return []*ModelInfo{
		{ID: "meta-llama/Meta-Llama-3.1-405B-Instruct", Object: "model", Created: 1714521600, OwnedBy: "hyperbolic", Type: "openai", DisplayName: "Llama 3.1 405B"},
		{ID: "Qwen/Qwen2.5-Coder-32B-Instruct", Object: "model", Created: 1714521600, OwnedBy: "hyperbolic", Type: "openai", DisplayName: "Qwen 2.5 Coder 32B"},
	}
}

// GetAntigravityModels returns the standard Antigravity model definitions.
func GetAntigravityModels() []*ModelInfo {
	return cloneModelInfos(getModels().Antigravity)
}

// WithCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates. Built-ins replace any matching IDs
// already present in the provided slice.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, codexBuiltinImageModelInfo())
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo())
}

func codexBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImageModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2",
		Version:     codexBuiltinImageModelID,
	}
}

func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - gemini-cli
//   - aistudio
//   - codex
//   - kimi
//   - antigravity
//   - github
//   - kiro
//   - qwen
//   - cline
//   - kilocode
//   - cursor
//   - xai
//   - groq, mistral, cloudflare-ai, deepseek, openrouter, fireworks,
//     cerebras, cohere, together, ollama, perplexity, glm, kimi-apikey,
//     siliconflow, hyperbolic
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
		return GetGeminiModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "gemini-cli":
		return GetGeminiCLIModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return GetCodexProModels()
	case "kimi":
		return GetKimiModels()
	case "antigravity":
		return GetAntigravityModels()
	case "github":
		return GetGitHubCopilotModels()
	case "kiro":
		return GetKiroModels()
	case "qwen":
		return GetQwenModels()
	case "cline":
		return GetClineModels()
	case "kilocode":
		return GetKiloCodeModels()
	case "cursor":
		return GetCursorModels()
	case "groq":
		return GetGroqModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	case "mistral":
		return GetMistralModels()
	case "cloudflare-ai", "cloudflare":
		return GetCloudflareAIModels()
	case "deepseek":
		return GetDeepSeekModels()
	case "openrouter":
		return GetOpenRouterModels()
	case "fireworks":
		return GetFireworksModels()
	case "cerebras":
		return GetCerebrasModels()
	case "cohere":
		return GetCohereModels()
	case "together":
		return GetTogetherModels()
	case "ollama":
		return GetOllamaModels()
	case "perplexity":
		return GetPerplexityModels()
	case "glm":
		return GetGLMModels()
	case "kimi-apikey":
		return GetKimiAPIKeyModels()
	case "siliconflow":
		return GetSiliconflowModels()
	case "hyperbolic":
		return GetHyperbolicModels()
	default:
		return nil
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	data := getModels()
	allModels := [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.GeminiCLI,
		data.AIStudio,
		data.CodexPro,
		data.Kimi,
		data.Antigravity,
		GetGitHubCopilotModels(),
		GetKiroModels(),
		GetQwenModels(),
		GetClineModels(),
		GetKiloCodeModels(),
		GetCursorModels(),
		GetGroqModels(),
		GetXAIModels(),
		GetMistralModels(),
		GetCloudflareAIModels(),
		GetDeepSeekModels(),
		GetOpenRouterModels(),
		GetFireworksModels(),
		GetCerebrasModels(),
		GetCohereModels(),
		GetTogetherModels(),
		GetOllamaModels(),
		GetPerplexityModels(),
		GetGLMModels(),
		GetKimiAPIKeyModels(),
		GetSiliconflowModels(),
		GetHyperbolicModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	return nil
}
