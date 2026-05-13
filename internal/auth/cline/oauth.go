// Package cline provides the authorization-code OAuth plumbing for the
// Cline Bot provider. Endpoint constants mirror OmniRoute's CLINE_CONFIG
// in src/lib/oauth/constants/oauth.ts (2026-05).
//
// Flow quirk: Cline's "authorization code" is actually the OAuth state
// callback with a base64-encoded JSON blob containing the tokens baked
// in. So the exchange is "decode base64; parse JSON". When the embedded
// decode fails we fall back to a server-side token exchange against
// /api/v1/auth/token. Subsequent refreshes hit /api/v1/auth/refresh.
package cline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	AppBaseURL       = "https://app.cline.bot"
	APIBaseURL       = "https://api.cline.bot"
	AuthorizeURL     = "https://api.cline.bot/api/v1/auth/authorize"
	TokenExchangeURL = "https://api.cline.bot/api/v1/auth/token"
	RefreshURL       = "https://api.cline.bot/api/v1/auth/refresh"

	// ChatCompletionsURL is the OpenAI-compatible chat endpoint.
	ChatCompletionsURL = "https://api.cline.bot/api/v1/chat/completions"

	// ClientType is the only value Cline's edge accepts for OAuth
	// traffic coming from third-party integrations.
	ClientType = "extension"

	// RefererHeader/TitleHeader are the OpenRouter-style attribution
	// headers Cline's edge gates on — a missing X-Title gets a silent
	// 429 in the quota-check path.
	RefererHeader = "HTTP-Referer"
	RefererValue  = "https://cline.bot"
	TitleHeader   = "X-Title"
	TitleValue    = "Cline"
)

// TokenBundle is the normalized shape we hand back from either the
// embedded-base64 extraction or the HTTP token-exchange path. ExpiresAt
// is a best-effort absolute Unix timestamp — Cline's embedded payload
// uses ISO-8601 so callers parse it at save time.
type TokenBundle struct {
	AccessToken  string
	RefreshToken string
	Email        string
	FirstName    string
	LastName     string
	ExpiresAt    string // ISO-8601 or unix seconds, caller normalizes
}

// LoginURL builds the browser URL users open to start a Cline login
// flow. redirectURI is the address the OAuth callback will redirect to
// — typically the proxy's /cline/callback.
func LoginURL(redirectURI string) string {
	params := url.Values{}
	params.Set("client_type", ClientType)
	params.Set("callback_url", redirectURI)
	params.Set("redirect_uri", redirectURI)
	return AuthorizeURL + "?" + params.Encode()
}

// ExchangeToken extracts tokens from a Cline callback code. It first
// tries the embedded-base64 path (Cline inlines the tokens in the code
// parameter); on failure it falls back to a server-side token exchange
// at /api/v1/auth/token. redirectURI must match the URL passed to
// LoginURL so the exchange endpoint accepts the request.
func ExchangeToken(ctx context.Context, code, redirectURI string) (*TokenBundle, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("cline exchange: empty code")
	}

	if bundle, err := decodeEmbedded(code); err == nil {
		return bundle, nil
	}
	// Fallback: hit the token exchange endpoint.
	body := map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"client_type":  ClientType,
		"redirect_uri": redirectURI,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cline exchange: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", TokenExchangeURL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("cline exchange: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cline exchange: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cline exchange: status %d", resp.StatusCode)
	}

	var out struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    string `json:"expiresAt"`
		Data         struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    string `json:"expiresAt"`
			UserInfo     struct {
				Email string `json:"email"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("cline exchange: decode: %w", err)
	}
	access := firstNonEmpty(out.Data.AccessToken, out.AccessToken)
	refresh := firstNonEmpty(out.Data.RefreshToken, out.RefreshToken)
	expires := firstNonEmpty(out.Data.ExpiresAt, out.ExpiresAt)
	if access == "" {
		return nil, fmt.Errorf("cline exchange: empty accessToken")
	}
	return &TokenBundle{
		AccessToken:  access,
		RefreshToken: refresh,
		Email:        out.Data.UserInfo.Email,
		ExpiresAt:    expires,
	}, nil
}

// decodeEmbedded extracts tokens from a Cline auth code that has a
// base64-encoded JSON blob baked in. Matches OmniRoute's client-side
// decoder in providers/cline.ts exchangeToken.
func decodeEmbedded(code string) (*TokenBundle, error) {
	trimmed, _ := url.QueryUnescape(code)
	if trimmed == "" {
		trimmed = code
	}
	// Restore padding so std base64 accepts it.
	if m := len(trimmed) % 4; m != 0 {
		trimmed += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// URL-safe encoding as a fallback.
		raw, err = base64.URLEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("cline embed: base64 decode: %w", err)
		}
	}
	last := strings.LastIndexByte(string(raw), '}')
	if last < 0 {
		return nil, fmt.Errorf("cline embed: no JSON object in decoded code")
	}
	jsonStr := string(raw)[:last+1]
	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
		FirstName    string `json:"firstName"`
		LastName     string `json:"lastName"`
		ExpiresAt    string `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &payload); err != nil {
		return nil, fmt.Errorf("cline embed: parse JSON: %w", err)
	}
	if payload.AccessToken == "" {
		return nil, fmt.Errorf("cline embed: empty accessToken in payload")
	}
	return &TokenBundle{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		Email:        payload.Email,
		FirstName:    payload.FirstName,
		LastName:     payload.LastName,
		ExpiresAt:    payload.ExpiresAt,
	}, nil
}

// RefreshResponse models POST /api/v1/auth/refresh. Both camelCase and
// nested "data" shapes are supported (Cline is inconsistent).
type RefreshResponse struct {
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	Data         struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    string `json:"expiresAt"`
	} `json:"data"`
}

// Refresh exchanges a refresh_token for a new access_token. Cline may
// rotate the refresh token — callers must persist the rotated value.
func Refresh(ctx context.Context, refreshToken string) (*RefreshResponse, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("cline refresh: empty refresh_token")
	}
	body, err := json.Marshal(map[string]string{"refreshToken": refreshToken})
	if err != nil {
		return nil, fmt.Errorf("cline refresh: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", RefreshURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("cline refresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cline refresh: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cline refresh: status %d", resp.StatusCode)
	}
	var out RefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("cline refresh: decode: %w", err)
	}
	access := firstNonEmpty(out.Data.AccessToken, out.AccessToken)
	if access == "" {
		return nil, fmt.Errorf("cline refresh: empty accessToken in response")
	}
	// Normalize into the top-level fields so callers see one shape.
	if out.AccessToken == "" {
		out.AccessToken = out.Data.AccessToken
	}
	if out.RefreshToken == "" {
		out.RefreshToken = out.Data.RefreshToken
	}
	if out.ExpiresAt == "" {
		out.ExpiresAt = out.Data.ExpiresAt
	}
	return &out, nil
}

// ParseExpiresAt accepts Cline's two common formats (ISO-8601 string or
// Unix-seconds number-as-string) and returns an absolute Unix timestamp.
// Returns 0 when the input cannot be parsed — callers should treat that
// as "unknown" and refresh aggressively.
func ParseExpiresAt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	// Numeric fallback.
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
		if n > 1_000_000_000_000 {
			// Millis.
			return n / 1000
		}
		return n
	}
	return 0
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
