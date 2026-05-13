// Package cline also exposes a token-storage struct mirroring the
// other OAuth providers (claude/codex/kiro/github/qwen) so the
// management layer can load/save credentials using the generic loader.
package cline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// ClineTokenStorage persists Cline credentials. AccessToken is short-
// lived (exp ~1h); RefreshToken is the long-lived identity artefact.
// Email + FirstName/LastName are informational — the admin UI uses
// them to distinguish credentials.
type ClineTokenStorage struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token"`
	Expire       int64  `json:"expired,omitempty"`
	Email        string `json:"email,omitempty"`
	FirstName    string `json:"first_name,omitempty"`
	LastName     string `json:"last_name,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
	Type         string `json:"type"`
	Metadata     map[string]any `json:"-"`
}

// SetMetadata lets management code inject metadata before saving.
func (ts *ClineTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile persists the credential to disk. 0700 directory mode
// because refresh tokens must not leak.
func (ts *ClineTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "cline"

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
