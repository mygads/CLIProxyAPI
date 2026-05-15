package management

// kiro_quota.go — exposes GET /v0/management/kiro-quota for the management
// UI. Mirrors 9router's KiroService.listAvailableModels (the endpoint Kiro
// IDE itself uses to render the model picker + rate badges):
//
//   POST https://codewhisperer.us-east-1.amazonaws.com/
//   Authorization: Bearer <kiro_access_token>
//   Content-Type: application/x-amz-json-1.0
//   X-Amz-Target: AmazonCodeWhispererService.ListAvailableModels
//   body: {"origin":"AI_EDITOR","profileArn":"<from credential>"}
//
// AWS does NOT expose a public getUserCredits endpoint — that path returns
// 404 UnknownOperationException. The model list response carries enough
// information (per-model rate multiplier + token limits) to populate the
// UI quota card without needing a separate ledger probe.
//
// Token rotation is handled by kiroauth.RefreshIfExpired; rotated fields
// are persisted to disk via authManager.Update.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// kiroQuotaSnapshot is the response shape returned to the FE.
type kiroQuotaSnapshot struct {
	Plan         string           `json:"plan,omitempty"`
	Email        string           `json:"email,omitempty"`
	ProfileArn   string           `json:"profile_arn,omitempty"`
	Region       string           `json:"region,omitempty"`
	DefaultModel string           `json:"default_model,omitempty"`
	Models       []kiroModelEntry `json:"models,omitempty"`
	// Message is set when the upstream call did not return a parseable
	// catalog so the UI shows informational text instead of an empty grid.
	Message string `json:"message,omitempty"`
}

// kiroModelEntry mirrors the AmazonCodeWhispererService.ListAvailableModels
// response shape with the fields the UI cares about. The full upstream
// model object has additional fields (promptCaching, supportedInputTypes,
// __type) that we do not render — they are dropped to keep the payload
// small.
type kiroModelEntry struct {
	ID             string  `json:"id"`
	Name           string  `json:"name,omitempty"`
	Description    string  `json:"description,omitempty"`
	RateMultiplier float64 `json:"rate_multiplier,omitempty"`
	RateUnit       string  `json:"rate_unit,omitempty"`
	MaxInputTokens int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
}

// rawKiroModel matches the upstream `AmazonCodeWhispererService.ListAvailableModels`
// payload. We unmarshal into this shape and then reduce to kiroModelEntry.
type rawKiroModel struct {
	ModelID         string  `json:"modelId"`
	ModelName       string  `json:"modelName"`
	Description     string  `json:"description"`
	RateMultiplier  float64 `json:"rateMultiplier"`
	RateUnit        string  `json:"rateUnit"`
	TokenLimits     struct {
		MaxInputTokens  int64 `json:"maxInputTokens"`
		MaxOutputTokens int64 `json:"maxOutputTokens"`
	} `json:"tokenLimits"`
}

type rawKiroListResponse struct {
	DefaultModel rawKiroModel   `json:"defaultModel"`
	Models       []rawKiroModel `json:"models"`
}

// GetKiroQuota refreshes the credential's access token (persisting any
// rotation), then calls ListAvailableModels and normalizes the response.
//
// Endpoint: GET /v0/management/kiro-quota?auth_index=<index>
//
// Errors:
//   - 400 if auth_index is missing or does not resolve to a Kiro credential
//   - 502 if CodeWhisperer returns a non-2xx
//   - 503 if the access token cannot be refreshed (refresh_token expired)
func (h *Handler) GetKiroQuota(c *gin.Context) {
	ctx := c.Request.Context()
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is not a Kiro credential"})
		return
	}

	accessToken, err := h.refreshKiroAccessTokenForAuth(ctx, auth)
	if err != nil {
		log.Warnf("kiro quota: token refresh failed for %s: %v", auth.ID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("token refresh failed: %v", err)})
		return
	}

	region, _ := auth.Metadata["region"].(string)
	endpoint := kiroauth.CodeWhispererBaseURL(region)
	profileArn, _ := auth.Metadata["profile_arn"].(string)

	body, errMarshal := json.Marshal(map[string]string{
		"origin":     "AI_EDITOR",
		"profileArn": profileArn,
	})
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("encode request: %v", errMarshal)})
		return
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("build request: %v", errReq)})
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableModels")
	req.Header.Set("User-Agent", kiroauth.UserAgent)
	req.Header.Set("X-Amz-User-Agent", kiroauth.XAmzUserAgent)

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("upstream request failed: %v", errDo)})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("read upstream body: %v", errRead)})
		return
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.JSON(resp.StatusCode, gin.H{
			"error":   "kiro token rejected by upstream — re-import the credential",
			"status":  resp.StatusCode,
			"message": strings.TrimSpace(string(respBody)),
		})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   fmt.Sprintf("upstream status %d", resp.StatusCode),
			"status":  resp.StatusCode,
			"message": strings.TrimSpace(string(respBody)),
		})
		return
	}

	snapshot := buildKiroSnapshot(auth, respBody)
	c.JSON(http.StatusOK, snapshot)
}

// buildKiroSnapshot normalizes the ListAvailableModels response into the
// shape the UI consumes. We expose every model in the catalog plus the
// `defaultModel` modelId so the UI can highlight the server-default entry.
func buildKiroSnapshot(auth *coreauth.Auth, body []byte) kiroQuotaSnapshot {
	out := kiroQuotaSnapshot{}
	if email, _ := auth.Metadata["email"].(string); email != "" {
		out.Email = email
	}
	if arn, _ := auth.Metadata["profile_arn"].(string); arn != "" {
		out.ProfileArn = arn
	}
	if region, _ := auth.Metadata["region"].(string); region != "" {
		out.Region = region
	}

	var raw rawKiroListResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		out.Message = "Kiro response was not valid JSON"
		return out
	}
	out.DefaultModel = raw.DefaultModel.ModelID
	out.Models = make([]kiroModelEntry, 0, len(raw.Models))
	for _, m := range raw.Models {
		out.Models = append(out.Models, kiroModelEntry{
			ID:              m.ModelID,
			Name:            m.ModelName,
			Description:     m.Description,
			RateMultiplier:  m.RateMultiplier,
			RateUnit:        m.RateUnit,
			MaxInputTokens:  m.TokenLimits.MaxInputTokens,
			MaxOutputTokens: m.TokenLimits.MaxOutputTokens,
		})
	}
	if len(out.Models) == 0 {
		out.Message = "Kiro returned an empty model catalog"
	}
	return out
}

// refreshKiroAccessTokenForAuth wraps RefreshIfExpired with persistence.
// It is the helper resolveTokenForAuth and GetKiroQuota share.
func (h *Handler) refreshKiroAccessTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("nil auth")
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	res, err := kiroauth.RefreshIfExpired(ctx, auth.ID, auth.Metadata)
	if err != nil {
		return "", err
	}
	if res.Rotated && h != nil && h.authManager != nil {
		now := time.Now()
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		auth.Metadata["type"] = "kiro"
		if _, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			log.Warnf("kiro quota: persist refreshed token: %v", errUpdate)
		}
	}
	return res.AccessToken, nil
}

// pickString returns the first non-empty string value found at the given
// keys in m. The keys are checked in order so callers can list aliases
// (e.g., camelCase preferred, snake_case fallback).
func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// pickNumber returns the first numeric value (int / int64 / float64) found
// at the given keys, plus a bool indicating whether any non-zero numeric
// was found. Used by github_quota.go.
func pickNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case float64:
			return v, true
		case int64:
			return float64(v), true
		case int:
			return float64(v), true
		case json.Number:
			if f, err := v.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}
