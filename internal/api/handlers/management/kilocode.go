package management

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	kilocodeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kilocode"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RequestKiloCodeToken starts a KiloCode device-code OAuth flow.
//
// KiloCode's device flow is simpler than RFC-8628: a single POST returns
// the user-visible verificationUrl + an opaque code. The same code is
// polled at /api/device-auth/codes/{code}; HTTP 202 is the "pending"
// signal, 410 is "expired", 403 is "denied". On approval the response
// payload holds {status, token, userEmail}.
//
// There is no refresh step — the access token is treated as long-lived
// (or rotated server-side without exposing a client refresh endpoint).
func (h *Handler) RequestKiloCodeToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	state := fmt.Sprintf("kc-%d", time.Now().UnixNano())

	device, err := kilocodeauth.InitiateDeviceAuth(ctx)
	if err != nil {
		log.Errorf("KiloCode device flow start failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start device flow"})
		return
	}

	RegisterOAuthSession(state, "kilocode")

	go func() {
		interval := time.Duration(device.Interval) * time.Second
		if interval <= 0 {
			interval = 3 * time.Second
		}
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		tok, errPoll := kilocodeauth.PollForApproval(pollCtx, device.DeviceCode, interval)
		if errPoll != nil {
			SetOAuthSessionError(state, fmt.Sprintf("authorization failed: %v", errPoll))
			return
		}

		label := "KiloCode"
		if tok.UserEmail != "" {
			label = fmt.Sprintf("KiloCode (%s)", tok.UserEmail)
		}

		storage := &kilocodeauth.KiloCodeTokenStorage{
			AccessToken: tok.AccessToken,
			Email:       tok.UserEmail,
			Type:        "kilocode",
			LastRefresh: time.Now().UTC().Format(time.RFC3339),
		}

		metadata := map[string]any{
			"type":         "kilocode",
			"access_token": tok.AccessToken,
			"email":        tok.UserEmail,
			"timestamp":    time.Now().UnixMilli(),
		}

		fileName := fmt.Sprintf("kilocode-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kilocode",
			FileName: fileName,
			Label:    label,
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("save kilocode token: %v", errSave)
			SetOAuthSessionError(state, "failed to persist tokens")
			return
		}
		log.Infof("KiloCode credential saved to %s", savedPath)
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("kilocode")
	}()

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
