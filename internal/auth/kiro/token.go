// Package kiro provides authentication and token management for Kiro AI,
// the AWS Builder ID-backed coding assistant whose free quota is the most
// generous option in our provider lineup.
//
// Kiro exposes two relevant moving parts:
//
//   1. OAuth via AWS Builder ID / the Kiro desktop auth endpoint. Users
//      complete the flow in a browser and the Kiro endpoint responds with
//      a refresh token (the same artefact the desktop app stores). The
//      proxy persists that refresh token and trades it for access tokens
//      on demand.
//
//   2. CodeWhisperer-style AWS eventstream responses for streaming chat.
//      That decoder lives in the executor, not here — this package is
//      only concerned with credential lifecycle.
package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// KiroTokenStorage persists Kiro credentials. The refresh token is the
// long-lived identity; access token is short-lived (~1 hour) and refreshed
// lazily on 401 inside the executor, same pattern as Claude/Codex.
type KiroTokenStorage struct {
	// AccessToken is the short-lived bearer token accepted by Kiro's
	// upstream endpoint. May be empty on first save — the executor will
	// mint one on the first request using RefreshToken.
	AccessToken string `json:"access_token,omitempty"`

	// RefreshToken is the long-lived artefact returned by the Kiro auth
	// endpoint after browser login. Losing it means the user must log in
	// again; it does not auto-rotate.
	RefreshToken string `json:"refresh_token"`

	// Expire is the Unix-seconds expiry of AccessToken, mirrored from the
	// `expiresAt` field of the refresh response. 0 when unknown.
	Expire int64 `json:"expired,omitempty"`

	// Region is the AWS region the CodeWhisperer endpoint should target
	// (e.g. "us-east-1"). Populated from the login response; falls back to
	// the hard-coded DefaultRegion if absent.
	Region string `json:"region,omitempty"`

	// ProfileArn optionally identifies the Kiro tenant profile. Some
	// enterprise accounts require it on CodeWhisperer requests.
	ProfileArn string `json:"profile_arn,omitempty"`

	// Email is informational. The Kiro login payload includes it and the
	// admin UI displays it to let operators tell credentials apart.
	Email string `json:"email,omitempty"`

	// AuthMethod records which login flow minted this credential. Values:
	//   "builder-id" — AWS Builder ID device-code (default for new logins)
	//   "idc"        — IAM Identity Center device-code with custom startUrl
	//   "social"     — legacy Google/GitHub Cognito flow (browser redirect)
	//   "imported"   — operator pasted a refresh token directly
	// Used by the management UI to label credentials and by the executor
	// to pick the right refresh path. Empty value on legacy files defaults
	// to "social" semantics (refresh via auth.desktop.kiro.dev/refreshToken).
	AuthMethod string `json:"auth_method,omitempty"`

	// LastRefresh is RFC3339 timestamp of the most recent refresh. Mirrors
	// the other provider storages.
	LastRefresh string `json:"last_refresh,omitempty"`

	// Type is always "kiro". Required by the generic auth loader.
	Type string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks. Flattened
	// during serialization to stay aligned with the other storages.
	Metadata map[string]any `json:"-"`
}

// SetMetadata lets management code inject metadata before saving.
func (ts *KiroTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes the storage to disk with metadata flattened. The
// parent directory is created (mode 0700) because the file contains a
// refresh token that must not leak.
func (ts *KiroTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "kiro"

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
