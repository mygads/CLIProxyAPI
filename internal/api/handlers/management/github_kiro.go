package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	githubauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/github"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RequestGitHubToken starts a GitHub Copilot OAuth device-code flow.
//
// GitHub device-code differs from the browser-redirect OAuth used by
// Claude/Codex: instead of redirecting to a callback, GitHub returns a
// user_code + verification_uri that the operator enters manually at
// github.com/login/device. This handler kicks off the flow, returns the
// verification URL so the admin UI can display it, then polls GitHub in
// the background until the user completes verification.
//
// On successful verification, the GitHub access token is exchanged for a
// short-lived Copilot token. The GitHub access token is stored so we can
// mint fresh Copilot tokens on demand (Copilot tokens expire ~30 min).
//
// NOTE: The executor body for GitHub Copilot is still a 501 stub —
// registering credentials here is still valuable because it lets the
// executor work land as a pure add without needing fresh OAuth wiring.
func (h *Handler) RequestGitHubToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	state := fmt.Sprintf("gh-%d", time.Now().UnixNano())

	device, err := githubauth.StartDeviceFlow(ctx, "")
	if err != nil {
		log.Errorf("GitHub device flow start failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start device flow"})
		return
	}

	RegisterOAuthSession(state, "github")

	go func() {
		interval := time.Duration(device.Interval) * time.Second
		if interval <= 0 {
			interval = 5 * time.Second
		}
		// PollDeviceFlow blocks until GitHub returns a token or
		// authorization expires.
		accessToken, errWait := githubauth.PollDeviceFlow(ctx, "", device.DeviceCode, interval)
		if errWait != nil {
			SetOAuthSessionError(state, fmt.Sprintf("authorization failed: %v", errWait))
			return
		}

		// Exchange for a Copilot token up-front so we fail fast if the
		// account is not Copilot-enabled.
		copilotTok, errCopilot := githubauth.ExchangeCopilotToken(ctx, accessToken)
		if errCopilot != nil {
			SetOAuthSessionError(state, fmt.Sprintf("copilot exchange failed: %v", errCopilot))
			return
		}

		// Best-effort login name for the admin UI. Failure here is
		// non-fatal — the token still works without a label.
		login, _ := githubauth.FetchLogin(ctx, accessToken)
		label := "GitHub Copilot"
		if login != "" {
			label = fmt.Sprintf("GitHub Copilot (%s)", login)
		}

		storage := &githubauth.GitHubTokenStorage{
			AccessToken:   accessToken,
			CopilotToken:  copilotTok.Token,
			CopilotExpire: copilotTok.ExpiresAt,
			Endpoints:     copilotTok.Endpoints,
			Login:         login,
			Type:          "github",
			LastRefresh:   time.Now().UTC().Format(time.RFC3339),
		}

		metadata := map[string]any{
			"type":               "github",
			"access_token":       accessToken,
			"copilot_token":      copilotTok.Token,
			"copilot_expires_at": copilotTok.ExpiresAt,
			"login":              login,
			"timestamp":          time.Now().UnixMilli(),
		}

		fileName := fmt.Sprintf("github-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "github",
			FileName: fileName,
			Label:    label,
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("save github token: %v", errSave)
			SetOAuthSessionError(state, "failed to persist tokens")
			return
		}
		log.Infof("GitHub Copilot credential saved to %s", savedPath)
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("github")
	}()

	// Device flow needs BOTH the URL and the user_code — the admin UI
	// displays the code next to a "copy" button that links to
	// verification_uri.
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"url":              device.VerificationURI,
		"verification_uri": device.VerificationURI,
		"user_code":        device.UserCode,
		"state":            state,
		"expires_in":       device.ExpiresIn,
		"interval":         device.Interval,
	})
}

// RequestKiroToken starts a Kiro OAuth flow.
//
// Kiro uses a browser-based login at its auth endpoint. The /kiro/callback
// route in server.go writes the refresh token to a pending-session file
// on disk; this handler watches for that file, then swaps the refresh
// token for an access token to validate the credential before storing it.
//
// NOTE: Like GitHub, the executor body for Kiro is still a 501 stub
// (AWS eventstream decoding remains). Registering credentials here is
// still useful — the executor work can land as a pure add.
func (h *Handler) RequestKiroToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	state := fmt.Sprintf("kr-%d", time.Now().UnixNano())
	authURL := kiroauth.LoginURL(state)

	RegisterOAuthSession(state, "kiro")

	go func() {
		// Wait up to 10 minutes for the operator to complete the browser
		// login. The callback route writes a pending-session file we
		// watch for here.
		callbackCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		payload, errWait := waitForPendingOAuthCallback(callbackCtx, h.cfg.AuthDir, "kiro", state)
		if errWait != nil {
			SetOAuthSessionError(state, fmt.Sprintf("authorization failed: %v", errWait))
			return
		}
		if payload.Error != "" {
			SetOAuthSessionError(state, fmt.Sprintf("kiro returned error: %s", payload.Error))
			return
		}

		refreshToken := strings.TrimSpace(payload.Code)
		if refreshToken == "" {
			SetOAuthSessionError(state, "kiro callback missing refresh token")
			return
		}

		// Exchange the refresh token for an access token so we know the
		// credential is valid before we store it.
		refreshResp, errRefresh := kiroauth.Refresh(ctx, refreshToken)
		if errRefresh != nil {
			log.Errorf("kiro initial refresh failed: %v", errRefresh)
			SetOAuthSessionError(state, "initial token refresh failed")
			return
		}

		// Kiro may rotate the refresh token on first use.
		storedRefresh := refreshToken
		if rotated := strings.TrimSpace(refreshResp.RefreshToken); rotated != "" {
			storedRefresh = rotated
		}

		storage := &kiroauth.KiroTokenStorage{
			AccessToken:  refreshResp.AccessToken,
			RefreshToken: storedRefresh,
			Expire:       refreshResp.ExpiresAt,
			Region:       refreshResp.Region,
			ProfileArn:   refreshResp.ProfileArn,
			Email:        refreshResp.Email,
			Type:         "kiro",
			LastRefresh:  time.Now().UTC().Format(time.RFC3339),
		}

		metadata := map[string]any{
			"type":          "kiro",
			"access_token":  refreshResp.AccessToken,
			"refresh_token": storedRefresh,
			"expires_at":    refreshResp.ExpiresAt,
			"region":        refreshResp.Region,
			"email":         refreshResp.Email,
			"timestamp":     time.Now().UnixMilli(),
		}

		label := "Kiro AI"
		if refreshResp.Email != "" {
			label = fmt.Sprintf("Kiro AI (%s)", refreshResp.Email)
		}

		fileName := fmt.Sprintf("kiro-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kiro",
			FileName: fileName,
			Label:    label,
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("save kiro token: %v", errSave)
			SetOAuthSessionError(state, "failed to persist tokens")
			return
		}
		log.Infof("Kiro credential saved to %s", savedPath)
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("kiro")
	}()

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"url":    authURL,
		"state":  state,
	})
}

// ImportKiroToken validates a manually-provided Kiro refresh token and
// saves it as a credential. This mirrors the "Import Token" flow in
// 9router where the operator pastes a refresh token (starting with
// "aorAAAAAG") directly instead of going through the browser OAuth flow.
func (h *Handler) ImportKiroToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	refreshToken := strings.TrimSpace(body.RefreshToken)
	if refreshToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refresh_token is required"})
		return
	}

	if !strings.HasPrefix(refreshToken, "aorAAAAAG") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token format — Kiro refresh tokens start with aorAAAAAG"})
		return
	}

	refreshResp, err := kiroauth.Refresh(ctx, refreshToken)
	if err != nil {
		log.Errorf("kiro import token refresh failed: %v", err)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": fmt.Sprintf("token validation failed: %v", err)})
		return
	}

	storedRefresh := refreshToken
	if rotated := strings.TrimSpace(refreshResp.RefreshToken); rotated != "" {
		storedRefresh = rotated
	}

	storage := &kiroauth.KiroTokenStorage{
		AccessToken:  refreshResp.AccessToken,
		RefreshToken: storedRefresh,
		Expire:       refreshResp.ExpiresAt,
		Region:       refreshResp.Region,
		ProfileArn:   refreshResp.ProfileArn,
		Email:        refreshResp.Email,
		Type:         "kiro",
		LastRefresh:  time.Now().UTC().Format(time.RFC3339),
	}

	metadata := map[string]any{
		"type":          "kiro",
		"access_token":  refreshResp.AccessToken,
		"refresh_token": storedRefresh,
		"expires_at":    refreshResp.ExpiresAt,
		"region":        refreshResp.Region,
		"email":         refreshResp.Email,
		"timestamp":     time.Now().UnixMilli(),
	}

	label := "Kiro AI (imported)"
	if refreshResp.Email != "" {
		label = fmt.Sprintf("Kiro AI (%s)", refreshResp.Email)
	}

	fileName := fmt.Sprintf("kiro-%d.json", time.Now().UnixMilli())
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "kiro",
		FileName: fileName,
		Label:    label,
		Storage:  storage,
		Metadata: metadata,
	}
	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		log.Errorf("save kiro imported token: %v", errSave)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist token"})
		return
	}
	log.Infof("Kiro credential (imported) saved to %s", savedPath)

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"label":  label,
		"email":  refreshResp.Email,
	})
}

// RequestKiroDeviceAuth starts an AWS Builder ID device-code flow for
// Kiro. Mirrors RequestGitHubToken structurally:
//
//   1. RegisterSSOClient → get a fresh OIDC clientId/clientSecret pair.
//   2. StartSSODeviceAuthorization → get user_code + verification URL.
//   3. Spawn a goroutine that polls /token until AWS returns tokens or
//      the 10-minute deadline elapses.
//   4. On success, persist a KiroTokenStorage with auth_method="builder-id"
//      AND provider_specific_data.{clientId,clientSecret,region,startUrl}
//      so RefreshIfExpired can rotate via the SSO /token endpoint.
//
// This replaces the broken browser-login URL flow that the legacy
// /kiro-auth-url endpoint emits — that URL is for Kiro IDE's desktop
// app callback (kiro://...) and rejects browser opens with a Cognito
// PKCE validation error.
func (h *Handler) RequestKiroDeviceAuth(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	region := kiroauth.DefaultRegion
	startURL := kiroauth.SSOStartURL

	registered, err := kiroauth.RegisterSSOClient(ctx, region)
	if err != nil {
		log.Errorf("kiro device auth: register client: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("AWS SSO client registration failed: %v", err)})
		return
	}

	device, err := kiroauth.StartSSODeviceAuthorization(ctx, region, registered.ClientID, registered.ClientSecret, startURL)
	if err != nil {
		log.Errorf("kiro device auth: start authorization: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("AWS SSO device authorization failed: %v", err)})
		return
	}

	state := fmt.Sprintf("kr-%d", time.Now().UnixNano())
	RegisterOAuthSession(state, "kiro")

	go func() {
		// 10-minute deadline matches the GitHub flow. AWS device codes
		// typically expire in 5–10 min; we honor the server-supplied
		// expires_in if it's tighter.
		timeout := 10 * time.Minute
		if device.ExpiresIn > 0 && time.Duration(device.ExpiresIn)*time.Second < timeout {
			timeout = time.Duration(device.ExpiresIn) * time.Second
		}
		pollCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		interval := time.Duration(device.Interval) * time.Second
		if interval < time.Second {
			interval = 5 * time.Second
		}

		var tokenResp *kiroauth.SSODeviceTokenResponse
		for {
			select {
			case <-pollCtx.Done():
				SetOAuthSessionError(state, "device authorization timed out — restart the login flow")
				return
			case <-time.After(interval):
			}

			resp, pollErr := kiroauth.PollSSODeviceToken(pollCtx, region, registered.ClientID, registered.ClientSecret, device.DeviceCode)
			if pollErr == nil && resp != nil && resp.AccessToken != "" {
				tokenResp = resp
				break
			}
			// Sentinel errors → keep polling. Other errors → terminal.
			if errors.Is(pollErr, kiroauth.ErrSSODeviceAuthPending) {
				continue
			}
			if errors.Is(pollErr, kiroauth.ErrSSODeviceAuthSlowDown) {
				// AWS asks us to back off by the device interval. Bump
				// the cadence by 5s as the spec recommends.
				interval += 5 * time.Second
				continue
			}
			SetOAuthSessionError(state, fmt.Sprintf("authorization failed: %v", pollErr))
			return
		}

		// Build storage. Builder-ID-minted credentials carry their
		// {clientId,clientSecret,region,startUrl} in metadata so the
		// refresh helper can rotate via SSO /token instead of the
		// social-auth /refreshToken endpoint.
		now := time.Now()
		expiresAt := int64(0)
		if tokenResp.ExpiresIn > 0 {
			expiresAt = now.Unix() + tokenResp.ExpiresIn
		}

		storage := &kiroauth.KiroTokenStorage{
			AccessToken:  tokenResp.AccessToken,
			RefreshToken: strings.TrimSpace(tokenResp.RefreshToken),
			Expire:       expiresAt,
			Region:       region,
			Type:         "kiro",
			AuthMethod:   "builder-id",
			LastRefresh:  now.UTC().Format(time.RFC3339),
		}

		metadata := map[string]any{
			"type":          "kiro",
			"access_token":  tokenResp.AccessToken,
			"refresh_token": strings.TrimSpace(tokenResp.RefreshToken),
			"expires_at":    expiresAt,
			"region":        region,
			"auth_method":   "builder-id",
			"timestamp":     now.UnixMilli(),
			"provider_specific_data": map[string]any{
				"clientId":     registered.ClientID,
				"clientSecret": registered.ClientSecret,
				"region":       region,
				"startUrl":     startURL,
				"authMethod":   "builder-id",
			},
		}

		label := "Kiro AI (Builder ID)"

		fileName := fmt.Sprintf("kiro-%d.json", now.UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kiro",
			FileName: fileName,
			Label:    label,
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("save kiro device-auth token: %v", errSave)
			SetOAuthSessionError(state, "failed to persist tokens")
			return
		}
		log.Infof("Kiro Builder ID credential saved to %s", savedPath)
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("kiro")
	}()

	// Tell the FE to render the device-code block (user_code + URL).
	// The shape mirrors what RequestGitHubToken returns so the existing
	// DEVICE_CODE_PROVIDERS rendering works without further changes.
	c.JSON(http.StatusOK, gin.H{
		"status":           "ok",
		"url":              firstNonEmpty(device.VerificationURIComplete, device.VerificationURI),
		"verification_uri": device.VerificationURI,
		"user_code":        device.UserCode,
		"state":            state,
		"expires_in":       device.ExpiresIn,
		"interval":         device.Interval,
	})
}

// firstNonEmpty returns the first non-empty trimmed string from values.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// waitForPendingOAuthCallback polls the auth directory for the callback
// file written by WriteOAuthCallbackFileForPendingSession. The file is
// removed as soon as it is read so a subsequent run with the same state
// does not see stale data.
//
// This mirrors the pattern used by other OAuth handlers (Claude, Codex,
// Gemini, Antigravity) — they all poll for the same ".oauth-{provider}-{state}.oauth"
// file, but the poll loop was previously inlined into each handler. We
// extract it here so GitHub and Kiro can reuse it cleanly.
func waitForPendingOAuthCallback(ctx context.Context, authDir, provider, state string) (*oauthCallbackFilePayload, error) {
	canonical, err := NormalizeOAuthProvider(provider)
	if err != nil {
		return nil, err
	}
	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", canonical, state)
	path := filepath.Join(authDir, fileName)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				if os.IsNotExist(readErr) {
					continue
				}
				return nil, fmt.Errorf("read callback file: %w", readErr)
			}
			_ = os.Remove(path)
			var payload oauthCallbackFilePayload
			if err := json.Unmarshal(data, &payload); err != nil {
				return nil, fmt.Errorf("parse callback file: %w", err)
			}
			return &payload, nil
		}
	}
}
