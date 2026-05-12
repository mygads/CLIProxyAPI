// Package github provides authentication and token management for GitHub
// Copilot. Copilot uses a two-step auth flow:
//
//   1. Standard GitHub OAuth device-code flow yields an access token that
//      identifies the user to GitHub's APIs.
//   2. That access token is exchanged for a short-lived Copilot bearer
//      token via `https://api.github.com/copilot_internal/v2/token`. The
//      Copilot token is the one actually sent to api.githubcopilot.com.
//
// This package owns steps 1 and 2. The Copilot token is refreshed lazily
// on 401 inside the executor.
package github

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// GitHubTokenStorage persists the long-lived GitHub OAuth access token plus
// the most recently minted Copilot bearer token.
//
// Wire shape intentionally mirrors ClaudeTokenStorage (snake_case JSON, a
// dedicated Type field, flattened metadata) so the rest of the proxy —
// auth loader, management UI, diff — can treat every provider
// interchangeably.
type GitHubTokenStorage struct {
	// AccessToken is the long-lived GitHub OAuth access token (prefix "gho_").
	// It never expires until the user revokes it, so it is the stable
	// identity that survives across Copilot-token rotations.
	AccessToken string `json:"access_token"`

	// CopilotToken is the short-lived bearer token accepted by
	// api.githubcopilot.com. Typically ~30 minutes TTL; refreshed from
	// AccessToken on demand.
	CopilotToken string `json:"copilot_token,omitempty"`

	// CopilotExpire is the Unix-seconds expiry of CopilotToken, parsed from
	// the `expires_at` field of the token-exchange response.
	CopilotExpire int64 `json:"copilot_expires_at,omitempty"`

	// Endpoints maps Copilot capabilities to upstream URLs as reported by
	// the token-exchange response (e.g. "api" -> "https://api.githubcopilot.com").
	Endpoints map[string]string `json:"endpoints,omitempty"`

	// Login is the GitHub username, populated from /user after first login.
	// Informational only — used in the admin UI.
	Login string `json:"login,omitempty"`

	// LastRefresh is the RFC3339 timestamp of the last Copilot-token
	// exchange. Mirrors ClaudeTokenStorage.LastRefresh.
	LastRefresh string `json:"last_refresh,omitempty"`

	// Type is always "github" for this storage. Required by the generic
	// auth loader that dispatches on this field.
	Type string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks. It is
	// flattened during serialization to stay compatible with the other
	// storages.
	Metadata map[string]any `json:"-"`
}

// SetMetadata lets management code inject metadata before saving.
func (ts *GitHubTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes the storage to disk as JSON with metadata flattened
// into the top-level object. The parent directory is created if missing
// (mode 0700 to match Claude's convention — these files contain bearer
// tokens and must not be world-readable).
func (ts *GitHubTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "github"

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}
	if err = json.NewEncoder(f).Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}
