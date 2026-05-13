// Package qwen also exposes a token-storage struct mirroring the other
// OAuth providers (claude/codex/kiro/github) so the management layer
// can load/save credentials using the generic auth loader.
package qwen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// QwenTokenStorage persists Qwen Code credentials. AccessToken is
// short-lived; RefreshToken is the long-lived identity artefact that
// the executor trades for a new access token when the cached one
// expires. ResourceURL is Qwen's way of telling the client which
// region/endpoint to hit — we honor it at request time so traffic
// lands on the user's assigned region.
type QwenTokenStorage struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	// Expire is the Unix-seconds expiry of AccessToken. 0 when unknown.
	Expire int64 `json:"expired,omitempty"`
	// ResourceURL is the per-user endpoint override returned at login
	// and refresh time. The executor prepends scheme when missing.
	ResourceURL string `json:"resource_url,omitempty"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Scope       string `json:"scope,omitempty"`
	LastRefresh string `json:"last_refresh,omitempty"`
	// Type is always "qwen". Required by the generic auth loader.
	Type string `json:"type"`
	// Metadata is flattened during serialization so custom attrs mix
	// cleanly with the top-level fields above.
	Metadata map[string]any `json:"-"`
}

// SetMetadata lets management code inject metadata before saving.
func (ts *QwenTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes the credential to disk with metadata
// flattened. 0700 directory mode because refresh tokens must not leak.
func (ts *QwenTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "qwen"

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
