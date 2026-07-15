// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import "time"

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// EnableGeminiCLIEndpoint controls whether Gemini CLI internal endpoints (/v1internal:*) are enabled.
	// Default is false for safety; when false, /v1internal:* requests are rejected.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "cc/claude-opus-4-7")
	// to target prefixed credentials.
	//
	// Default: true (strict mode). When the config key is absent it is treated as true.
	//   - Prefixed credentials only publish "{prefix}/{model}" entries on /v1/models.
	//   - Requests without a matching prefix bypass prefixed credentials entirely,
	//     which prevents cross-provider leakage (e.g. "gpt-5.5" falling through to
	//     both Codex and OpenRouter credentials at the same time).
	//
	// Set to false only to restore the legacy permissive behavior where unprefixed
	// requests may be served by any credential regardless of its Prefix.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`

	// UpstreamTimeoutSeconds sets the HTTP client timeout for requests to upstream AI providers.
	// Applies to all executors (Kiro, Claude, OpenAI-compat, Gemini, etc.).
	// Default is 180 seconds. Set to 0 for no timeout (not recommended).
	UpstreamTimeoutSeconds int `yaml:"upstream-timeout-seconds,omitempty" json:"upstream-timeout-seconds,omitempty"`

	// ComboAttemptTimeoutSeconds caps how long a single non-last combo candidate may run
	// before it is abandoned and the loop falls through to the next entry.
	// Default is 30 seconds. Set to 0 to use the default.
	ComboAttemptTimeoutSeconds int `yaml:"combo-attempt-timeout-seconds,omitempty" json:"combo-attempt-timeout-seconds,omitempty"`

	// ComboStreamIdleTimeoutSeconds bounds how long an already-committed combo stream
	// may go without receiving any byte from the upstream before it is abandoned.
	// Default is 60 seconds. Set to 0 to disable the idle timeout (not recommended).
	ComboStreamIdleTimeoutSeconds int `yaml:"combo-stream-idle-timeout-seconds,omitempty" json:"combo-stream-idle-timeout-seconds,omitempty"`
}

// UpstreamTimeout returns the configured upstream timeout duration.
// Defaults to 300s (5 minutes) if not set or zero.
func (c *SDKConfig) UpstreamTimeout() time.Duration {
	if c == nil || c.UpstreamTimeoutSeconds <= 0 {
		return 300 * time.Second
	}
	return time.Duration(c.UpstreamTimeoutSeconds) * time.Second
}

// defaultComboAttemptTimeout bounds time-to-first-byte for a non-last combo
// candidate before the loop falls through to the next entry. This bounds only
// time-to-first-visible-content, not the full generation. Thirty seconds keeps
// two dead candidates inside the common 60s client idle window while still
// allowing reasoning models time to bootstrap. The post-commit idle timeout
// separately guards a stream that starts and later stalls. Operators can
// override via combo-attempt-timeout-seconds in config.
const defaultComboAttemptTimeout = 30 * time.Second

// defaultComboStreamIdleTimeout bounds a post-commit stall (stream already
// producing, then goes silent). Kept longer (60s) than the bootstrap timeout
// so a genuinely long but steadily-producing generation is never cut short —
// the timer resets on every upstream byte.
const defaultComboStreamIdleTimeout = 60 * time.Second

// ComboAttemptTimeout returns the configured per-attempt combo timeout.
// Defaults to 30s if not set or zero.
func (c *SDKConfig) ComboAttemptTimeout() time.Duration {
	if c == nil || c.ComboAttemptTimeoutSeconds <= 0 {
		return defaultComboAttemptTimeout
	}
	return time.Duration(c.ComboAttemptTimeoutSeconds) * time.Second
}

// ComboStreamIdleTimeout returns the configured post-commit combo idle timeout.
// Defaults to 60s if not set or zero.
func (c *SDKConfig) ComboStreamIdleTimeout() time.Duration {
	if c == nil || c.ComboStreamIdleTimeoutSeconds <= 0 {
		return defaultComboStreamIdleTimeout
	}
	return time.Duration(c.ComboStreamIdleTimeoutSeconds) * time.Second
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
