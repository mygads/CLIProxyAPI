package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kiro endpoints reverse-engineered from the Kiro desktop app. Values
// mirror OmniRoute's src/lib/oauth/constants/oauth.ts (2026-05). The
// client IDs in this file are NOT present — the "SSO OIDC" flow uses a
// dynamically-registered client (POST /client/register) whose id+secret
// are stored in auth.Metadata.providerSpecificData. The "social" flow is
// a plain refresh-token-in/refresh-token-out exchange at auth.desktop.kiro.dev.
//
// Two distinct refresh paths exist and we support both:
//
//  1. SSO OIDC  — when auth.Metadata holds clientId + clientSecret from a
//     prior registerClient call, we POST to oidc.us-east-1.amazonaws.com/token.
//  2. Social    — otherwise we POST the single-field body to
//     auth.desktop.kiro.dev/refreshToken.
//
// The logic for choosing between them lives in the executor; this file
// only exposes the two transport functions.
const (
	DefaultRegion = "us-east-1"

	// Social auth (desktop Kiro OAuth). This is the default path used
	// when a credential was created by the browser login flow.
	AuthBaseURL     = "https://prod.us-east-1.auth.desktop.kiro.dev"
	RefreshTokenURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	// LoginStartURL is the browser entry point for the Kiro OAuth flow.
	// Kiro redirects back to a localhost callback with the refresh token
	// in the fragment. The kiro:// redirect_uri is the desktop-app scheme;
	// for a web proxy we substitute a localhost callback at runtime.
	LoginStartURL      = "https://prod.us-east-1.auth.desktop.kiro.dev/login"
	SocialTokenURL     = "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token"
	DesktopRedirectURI = "kiro://kiro.kiroAgent/authenticate-success"

	// SSO OIDC auth (AWS Identity Center). When present in credential
	// metadata, clientId + clientSecret prefer this path. Endpoints are
	// region-templated; DefaultRegion is used when Metadata.region is empty.
	SSOOIDCEndpointTemplate    = "https://oidc.%s.amazonaws.com"
	SSORegisterClientURLTpl    = "https://oidc.%s.amazonaws.com/client/register"
	SSODeviceAuthURLTpl        = "https://oidc.%s.amazonaws.com/device_authorization"
	SSOTokenURLTpl             = "https://oidc.%s.amazonaws.com/token"
	SSOClientName              = "kiro-oauth-client"
	SSOClientType              = "public"
	SSOIssuerURL               = "https://identitycenter.amazonaws.com/ssoins-722374e8c3c8e6c6"
	SSOStartURL                = "https://view.awsapps.com/start"
	SSOGrantTypeDeviceCode     = "urn:ietf:params:oauth:grant-type:device_code"
	SSOGrantTypeRefresh        = "refresh_token"
	SSOScopeCompletions        = "codewhisperer:completions"
	SSOScopeAnalysis           = "codewhisperer:analysis"
	SSOScopeConversations      = "codewhisperer:conversations"

	// CodeWhisperer runtime headers — mirror OmniRoute's
	// providerHeaderProfiles.ts (2026-05). UserAgent is the AWS SDK UA
	// shape that CodeWhisperer's edge expects; XAmzUserAgent is the
	// lowercase sibling that AWS SDK JS sends alongside it. A UA mismatch
	// here is the quickest way to get rate-limited.
	UserAgent           = "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0"
	XAmzUserAgent       = "aws-sdk-js/3.0.0 kiro-ide/1.0.0"
	XAmzTarget          = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	AnthropicBeta       = "prompt-caching-2024-07-31"
	BedrockCacheControl = "enable"
	AmzSdkRequest       = "attempt=1; max=3"
	AcceptEventStream   = "application/vnd.amazon.eventstream"

	// Imported-token sanity check. OmniRoute rejects any refresh token
	// that does not start with this prefix when the operator pastes one
	// into the management UI.
	ImportedTokenPrefix = "aorAAAAAG"

	// CodeWhispererEndpointTemplate is the regional CodeWhisperer host. The
	// quota probe (AmazonCodeWhispererService.ListAvailableModels) is a
	// POST to the bare host, while chat (generateAssistantResponse) lives
	// at /generateAssistantResponse on the same host.
	CodeWhispererEndpointTemplate = "https://codewhisperer.%s.amazonaws.com"
)

// CodeWhispererBaseURL builds the CodeWhisperer base URL for a region.
// Pass an empty string to fall back to DefaultRegion.
func CodeWhispererBaseURL(region string) string {
	if strings.TrimSpace(region) == "" {
		region = DefaultRegion
	}
	return fmt.Sprintf(CodeWhispererEndpointTemplate, region)
}

// SSOScopes is the scope list expected by the SSO OIDC registerClient
// call. Exposed as a slice so callers can feed it straight into a JSON
// body without string-splitting.
var SSOScopes = []string{SSOScopeCompletions, SSOScopeAnalysis, SSOScopeConversations}

// RefreshResponse models the subset of /refreshToken (social auth) we
// consume. ExpiresAt is the preferred absolute timestamp; ExpiresIn is a
// seconds-from-now fallback that Refresh normalizes.
type RefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	Region       string `json:"region,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	Email        string `json:"email,omitempty"`
}

// Refresh exchanges a refresh token for a fresh access token via the
// social-auth endpoint. Kiro sometimes rotates the refresh token on a
// refresh response — when RefreshToken in the response is non-empty,
// callers must persist it and replace the stored one, otherwise future
// refreshes break permanently.
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

// SSORefreshResponse models the AWS SSO OIDC /token response for a
// refresh_token grant. AWS uses snake_case here (unlike the social-auth
// endpoint which is camelCase).
type SSORefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	TokenType    string `json:"tokenType,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	// IdToken is returned for some grants but we don't consume it.
	IDToken string `json:"idToken,omitempty"`
}

// RefreshSSO uses the AWS SSO OIDC /token endpoint to refresh a Kiro
// credential minted via the SSO device-code flow. clientId + clientSecret
// come from a prior registerClient call (stored in credential metadata);
// passing the wrong pair returns InvalidClientException.
//
// The region defaults to DefaultRegion when empty — callers should pass
// the region recorded on the credential so enterprise SSO instances in
// non-us-east-1 regions work.
func RefreshSSO(ctx context.Context, region, clientID, clientSecret, refreshToken string) (*SSORefreshResponse, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	refreshToken = strings.TrimSpace(refreshToken)
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("kiro sso refresh: missing clientId/clientSecret")
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("kiro sso refresh: empty refresh token")
	}
	if strings.TrimSpace(region) == "" {
		region = DefaultRegion
	}

	url := fmt.Sprintf(SSOTokenURLTpl, region)
	reqBody, err := json.Marshal(map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    SSOGrantTypeRefresh,
	})
	if err != nil {
		return nil, fmt.Errorf("kiro sso refresh: encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("kiro sso refresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("X-Amz-User-Agent", XAmzUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro sso refresh: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kiro sso refresh: status %d", resp.StatusCode)
	}
	var out SSORefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("kiro sso refresh: decode: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("kiro sso refresh: empty access token in response")
	}
	return &out, nil
}

// LoginURL builds the browser URL users open to start a Kiro login flow.
// The state parameter is echoed back in the redirect so the local
// callback server can match responses to sessions.
func LoginURL(state string) string {
	return fmt.Sprintf("%s?state=%s", LoginStartURL, state)
}
