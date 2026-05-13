// Package cursor implements Cursor IDE credential storage. Cursor's
// flow is distinct from the other OAuth providers: there is NO public
// OAuth endpoint. Operators paste a `accessToken` value they extract
// from their logged-in Cursor IDE (typically ~/.cursor/db SQLite or
// the Electron app's storage).
//
// Scope note (2026-05): this package carries the token-storage layer
// only. The actual executor (gRPC/connect-protobuf talking to
// agent.v1.AgentService/Run) is a larger piece of work tracked in PRD
// v2 Phase 2A follow-up. Until then, Cursor credentials can be
// registered and listed in the admin UI but chat traffic for the
// `cu/` prefix returns HTTP 501 from the executor skeleton.
package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

const (
	// ChatCompletionsURL is the legacy aiserver endpoint. The modern
	// agent.v1.AgentService/Run path (at the same host) is what the
	// cursor-agent CLI and Cursor IDE use today. Both live behind
	// api2.cursor.sh.
	APIBaseURL          = "https://api2.cursor.sh"
	LegacyChatPath      = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"
	AgentRunPath        = "/agent.v1.AgentService/Run"

	// Attribution headers — Cursor's edge checks these to distinguish
	// cursor-agent CLI traffic from arbitrary clients. If these drift
	// bump in lockstep with OmniRoute's cursorVersionDetector.ts.
	ClientVersionHeader = "x-cursor-client-version"
	DefaultClientVer    = "0.44.11"
)

// CursorTokenStorage persists a Cursor credential. There is no refresh
// token — Cursor issues long-lived access tokens.
type CursorTokenStorage struct {
	AccessToken string `json:"access_token"`
	// Email is informational. Cursor's token payload is a JWT, so we
	// could decode it client-side, but for now operators supply it at
	// import time.
	Email       string         `json:"email,omitempty"`
	LastRefresh string         `json:"last_refresh,omitempty"`
	Type        string         `json:"type"`
	Metadata    map[string]any `json:"-"`
}

// SetMetadata lets management code inject metadata before saving.
func (ts *CursorTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile persists the credential to disk (0700 dir mode).
func (ts *CursorTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "cursor"

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
