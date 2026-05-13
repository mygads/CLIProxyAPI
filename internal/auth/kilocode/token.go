// Package kilocode also exposes a token-storage struct matching other
// OAuth providers so the management layer can load/save credentials
// using the generic auth loader. KiloCode has no refresh token — the
// access token is long-lived.
package kilocode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// KiloCodeTokenStorage persists KiloCode credentials. RefreshToken is
// always empty — the access token does not expire client-side (or the
// server rotates it without exposing a refresh endpoint, same outcome
// from our side).
type KiloCodeTokenStorage struct {
	AccessToken string `json:"access_token"`
	Email       string `json:"email,omitempty"`
	LastRefresh string `json:"last_refresh,omitempty"`
	Type        string `json:"type"`
	Metadata    map[string]any `json:"-"`
}

// SetMetadata lets management code inject metadata before saving.
func (ts *KiloCodeTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile persists the credential to disk. 0700 directory mode
// because the access token is the full credential (no refresh token).
func (ts *KiloCodeTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "kilocode"

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
