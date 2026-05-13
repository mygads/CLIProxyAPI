// Package qwen provides OAuth (device-code + PKCE) and token-refresh
// plumbing for the Qwen Code provider. Values below mirror OmniRoute's
// src/lib/oauth/constants/oauth.ts (QWEN_CONFIG) and
// open-sse/config/providerHeaderProfiles.ts (2026-05).
//
// Qwen uses the standard device-code flow, BUT requires PKCE (S256) on
// top of it — the device_code request must include code_challenge, and
// the token poll must present the matching code_verifier. The client_id
// is public, baked into the Qwen Code CLI.
package qwen

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultClientID is the public OAuth client baked into the Qwen
	// Code CLI. Override via env for enterprise deployments pointing at
	// their own OAuth app.
	DefaultClientID = "f0304373b74a44d2b584a3fb70ca9e56"

	DeviceCodeURL = "https://chat.qwen.ai/api/v1/oauth2/device/code"
	TokenURL      = "https://chat.qwen.ai/api/v1/oauth2/token"

	DefaultScope         = "openid profile email model.completion"
	CodeChallengeMethod  = "S256"
	DeviceGrantType      = "urn:ietf:params:oauth:grant-type:device_code"
	RefreshGrantType     = "refresh_token"

	// Fingerprint values from OmniRoute's providerHeaderProfiles.ts.
	// CLIVersion tracks the public Qwen Code release so our UA matches
	// what the edge gates on. Bump in lockstep with OmniRoute.
	CLIVersion              = "0.15.9"
	StainlessLang           = "js"
	StainlessPackageVersion = "5.11.0"
	StainlessRetryCount     = "1"
	StainlessRuntime        = "node"

	// Static Dashscope headers the edge expects on OAuth-authenticated
	// calls. A missing X-Dashscope-AuthType gets a 401.
	DashscopeAuthTypeHeader  = "X-Dashscope-AuthType"
	DashscopeAuthTypeValue   = "qwen-oauth"
	DashscopeCacheCtrlHeader = "X-Dashscope-CacheControl"
	DashscopeCacheCtrlValue  = "enable"

	// DefaultBaseURL is the Qwen Code chat endpoint. When the token
	// response carries a resource_url, callers should prefer that.
	DefaultBaseURL = "https://chat.qwen.ai/api/v1/services/aigc/text-generation/generation"
)

// DeviceCode is the response shape from POST /oauth2/device/code.
// Mirrors RFC 8628 with the addition of Qwen's verification_uri_complete.
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse covers both success and pending-error shapes returned
// from POST /oauth2/token. Qwen returns the resource_url with the token
// so the executor can point at the user's assigned region.
type TokenResponse struct {
	AccessToken      string `json:"access_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	IDToken          string `json:"id_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	Scope            string `json:"scope,omitempty"`
	ResourceURL      string `json:"resource_url,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// PKCE carries the verifier/challenge pair for a single login session.
// Callers keep the verifier in memory until PollDeviceFlow completes.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a fresh S256 PKCE pair. The verifier is 32 bytes of
// URL-safe random data (matches Qwen Code CLI's own implementation).
func NewPKCE() (*PKCE, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("qwen pkce: read random: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return &PKCE{Verifier: verifier, Challenge: challenge}, nil
}

// StartDeviceFlow begins a device-code login with the supplied PKCE
// challenge. clientID is optional (DefaultClientID when blank). The
// returned DeviceCode is shown to the user; the device_code is used to
// poll for the token.
func StartDeviceFlow(ctx context.Context, clientID string, pkce *PKCE) (*DeviceCode, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = DefaultClientID
	}
	if pkce == nil || pkce.Challenge == "" {
		return nil, fmt.Errorf("qwen device: nil or empty PKCE challenge")
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", DefaultScope)
	form.Set("code_challenge", pkce.Challenge)
	form.Set("code_challenge_method", CodeChallengeMethod)

	req, err := http.NewRequestWithContext(ctx, "POST", DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("qwen device: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qwen device: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qwen device: status %d", resp.StatusCode)
	}
	var dc DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, fmt.Errorf("qwen device: decode: %w", err)
	}
	if dc.DeviceCode == "" {
		return nil, fmt.Errorf("qwen device: empty device_code in response")
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc, nil
}

// PollDeviceFlow loops on POST /oauth2/token until the user completes
// authorization, the device_code expires, or ctx is cancelled. Returns
// a full TokenResponse on success so callers can persist refresh_token
// and resource_url.
func PollDeviceFlow(ctx context.Context, clientID, deviceCode string, pkce *PKCE, interval time.Duration) (*TokenResponse, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = DefaultClientID
	}
	if pkce == nil || pkce.Verifier == "" {
		return nil, fmt.Errorf("qwen poll: nil or empty PKCE verifier")
	}
	if interval < time.Second {
		interval = 5 * time.Second
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		tok, backoff, err := pollOnce(ctx, clientID, deviceCode, pkce.Verifier)
		if err != nil {
			return nil, err
		}
		if tok != nil && tok.AccessToken != "" {
			return tok, nil
		}
		next := interval
		if backoff > 0 {
			next = backoff
		}
		timer.Reset(next)
	}
}

// pollOnce issues one token-poll request. Terminal errors return err
// non-nil; authorization_pending returns a zero token with no error;
// slow_down returns a backoff duration.
func pollOnce(ctx context.Context, clientID, deviceCode, verifier string) (*TokenResponse, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", DeviceGrantType)
	form.Set("client_id", clientID)
	form.Set("device_code", deviceCode)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, fmt.Errorf("qwen poll: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("qwen poll: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, 0, fmt.Errorf("qwen poll: decode: %w", err)
	}
	if tr.AccessToken != "" {
		return &tr, 0, nil
	}
	switch tr.Error {
	case "authorization_pending":
		return nil, 0, nil
	case "slow_down":
		return nil, 5 * time.Second, nil
	case "expired_token":
		return nil, 0, fmt.Errorf("qwen poll: device code expired, restart login")
	case "access_denied":
		return nil, 0, fmt.Errorf("qwen poll: user denied authorization")
	case "":
		return nil, 0, fmt.Errorf("qwen poll: empty token without error (status %d)", resp.StatusCode)
	default:
		desc := tr.ErrorDescription
		if desc == "" {
			desc = tr.Error
		}
		return nil, 0, fmt.Errorf("qwen poll: %s: %s", tr.Error, desc)
	}
}

// Refresh exchanges a refresh_token for a fresh access_token. Qwen
// sometimes rotates the refresh_token on refresh — callers must persist
// the rotated value if TokenResponse.RefreshToken is non-empty.
func Refresh(ctx context.Context, clientID, refreshToken string) (*TokenResponse, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = DefaultClientID
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("qwen refresh: empty refresh_token")
	}
	form := url.Values{}
	form.Set("grant_type", RefreshGrantType)
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("qwen refresh: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qwen refresh: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("qwen refresh: decode: %w", err)
	}
	if tr.Error != "" {
		desc := tr.ErrorDescription
		if desc == "" {
			desc = tr.Error
		}
		return nil, fmt.Errorf("qwen refresh: %s: %s", tr.Error, desc)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("qwen refresh: empty access_token (status %d)", resp.StatusCode)
	}
	return &tr, nil
}

// UserAgent returns the OmniRoute-compatible UA for Qwen Code traffic.
// Uses the default CLI version; override by setting QWEN_CLI_VERSION in
// the env and rebuilding.
func UserAgent() string {
	return fmt.Sprintf("QwenCode/%s (%s; %s)", CLIVersion, "linux", "x64")
}
