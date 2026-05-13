// Package kilocode provides the device-code OAuth plumbing for the
// KiloCode provider. KiloCode's flow is a simplified device-code
// variant: `POST /api/device-auth/codes` returns {code, verificationUrl},
// and `GET /api/device-auth/codes/{code}` is polled until the response
// payload has status="approved" with a token. There is NO refresh
// token — the access token is long-lived and persisted as-is.
//
// Values below mirror OmniRoute's KILOCODE_CONFIG in
// src/lib/oauth/constants/oauth.ts (2026-05).
package kilocode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	APIBaseURL     = "https://api.kilo.ai"
	InitiateURL    = "https://api.kilo.ai/api/device-auth/codes"
	PollURLBase    = "https://api.kilo.ai/api/device-auth/codes"
	ChatCompletionsURL = "https://api.kilo.ai/api/openrouter/chat/completions"
	ModelsURL      = "https://api.kilo.ai/api/openrouter/models"
)

// DeviceCode is the normalized shape returned by InitiateDeviceAuth.
// Matches the RFC-8628 field names so callers can treat it like the
// other device-code providers.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// InitiateDeviceAuth begins a KiloCode device auth flow. The returned
// DeviceCode is handed to the user (verification_uri is the URL they
// open), and device_code is used to poll for the token.
//
// KiloCode returns a single "code" field that serves BOTH as the
// device_code (polled) and as the user_code shown on the verification
// page. We populate both slots so the admin UI does not need special
// casing.
func InitiateDeviceAuth(ctx context.Context) (*DeviceCode, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", InitiateURL, nil)
	if err != nil {
		return nil, fmt.Errorf("kilocode device: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kilocode device: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("kilocode device: too many pending authorization requests — try again later")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("kilocode device: status %d", resp.StatusCode)
	}

	var raw struct {
		Code            string `json:"code"`
		VerificationURL string `json:"verificationUrl"`
		ExpiresIn       int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("kilocode device: decode: %w", err)
	}
	if raw.Code == "" {
		return nil, fmt.Errorf("kilocode device: empty code in response")
	}
	if raw.ExpiresIn == 0 {
		raw.ExpiresIn = 300
	}
	return &DeviceCode{
		DeviceCode:      raw.Code,
		UserCode:        raw.Code,
		VerificationURI: raw.VerificationURL,
		ExpiresIn:       raw.ExpiresIn,
		Interval:        3,
	}, nil
}

// TokenResult is what PollForApproval returns on success: the
// long-lived access token and the user's email (informational).
type TokenResult struct {
	AccessToken string
	UserEmail   string
}

// PollForApproval loops on GET /api/device-auth/codes/{deviceCode}
// until the response has status="approved" with a token, or a
// terminal error. 202 = pending (continue), 403 = denied, 410 =
// expired.
func PollForApproval(ctx context.Context, deviceCode string, interval time.Duration) (*TokenResult, error) {
	if strings.TrimSpace(deviceCode) == "" {
		return nil, fmt.Errorf("kilocode poll: empty device_code")
	}
	if interval < time.Second {
		interval = 3 * time.Second
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		tok, err := pollOnce(ctx, deviceCode)
		if err != nil {
			return nil, err
		}
		if tok != nil && tok.AccessToken != "" {
			return tok, nil
		}
		timer.Reset(interval)
	}
}

// pollOnce issues one GET against the poll endpoint. Returns nil token
// without error on pending, and a terminal error on denial/expiry.
func pollOnce(ctx context.Context, deviceCode string) (*TokenResult, error) {
	url := PollURLBase + "/" + deviceCode
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("kilocode poll: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kilocode poll: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil, nil
	case http.StatusForbidden:
		return nil, fmt.Errorf("kilocode poll: user denied authorization")
	case http.StatusGone:
		return nil, fmt.Errorf("kilocode poll: device code expired")
	case http.StatusOK:
		// continue below
	default:
		return nil, fmt.Errorf("kilocode poll: status %d", resp.StatusCode)
	}

	var payload struct {
		Status    string `json:"status"`
		Token     string `json:"token"`
		UserEmail string `json:"userEmail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("kilocode poll: decode: %w", err)
	}
	if payload.Status == "approved" && payload.Token != "" {
		return &TokenResult{AccessToken: payload.Token, UserEmail: payload.UserEmail}, nil
	}
	return nil, nil
}
