package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	clineauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cline"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RequestClineToken starts a Cline Bot OAuth authorization-code flow.
//
// Cline's UX: the operator opens the authorize URL, completes login in
// the browser, and is redirected to a callback URL with the "code"
// parameter containing a base64-encoded JSON blob of the tokens. The
// callback is handled by the /cline/callback route (registered in
// server.go); this handler just returns the login URL + session state.
//
// redirect_uri is built from the request host so it matches what the
// admin UI is opened at (prod vs local). When that doesn't work, the
// operator can set a custom redirect_uri via query param.
func (h *Handler) RequestClineToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	state := fmt.Sprintf("cl-%d", time.Now().UnixNano())

	redirectURI := strings.TrimSpace(c.Query("redirect_uri"))
	if redirectURI == "" {
		scheme := "https"
		if c.Request != nil && c.Request.TLS == nil {
			// Allow http when the proxy is terminating TLS upstream —
			// the admin UI is already on the same origin, so whichever
			// scheme got us here is correct.
			if c.Request.Header.Get("X-Forwarded-Proto") == "" {
				scheme = "http"
			}
		}
		host := c.Request.Host
		redirectURI = fmt.Sprintf("%s://%s/cline/callback", scheme, host)
	}

	authURL := clineauth.LoginURL(redirectURI)

	RegisterOAuthSession(state, "cline")

	go func() {
		callbackCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		payload, errWait := waitForPendingOAuthCallback(callbackCtx, h.cfg.AuthDir, "cline", state)
		if errWait != nil {
			SetOAuthSessionError(state, fmt.Sprintf("authorization failed: %v", errWait))
			return
		}
		if payload.Error != "" {
			SetOAuthSessionError(state, fmt.Sprintf("cline returned error: %s", payload.Error))
			return
		}

		code := strings.TrimSpace(payload.Code)
		if code == "" {
			SetOAuthSessionError(state, "cline callback missing code")
			return
		}

		bundle, errEx := clineauth.ExchangeToken(ctx, code, redirectURI)
		if errEx != nil {
			log.Errorf("cline token exchange failed: %v", errEx)
			SetOAuthSessionError(state, "token exchange failed")
			return
		}

		// Label: prefer real-name composite, fall back to email, then
		// to a token suffix so the admin UI can always distinguish
		// credentials.
		label := "Cline"
		name := strings.TrimSpace(strings.TrimSpace(bundle.FirstName) + " " + strings.TrimSpace(bundle.LastName))
		switch {
		case name != "":
			label = fmt.Sprintf("Cline (%s)", name)
		case bundle.Email != "":
			label = fmt.Sprintf("Cline (%s)", bundle.Email)
		default:
			suffix := bundle.AccessToken
			if len(suffix) > 6 {
				suffix = suffix[len(suffix)-6:]
			}
			label = fmt.Sprintf("Cline (%s)", suffix)
		}

		expiresAt := clineauth.ParseExpiresAt(bundle.ExpiresAt)

		storage := &clineauth.ClineTokenStorage{
			AccessToken:  bundle.AccessToken,
			RefreshToken: bundle.RefreshToken,
			Expire:       expiresAt,
			Email:        bundle.Email,
			FirstName:    bundle.FirstName,
			LastName:     bundle.LastName,
			Type:         "cline",
			LastRefresh:  time.Now().UTC().Format(time.RFC3339),
		}

		metadata := map[string]any{
			"type":          "cline",
			"access_token":  bundle.AccessToken,
			"refresh_token": bundle.RefreshToken,
			"expires_at":    expiresAt,
			"email":         bundle.Email,
			"first_name":    bundle.FirstName,
			"last_name":     bundle.LastName,
			"timestamp":     time.Now().UnixMilli(),
		}

		fileName := fmt.Sprintf("cline-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "cline",
			FileName: fileName,
			Label:    label,
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("save cline token: %v", errSave)
			SetOAuthSessionError(state, "failed to persist tokens")
			return
		}
		log.Infof("Cline credential saved to %s", savedPath)
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("cline")
	}()

	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"url":          authURL,
		"state":        state,
		"redirect_uri": redirectURI,
	})
}
