package management

// github_quota.go — exposes GET /v0/management/github-quota for the
// management UI. Mirrors OmniRoute's lib/usage/fetcher.ts::getGitHubUsage:
// rotate the Copilot bearer token if expired, hit
// https://api.github.com/copilot_internal/user with the Copilot extension
// header set, then normalize the response so the UI does not need to know
// about the paid-plan vs free-plan format split.
//
// Token rotation is handled by githubauth.EnsureCopilotToken — the same
// helper the chat executor uses — and rotated fields are persisted to
// disk via authManager.Update so the credential file stays in sync.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	githubauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/github"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// githubQuotaSnapshot is the response shape the FE consumes. Keeping the
// JSON tags snake_case aligns with the rest of the management API.
type githubQuotaSnapshot struct {
	Plan      string                            `json:"plan,omitempty"`
	Login     string                            `json:"login,omitempty"`
	ResetDate string                            `json:"reset_date,omitempty"`
	Quotas    map[string]*githubQuotaUsage      `json:"quotas,omitempty"`
	// Message surfaces in the UI when the response is unparseable (e.g.,
	// new account types we have not modeled yet). Mirrors OmniRoute's
	// fallback "GitHub Copilot connected. Unable to parse quota data".
	Message string `json:"message,omitempty"`
	// Raw is the upstream JSON body. The UI hides it by default but it is
	// useful for operators when debugging unexpected payload shapes.
	Raw map[string]any `json:"raw,omitempty"`
}

type githubQuotaUsage struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining,omitempty"`
	Unlimited bool    `json:"unlimited"`
}

// GetGithubQuota refreshes the Copilot bearer token, persists any rotated
// fields, then probes /copilot_internal/user and normalizes the response.
//
// Endpoint: GET /v0/management/github-quota?auth_index=<index>
//
// Errors:
//   - 400 if auth_index is missing or does not resolve to a GitHub credential
//   - 502 if the upstream returns non-2xx
//   - 503 if the Copilot exchange/refresh chain fails (revoked credential)
func (h *Handler) GetGithubQuota(c *gin.Context) {
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
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "github") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is not a GitHub credential"})
		return
	}

	copilotToken, err := h.refreshGithubCopilotTokenForAuth(ctx, auth)
	if err != nil {
		log.Warnf("github quota: copilot token refresh failed for %s: %v", auth.ID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("copilot token refresh failed: %v", err)})
		return
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, githubauth.CopilotInternalUserURL, nil)
	if errReq != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("build request: %v", errReq)})
		return
	}
	// Headers mirror OmniRoute's getGitHubCopilotInternalUserHeaders. The
	// auth scheme is "Bearer <copilot_token>" — NOT "token <gho_...>" —
	// because /copilot_internal/user is the Copilot-side API, not GitHub's
	// REST API.
	req.Header.Set("Authorization", "Bearer "+copilotToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubauth.APIVersion)
	req.Header.Set("User-Agent", githubauth.UserAgent)
	req.Header.Set("Editor-Version", githubauth.EditorVersion)
	req.Header.Set("Editor-Plugin-Version", githubauth.ChatPluginVersion)

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

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("read upstream body: %v", errRead)})
		return
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.JSON(resp.StatusCode, gin.H{
			"error":   "github copilot token rejected by upstream — re-authorize the credential",
			"status":  resp.StatusCode,
			"message": strings.TrimSpace(string(body)),
		})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   fmt.Sprintf("upstream status %d", resp.StatusCode),
			"status":  resp.StatusCode,
			"message": strings.TrimSpace(string(body)),
		})
		return
	}

	snapshot := buildGithubSnapshot(auth, body)
	c.JSON(http.StatusOK, snapshot)
}

// buildGithubSnapshot handles both Copilot response shapes that OmniRoute
// supports:
//
//   - Paid plan: { copilot_plan, quota_reset_date, quota_snapshots: {chat, completions, premium_interactions} }
//   - Free / limited plan: { copilot_plan|access_type_sku, monthly_quotas: {chat, completions}, limited_user_quotas: {chat, completions}, limited_user_reset_date }
//
// Any unrecognized shape is returned as a Message so operators can see what
// arrived and we can extend this normalizer later.
func buildGithubSnapshot(auth *coreauth.Auth, body []byte) githubQuotaSnapshot {
	out := githubQuotaSnapshot{}
	if login, _ := auth.Metadata["login"].(string); login != "" {
		out.Login = login
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		out.Message = "GitHub response was not valid JSON"
		return out
	}
	out.Raw = raw
	out.Plan = pickString(raw, "copilot_plan", "access_type_sku")

	if snapshots, ok := raw["quota_snapshots"].(map[string]any); ok {
		out.ResetDate = pickString(raw, "quota_reset_date", "limited_user_reset_date")
		out.Quotas = map[string]*githubQuotaUsage{}
		for _, key := range []string{"chat", "completions", "premium_interactions"} {
			if entry, ok := snapshots[key].(map[string]any); ok {
				out.Quotas[key] = parseGithubSnapshot(entry)
			}
		}
		return out
	}

	monthly, _ := raw["monthly_quotas"].(map[string]any)
	used, _ := raw["limited_user_quotas"].(map[string]any)
	if monthly != nil || used != nil {
		out.ResetDate = pickString(raw, "limited_user_reset_date", "quota_reset_date")
		out.Quotas = map[string]*githubQuotaUsage{}
		for _, key := range []string{"chat", "completions"} {
			total, _ := pickNumber(monthly, key)
			usedAmt, _ := pickNumber(used, key)
			remaining := total - usedAmt
			if remaining < 0 {
				remaining = 0
			}
			out.Quotas[key] = &githubQuotaUsage{
				Used:      usedAmt,
				Total:     total,
				Remaining: remaining,
				Unlimited: total == 0,
			}
		}
		return out
	}

	out.Message = "GitHub Copilot connected. Quota shape not recognized — check Raw."
	return out
}

// parseGithubSnapshot reads one entry of `quota_snapshots`, which carries
// {entitlement, remaining, percent_remaining, unlimited}. We compute
// `used` so the UI does not have to.
func parseGithubSnapshot(entry map[string]any) *githubQuotaUsage {
	entitlement, _ := pickNumber(entry, "entitlement")
	remaining, _ := pickNumber(entry, "remaining")
	unlimited, _ := entry["unlimited"].(bool)
	used := entitlement - remaining
	if used < 0 {
		used = 0
	}
	return &githubQuotaUsage{
		Used:      used,
		Total:     entitlement,
		Remaining: remaining,
		Unlimited: unlimited,
	}
}

// refreshGithubCopilotTokenForAuth wraps EnsureCopilotToken with
// persistence. Shared between resolveTokenForAuth and GetGithubQuota.
func (h *Handler) refreshGithubCopilotTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("nil auth")
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	res, err := githubauth.EnsureCopilotToken(ctx, auth.ID, auth.Metadata)
	if err != nil {
		return "", err
	}
	if res.Rotated && h != nil && h.authManager != nil {
		now := time.Now()
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		auth.Metadata["type"] = "github"
		if _, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			log.Warnf("github quota: persist refreshed token: %v", errUpdate)
		}
	}
	return res.CopilotToken, nil
}
