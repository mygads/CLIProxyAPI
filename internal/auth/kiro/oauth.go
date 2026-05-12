package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kiro endpoints reverse-engineered from the Kiro desktop app (9Router and
// OmniRoute both use the same endpoints). These are public — the Kiro
// client ID is shipped in every desktop build.
//
// The "auth.desktop.kiro.dev" hostname is the user-facing side (launches
// login in a browser), and /refreshToken handles the OAuth token lifecycle.
const (
	DefaultRegion   = "us-east-1"
	AuthBaseURL     = "https://prod.us-east-1.auth.desktop.kiro.dev"
	RefreshTokenURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	// LoginStartURL is the browser entry point for the Kiro OAuth flow.
	// The caller appends a ?state= query parameter and opens it in the
	// user's browser. Kiro redirects back to a localhost callback with the
	// refresh token in the fragment.
	LoginStartURL = "https://prod.us-east-1.auth.desktop.kiro.dev/authorize"

	// UserAgent mimics the Kiro desktop client so the endpoint does not
	// reject our traffic as unknown.
	UserAgent = "KiroIDE/0.1.0"
)

// RefreshResponse models the subset of /refreshToken we consume.
type RefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	Region       string `json:"region,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	Email        string `json:"email,omitempty"`
}

// Refresh exchanges a refresh token for a fresh access token. Kiro
// sometimes rotates the refresh token itself — when RefreshToken in the
// response is non-empty, callers must persist it and replace the stored
// one. Losing that rotation bricks future refreshes.
func Refresh(ctx context.Context, refreshToken string) (*RefreshResponse, error) {
	trimmed := strings.TrimSpace(refreshToken)
	if trimmed == "" {
		return nil, fmt.Errorf("kiro refresh: empty refresh token")
	}

	body, err := json.Marshal(map[string]string{"refreshToken": trimmed})
	if err != nil {
		return nil, fmt.Errorf("kiro refresh: encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", RefreshTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("kiro refresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro refresh: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kiro refresh: status %d", resp.StatusCode)
	}

	var out RefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("kiro refresh: decode: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("kiro refresh: empty access token in response")
	}

	// Normalize expiry: prefer absolute ExpiresAt, fall back to
	// relative ExpiresIn + now.
	if out.ExpiresAt == 0 && out.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Unix() + out.ExpiresIn
	}
	if out.Region == "" {
		out.Region = DefaultRegion
	}
	return &out, nil
}

// LoginURL builds the browser URL users open to start a Kiro login flow.
// The state parameter is echoed back in the redirect so the local
// callback server can match responses to sessions.
func LoginURL(state string) string {
	// Kiro's auth endpoint expects simple query-string state; no PKCE is
	// documented on the public flow (yet). This mirrors what the desktop
	// client does.
	return fmt.Sprintf("%s?state=%s", LoginStartURL, state)
}
