package github

// refresh_helper.go — Copilot-token rotation helper extracted from
// runtime/executor/github_executor.go::ensureCopilotToken so management
// handlers (quota probes, $TOKEN$ substitution) can share the same logic
// without depending on the executor package.
//
// Two refresh layers, mirroring OmniRoute's github.ts:200-217:
//
//   1. Exchange the long-lived GitHub access_token for a fresh ~30 min
//      Copilot bearer token. This is the happy path.
//   2. If the access_token has expired (refresh-token enabled GitHub OAuth
//      app) and a refresh_token is on file, refresh the GitHub access_token
//      first, then retry layer 1.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// copilotRefreshSkewSeconds is the early-rotation window. We exchange a
// fresh Copilot token when the cached one has fewer than this many seconds
// of validity left. Matches the executor.
const copilotRefreshSkewSeconds = 60

var copilotMuRegistry sync.Map

func copilotLockFor(authID string) *sync.Mutex {
	muAny, _ := copilotMuRegistry.LoadOrStore(authID, &sync.Mutex{})
	return muAny.(*sync.Mutex)
}

func readUnix(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case int:
		return int64(t)
	}
	return 0
}

// CopilotRefreshResult mirrors the shape RefreshIfExpired returns. Rotated
// is true when ANY persisted field changed (access_token, refresh_token,
// copilot_token, copilot_expires_at, copilot_endpoints) — callers persist
// only on Rotated to keep disk writes off the hot path.
type CopilotRefreshResult struct {
	CopilotToken string
	Rotated      bool
}

// EnsureCopilotToken returns a fresh Copilot bearer token, exchanging when
// the cached one is missing or about to expire. Mutates `metadata` in place
// with rotated fields; the caller owns persistence.
//
// Safe under concurrency: per-authID locking serializes the exchange, and
// re-checks under the lock so the second caller sees the cached refresh.
func EnsureCopilotToken(ctx context.Context, authID string, metadata map[string]any) (CopilotRefreshResult, error) {
	if metadata == nil {
		return CopilotRefreshResult{}, fmt.Errorf("github copilot: nil metadata")
	}
	if cached, _ := metadata["copilot_token"].(string); cached != "" {
		if readUnix(metadata["copilot_expires_at"]) > time.Now().Unix()+copilotRefreshSkewSeconds {
			return CopilotRefreshResult{CopilotToken: cached}, nil
		}
	}

	mu := copilotLockFor(authID)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the lock; another goroutine may have just refreshed.
	cached, _ := metadata["copilot_token"].(string)
	if cached != "" && readUnix(metadata["copilot_expires_at"]) > time.Now().Unix()+copilotRefreshSkewSeconds {
		return CopilotRefreshResult{CopilotToken: cached}, nil
	}

	accessToken, _ := metadata["access_token"].(string)
	if accessToken == "" {
		return CopilotRefreshResult{}, &authError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "github copilot: missing access_token in credential metadata",
		}
	}

	// Layer 1 — exchange existing GitHub access_token for a Copilot bearer.
	resp, err := ExchangeCopilotToken(ctx, accessToken)
	if err == nil {
		metadata["copilot_token"] = resp.Token
		metadata["copilot_expires_at"] = resp.ExpiresAt
		if resp.Endpoints != nil {
			metadata["copilot_endpoints"] = resp.Endpoints
		}
		metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
		return CopilotRefreshResult{CopilotToken: resp.Token, Rotated: true}, nil
	}

	// Layer 2 — refresh the GitHub access token, then retry the exchange.
	// Only available when the OAuth app issued a refresh_token (post-2022
	// apps or enterprise apps). Otherwise surface the original error so the
	// scheduler marks the credential unavailable.
	refreshToken, _ := metadata["refresh_token"].(string)
	if refreshToken == "" {
		return CopilotRefreshResult{}, fmt.Errorf("github copilot: exchange token: %w", err)
	}
	clientID, _ := metadata["client_id"].(string)
	clientSecret, _ := metadata["client_secret"].(string)
	refreshed, refreshErr := RefreshGitHubToken(ctx, clientID, clientSecret, refreshToken)
	if refreshErr != nil {
		return CopilotRefreshResult{}, fmt.Errorf("github copilot: refresh failed after exchange error %v: %w", err, refreshErr)
	}
	metadata["access_token"] = refreshed.AccessToken
	if rotated := strings.TrimSpace(refreshed.RefreshToken); rotated != "" {
		// Losing rotation bricks future refreshes — always persist.
		metadata["refresh_token"] = rotated
	}
	resp, err = ExchangeCopilotToken(ctx, refreshed.AccessToken)
	if err != nil {
		return CopilotRefreshResult{}, fmt.Errorf("github copilot: exchange after refresh: %w", err)
	}
	metadata["copilot_token"] = resp.Token
	metadata["copilot_expires_at"] = resp.ExpiresAt
	if resp.Endpoints != nil {
		metadata["copilot_endpoints"] = resp.Endpoints
	}
	metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	return CopilotRefreshResult{CopilotToken: resp.Token, Rotated: true}, nil
}

// authError mirrors sdk/cliproxy/auth.Error without importing it — keeps
// internal/auth/github free of executor-side dependencies (would otherwise
// create an import cycle since the executor depends on both).
type authError struct {
	HTTPStatus int
	Message    string
}

func (e *authError) Error() string { return e.Message }
