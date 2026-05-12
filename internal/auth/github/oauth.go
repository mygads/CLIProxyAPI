package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Public OAuth client ID used by the GitHub Copilot CLI integrations. This is
// not a secret — it is baked into the public VS Code Copilot extension and
// shipped to every user. Overridable via env so enterprise deployments can
// point at their own OAuth app.
//
// Reference: github.com/microsoft/vscode-copilot-release (Client IDs table).
const (
	DefaultClientID        = "Iv1.b507a08c87ecfe98"
	DeviceCodeURL          = "https://github.com/login/device/code"
	DeviceTokenURL         = "https://github.com/login/oauth/access_token"
	DefaultScope           = "read:user"
	DeviceGrantType        = "urn:ietf:params:oauth:grant-type:device_code"
	CopilotTokenExchangeURL = "https://api.github.com/copilot_internal/v2/token"
	UserInfoURL             = "https://api.github.com/user"

	// UserAgent mimics the upstream Copilot CLI so GitHub does not gate us.
	// Bumping this string is cheap — if GitHub rejects future requests, the
	// first thing to try is matching the current VS Code Copilot UA.
	UserAgent = "GitHubCopilotChat/0.26.0"
)

// DeviceCode is the response from POST /login/device/code. The fields
// mirror the RFC 8628 shape so the management UI can render the
// verification URL + user code directly.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// tokenResp covers both the "pending" and the "success" shapes that GitHub
// emits from /login/oauth/access_token. It also surfaces slow_down /
// authorization_pending as first-class errors via the Error field.
type tokenResp struct {
	AccessToken      string `json:"access_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	Scope            string `json:"scope,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	// GitHub documents `interval` growth for slow_down responses. We honor
	// the server-supplied value rather than doing our own backoff math.
	Interval int `json:"interval,omitempty"`
}

// CopilotTokenResponse is the shape of POST /copilot_internal/v2/token.
// Only the fields we use are typed — Copilot's response has many more
// capability flags, and we keep the raw endpoints map for future use.
type CopilotTokenResponse struct {
	Token       string            `json:"token"`
	ExpiresAt   int64             `json:"expires_at"`
	RefreshIn   int               `json:"refresh_in"`
	Endpoints   map[string]string `json:"endpoints"`
	ChatEnabled bool              `json:"chat_enabled"`
}

// StartDeviceFlow begins the GitHub OAuth device-code flow. The returned
// DeviceCode is handed to the user (verification_uri + user_code), while
// the device_code string is passed back to PollDeviceFlow.
//
// The clientID parameter is optional — DefaultClientID is used when blank.
func StartDeviceFlow(ctx context.Context, clientID string) (*DeviceCode, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = DefaultClientID
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", DefaultScope)

	req, err := http.NewRequestWithContext(ctx, "POST", DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("github device: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github device: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github device: status %d", resp.StatusCode)
	}
	var dc DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, fmt.Errorf("github device: decode: %w", err)
	}
	if dc.DeviceCode == "" {
		return nil, fmt.Errorf("github device: empty device_code in response")
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc, nil
}

// PollDeviceFlow exchanges a device_code for an access_token. It loops
// honoring the server-supplied interval (including slow_down), respects
// the passed-in context for cancellation, and gives up when the device
// code expires (deadline set by the caller via context.WithTimeout).
//
// Returns the access token on success, or a typed error on authorization
// decline / expiry / unexpected server error.
func PollDeviceFlow(ctx context.Context, clientID, deviceCode string, interval time.Duration) (string, error) {
	if strings.TrimSpace(clientID) == "" {
		clientID = DefaultClientID
	}
	if interval < time.Second {
		interval = 5 * time.Second
	}

	// Small initial delay: GitHub rejects a poll issued in the first ~second
	// after device_code with slow_down. Paying the interval up front keeps
	// the loop body simple.
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}

		tok, backoff, err := pollOnce(ctx, clientID, deviceCode)
		if err != nil {
			return "", err
		}
		if tok != "" {
			return tok, nil
		}
		// Honor server-supplied slow_down; otherwise keep the original cadence.
		next := interval
		if backoff > 0 {
			next = backoff
		}
		timer.Reset(next)
	}
}

// pollOnce issues a single token poll. The second return value is a
// server-requested backoff duration (slow_down) which is zero when the
// original interval should be reused. The error is non-nil only for
// terminal conditions (access_denied, expired_token, transport fail).
func pollOnce(ctx context.Context, clientID, deviceCode string) (token string, backoff time.Duration, err error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", DeviceGrantType)

	req, errReq := http.NewRequestWithContext(ctx, "POST", DeviceTokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return "", 0, fmt.Errorf("github poll: build request: %w", errReq)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", UserAgent)

	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		return "", 0, fmt.Errorf("github poll: do: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr tokenResp
	if errDec := json.NewDecoder(resp.Body).Decode(&tr); errDec != nil {
		return "", 0, fmt.Errorf("github poll: decode: %w", errDec)
	}
	if tr.AccessToken != "" {
		return tr.AccessToken, 0, nil
	}
	switch tr.Error {
	case "authorization_pending":
		// User has not approved yet. Keep polling at the original cadence.
		return "", 0, nil
	case "slow_down":
		// Server asked us to back off. Respect its interval.
		if tr.Interval > 0 {
			return "", time.Duration(tr.Interval) * time.Second, nil
		}
		return "", 10 * time.Second, nil
	case "expired_token":
		return "", 0, fmt.Errorf("github poll: device code expired, restart the login flow")
	case "access_denied":
		return "", 0, fmt.Errorf("github poll: user denied the authorization request")
	case "":
		return "", 0, fmt.Errorf("github poll: empty access_token without error (status %d)", resp.StatusCode)
	default:
		desc := tr.ErrorDescription
		if desc == "" {
			desc = tr.Error
		}
		return "", 0, fmt.Errorf("github poll: %s: %s", tr.Error, desc)
	}
}

// ExchangeCopilotToken trades a long-lived GitHub access token for a
// short-lived Copilot bearer token. The Copilot API expects this exact
// UA + auth header combination.
func ExchangeCopilotToken(ctx context.Context, githubAccessToken string) (*CopilotTokenResponse, error) {
	if strings.TrimSpace(githubAccessToken) == "" {
		return nil, fmt.Errorf("github copilot: empty github access token")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", CopilotTokenExchangeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("github copilot: build request: %w", err)
	}
	// GitHub OAuth tokens use the `token` auth scheme, not `Bearer`, on the
	// REST API. The Copilot endpoint enforces this.
	req.Header.Set("Authorization", "token "+githubAccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Editor-Version", "vscode/1.99.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github copilot: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github copilot: token exchange status %d", resp.StatusCode)
	}
	var out CopilotTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github copilot: decode: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("github copilot: empty token in exchange response")
	}
	return &out, nil
}

// FetchLogin returns the GitHub login (username) for the given access
// token. Informational only — the proxy stores it so operators can tell
// credentials apart in the admin UI. A failure here is non-fatal; callers
// should proceed with an empty login string.
func FetchLogin(ctx context.Context, githubAccessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", UserInfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+githubAccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github user: status %d", resp.StatusCode)
	}
	var payload struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.Login, nil
}
