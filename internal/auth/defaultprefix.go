// Package auth hosts shared auth helpers that cut across the per-provider
// subpackages (claude, codex, gemini, kimi, …). This file owns the default
// prefix registry: when admin creates a new OAuth credential without an
// explicit prefix, the server fills it with the registry value so "{prefix}/"
// routing works out of the box.
//
// See docs/PRD-V3-PREFIX-LOADBALANCE.md §3.2.
package auth

import "strings"

// DefaultOAuthPrefix maps a lowercased provider key (as stored in Auth.Provider
// / auth-file metadata `type`) to its default routing prefix.
//
// The values match the final decision in the PRD (cc/cx/gmn/kr/gh/qw/cln/klc/km/ag)
// and override the older 9Router-era mapping (gc/kmc/kc/cl). Admins can still
// override per-credential via the panel.
var DefaultOAuthPrefix = map[string]string{
	// Claude Code (Anthropic OAuth).
	"claude":         "cc",
	"claude-code":    "cc",
	"claude-oauth":   "cc",
	"anthropic":      "cc",
	// Codex CLI (OpenAI ChatGPT OAuth).
	"codex":          "cx",
	"codex-cli":      "cx",
	"openai-codex":   "cx",
	// Gemini CLI (Google OAuth).
	"gemini":         "gmn",
	"gemini-cli":     "gmn",
	"gemini-oauth":   "gmn",
	// Kiro AI (AWS Builder ID).
	"kiro":           "kr",
	// GitHub Copilot (GitHub Device Code).
	"github":         "gh",
	"github-copilot": "gh",
	"copilot":        "gh",
	// Qwen Code (Alibaba DashScope OAuth).
	"qwen":           "qw",
	"qwen-code":      "qw",
	// Cline.
	"cline":          "cln",
	// KiloCode.
	"kilocode":       "klc",
	// Kimi Coding.
	"kimi":           "km",
	"kimi-coding":    "km",
	// Antigravity (Google One AI).
	"antigravity":    "ag",
}

// DefaultPrefixFor returns the registry default for a provider, or "" when
// unknown. The lookup is case- and whitespace-insensitive.
func DefaultPrefixFor(provider string) string {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" {
		return ""
	}
	return DefaultOAuthPrefix[key]
}
