package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	qwenauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qwen"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RequestQwenToken starts a Qwen Code OAuth device-code flow with PKCE.
//
// Qwen's login UX differs slightly from GitHub's: the device endpoint
// returns a verification_uri plus a verification_uri_complete that
// bakes the user_code into the URL. The admin UI should open the
// _complete URL in a popup so the operator does not have to copy-paste
// the user_code. Polling uses the PKCE code_verifier generated here —
// we stash it in the pending-session state so the background goroutine
// can retrieve it.
//
// On success we persist the access_token + refresh_token + resource_url
// (Qwen's per-user endpoint override) so subsequent requests land on
// the correct region.
func (h *Handler) RequestQwenToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	state := fmt.Sprintf("qw-%d", time.Now().UnixNano())

	pkce, err := qwenauth.NewPKCE()
	if err != nil {
		log.Errorf("Qwen PKCE generation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE"})
		return
	}

	device, err := qwenauth.StartDeviceFlow(ctx, "", pkce)
	if err != nil {
		log.Errorf("Qwen device flow start failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start device flow"})
		return
	}

	RegisterOAuthSession(state, "qwen")

	go func() {
		interval := time.Duration(device.Interval) * time.Second
		if interval <= 0 {
			interval = 5 * time.Second
		}
		// Up to 10 minutes for the operator to complete the browser flow.
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		tok, errPoll := qwenauth.PollDeviceFlow(pollCtx, "", device.DeviceCode, pkce, interval)
		if errPoll != nil {
			SetOAuthSessionError(state, fmt.Sprintf("authorization failed: %v", errPoll))
			return
		}

		label := "Qwen Code"
		email := ""
		// Best-effort: if mapTokens equivalent surfaces an email via the
		// id_token we will fill this in later; for now use the access
		// token suffix as a differentiator.
		if tok.AccessToken != "" {
			suffix := tok.AccessToken
			if len(suffix) > 6 {
				suffix = suffix[len(suffix)-6:]
			}
			label = fmt.Sprintf("Qwen Code (%s)", suffix)
		}

		var expiresAt int64
		if tok.ExpiresIn > 0 {
			expiresAt = time.Now().Unix() + int64(tok.ExpiresIn)
		}

		storage := &qwenauth.QwenTokenStorage{
			AccessToken:  tok.AccessToken,
			RefreshToken: strings.TrimSpace(tok.RefreshToken),
			IDToken:      tok.IDToken,
			Expire:       expiresAt,
			ResourceURL:  tok.ResourceURL,
			Scope:        tok.Scope,
			Email:        email,
			Type:         "qwen",
			LastRefresh:  time.Now().UTC().Format(time.RFC3339),
		}

		metadata := map[string]any{
			"type":          "qwen",
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_at":    expiresAt,
			"resource_url":  tok.ResourceURL,
			"id_token":      tok.IDToken,
			"scope":         tok.Scope,
			"timestamp":     time.Now().UnixMilli(),
		}

		fileName := fmt.Sprintf("qwen-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "qwen",
			FileName: fileName,
			Label:    label,
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("save qwen token: %v", errSave)
			SetOAuthSessionError(state, "failed to persist tokens")
			return
		}
		log.Infof("Qwen credential saved to %s", savedPath)
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("qwen")
	}()

	// Device flow returns BOTH the verification URI and the user code.
	// verification_uri_complete bakes the code in — admin UI should
	// open it directly. Otherwise the operator has to type user_code
	// into the URL on their own device.
	c.JSON(http.StatusOK, gin.H{
		"status":                    "ok",
		"url":                       device.VerificationURI,
		"verification_uri":          device.VerificationURI,
		"verification_uri_complete": device.VerificationURIComplete,
		"user_code":                 device.UserCode,
		"state":                     state,
		"expires_in":                device.ExpiresIn,
		"interval":                  device.Interval,
	})
}
