package management

// kiro_quota.go — exposes GET /v0/management/kiro-quota for the management
// UI. Mirrors 9router's open-sse/services/usage.js getKiroUsage:
//
//   GET https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits?
//       isEmailRequired=true&origin=AI_EDITOR&resourceType=AGENTIC_REQUEST
//   Authorization: Bearer <kiro_access_token>
//
// Response shape carries usageBreakdownList[] entries with
// {resourceType, currentUsageWithPrecision, usageLimitWithPrecision,
// freeTrialInfo?, nextDateReset}, plus subscriptionInfo.subscriptionTitle.
// We normalise into a flat quotas map (e.g. {credit: {used, total, ...},
// credit_freetrial: {...}}) so the UI can render rows without provider
// branches — same shape 9router exposes at /dashboard/quota.
//
// The handler tries 3 endpoint variants in order (the same fallback chain
// 9router uses) so social/IDC/builder-id auth methods all resolve.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// kiroQuotaSnapshot is the response shape returned to the FE.
type kiroQuotaSnapshot struct {
	Plan       string                       `json:"plan,omitempty"`
	Email      string                       `json:"email,omitempty"`
	UserID     string                       `json:"user_id,omitempty"`
	ProfileArn string                       `json:"profile_arn,omitempty"`
	Region     string                       `json:"region,omitempty"`
	Quotas     map[string]*kiroQuotaUsage   `json:"quotas,omitempty"`
	Message    string                       `json:"message,omitempty"`
}

// kiroQuotaUsage is one row of the quota map. Field names mirror the
// shape 9router's parseKiroQuotaData produces so a UI port between the
// two stays straightforward.
type kiroQuotaUsage struct {
	ResourceType string  `json:"resource_type,omitempty"`
	DisplayName  string  `json:"display_name,omitempty"`
	Used         float64 `json:"used"`
	Total        float64 `json:"total"`
	Remaining    float64 `json:"remaining"`
	Unit         string  `json:"unit,omitempty"`
	ResetAt      string  `json:"reset_at,omitempty"`
	Unlimited    bool    `json:"unlimited"`
	IsFreeTrial  bool    `json:"is_free_trial,omitempty"`
}

// rawKiroBreakdown / rawKiroResponse model the AWS getUsageLimits payload.
// Only the fields we display are typed; the rest pass through unchanged
// in case the UI later wants to surface them.
type rawKiroBreakdown struct {
	ResourceType                string  `json:"resourceType"`
	DisplayName                 string  `json:"displayName"`
	DisplayNamePlural           string  `json:"displayNamePlural"`
	Unit                        string  `json:"unit"`
	CurrentUsage                float64 `json:"currentUsage"`
	CurrentUsageWithPrecision   float64 `json:"currentUsageWithPrecision"`
	UsageLimit                  float64 `json:"usageLimit"`
	UsageLimitWithPrecision     float64 `json:"usageLimitWithPrecision"`
	NextDateReset               float64 `json:"nextDateReset"`
	FreeTrialInfo               *struct {
		CurrentUsage              float64 `json:"currentUsage"`
		CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
		UsageLimit                float64 `json:"usageLimit"`
		UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
		FreeTrialExpiry           any     `json:"freeTrialExpiry"`
	} `json:"freeTrialInfo"`
}

type rawKiroResponse struct {
	NextDateReset    float64            `json:"nextDateReset"`
	UsageBreakdownList []rawKiroBreakdown `json:"usageBreakdownList"`
	SubscriptionInfo *struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
		Type              string `json:"type"`
	} `json:"subscriptionInfo"`
	UserInfo *struct {
		Email  string `json:"email"`
		UserID string `json:"userId"`
	} `json:"userInfo"`
}

// GetKiroQuota refreshes the credential's access token (persisting any
// rotation), then calls getUsageLimits and normalizes the response.
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
	profileArn, _ := auth.Metadata["profile_arn"].(string)

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}

	body, status, attemptErr := tryKiroUsageEndpoints(ctx, httpClient, accessToken, region, profileArn)
	if attemptErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("upstream request failed: %v", attemptErr)})
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.JSON(status, gin.H{
			"error":   "kiro token rejected by upstream — re-import the credential",
			"status":  status,
			"message": strings.TrimSpace(string(body)),
		})
		return
	}
	if status < 200 || status >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   fmt.Sprintf("upstream status %d", status),
			"status":  status,
			"message": strings.TrimSpace(string(body)),
		})
		return
	}

	snapshot := buildKiroSnapshot(auth, body)
	c.JSON(http.StatusOK, snapshot)
}

// tryKiroUsageEndpoints walks the 3 known Kiro usage endpoints in order
// (codewhisperer GET, codewhisperer POST, q.us-east-1 GET) and returns
// the first successful body. Authentication errors short-circuit so the
// handler can surface a re-auth hint to the UI.
func tryKiroUsageEndpoints(
	ctx context.Context,
	client *http.Client,
	accessToken, region, profileArn string,
) ([]byte, int, error) {
	if strings.TrimSpace(region) == "" {
		region = kiroauth.DefaultRegion
	}
	cwBase := fmt.Sprintf(kiroauth.CodeWhispererEndpointTemplate, region)
	getParams := url.Values{}
	getParams.Set("isEmailRequired", "true")
	getParams.Set("origin", "AI_EDITOR")
	getParams.Set("resourceType", "AGENTIC_REQUEST")

	type attempt struct {
		name    string
		method  string
		url     string
		headers map[string]string
		body    []byte
	}
	attempts := []attempt{
		{
			name:   "codewhisperer-get",
			method: http.MethodGet,
			url:    cwBase + "/getUsageLimits?" + getParams.Encode(),
			headers: map[string]string{
				"Authorization":    "Bearer " + accessToken,
				"Accept":           "application/json",
				"x-amz-user-agent": "aws-sdk-js/1.0.0 KiroIDE",
				"user-agent":       "aws-sdk-js/1.0.0 KiroIDE",
			},
		},
		{
			name:   "codewhisperer-post",
			method: http.MethodPost,
			url:    cwBase,
			headers: map[string]string{
				"Authorization": "Bearer " + accessToken,
				"Content-Type":  "application/x-amz-json-1.0",
				"x-amz-target":  "AmazonCodeWhispererService.GetUsageLimits",
				"Accept":        "application/json",
			},
			body: mustMarshal(map[string]any{
				"origin":       "AI_EDITOR",
				"profileArn":   profileArn,
				"resourceType": "AGENTIC_REQUEST",
			}),
		},
		{
			name:   "q-get",
			method: http.MethodGet,
			url: fmt.Sprintf("https://q.%s.amazonaws.com/getUsageLimits?", region) + url.Values{
				"origin":       []string{"AI_EDITOR"},
				"profileArn":   []string{profileArn},
				"resourceType": []string{"AGENTIC_REQUEST"},
			}.Encode(),
			headers: map[string]string{
				"Authorization": "Bearer " + accessToken,
				"Accept":        "application/json",
			},
		},
	}

	var (
		lastBody   []byte
		lastStatus int
		lastErr    error
	)
	for _, a := range attempts {
		var bodyReader io.Reader
		if a.body != nil {
			bodyReader = strings.NewReader(string(a.body))
		}
		req, errReq := http.NewRequestWithContext(ctx, a.method, a.url, bodyReader)
		if errReq != nil {
			lastErr = fmt.Errorf("%s: build request: %w", a.name, errReq)
			continue
		}
		for k, v := range a.headers {
			req.Header.Set(k, v)
		}
		resp, errDo := client.Do(req)
		if errDo != nil {
			lastErr = fmt.Errorf("%s: do: %w", a.name, errDo)
			continue
		}
		respBody, errRead := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if errRead != nil {
			lastErr = fmt.Errorf("%s: read body: %w", a.name, errRead)
			continue
		}
		lastBody = respBody
		lastStatus = resp.StatusCode
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, resp.StatusCode, nil
		}
		// Auth errors short-circuit; the next endpoint will fail too and
		// the UI hint should reflect "re-auth needed".
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return respBody, resp.StatusCode, nil
		}
	}
	if lastBody != nil {
		return lastBody, lastStatus, nil
	}
	return nil, 0, lastErr
}

// buildKiroSnapshot turns getUsageLimits payload into the FE-friendly map.
func buildKiroSnapshot(auth *coreauth.Auth, body []byte) kiroQuotaSnapshot {
	out := kiroQuotaSnapshot{Quotas: map[string]*kiroQuotaUsage{}}

	if email, _ := auth.Metadata["email"].(string); email != "" {
		out.Email = email
	}
	if arn, _ := auth.Metadata["profile_arn"].(string); arn != "" {
		out.ProfileArn = arn
	}
	if region, _ := auth.Metadata["region"].(string); region != "" {
		out.Region = region
	}

	var raw rawKiroResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		out.Message = "Kiro response was not valid JSON"
		return out
	}
	if raw.SubscriptionInfo != nil {
		out.Plan = strings.TrimSpace(raw.SubscriptionInfo.SubscriptionTitle)
	}
	if raw.UserInfo != nil {
		if v := strings.TrimSpace(raw.UserInfo.Email); v != "" {
			out.Email = v
		}
		out.UserID = strings.TrimSpace(raw.UserInfo.UserID)
	}

	rootResetAt := parseKiroResetTime(raw.NextDateReset)
	for _, b := range raw.UsageBreakdownList {
		key := strings.ToLower(strings.TrimSpace(b.ResourceType))
		if key == "" {
			key = "unknown"
		}
		used := pickFloat(b.CurrentUsageWithPrecision, b.CurrentUsage)
		total := pickFloat(b.UsageLimitWithPrecision, b.UsageLimit)
		entryReset := parseKiroResetTime(b.NextDateReset)
		if entryReset == "" {
			entryReset = rootResetAt
		}
		out.Quotas[key] = &kiroQuotaUsage{
			ResourceType: b.ResourceType,
			DisplayName:  b.DisplayName,
			Used:         used,
			Total:        total,
			Remaining:    math.Max(0, total-used),
			Unit:         b.Unit,
			ResetAt:      entryReset,
			Unlimited:    total == 0,
		}
		if b.FreeTrialInfo != nil {
			fUsed := pickFloat(b.FreeTrialInfo.CurrentUsageWithPrecision, b.FreeTrialInfo.CurrentUsage)
			fTotal := pickFloat(b.FreeTrialInfo.UsageLimitWithPrecision, b.FreeTrialInfo.UsageLimit)
			out.Quotas[key+"_freetrial"] = &kiroQuotaUsage{
				ResourceType: b.ResourceType,
				DisplayName:  b.DisplayName + " (Free Trial)",
				Used:         fUsed,
				Total:        fTotal,
				Remaining:    math.Max(0, fTotal-fUsed),
				Unit:         b.Unit,
				ResetAt:      parseKiroExpiry(b.FreeTrialInfo.FreeTrialExpiry, entryReset),
				Unlimited:    fTotal == 0,
				IsFreeTrial:  true,
			}
		}
	}

	if len(out.Quotas) == 0 {
		out.Message = "Kiro returned an empty usage breakdown"
	}
	return out
}

// parseKiroResetTime accepts the Unix-seconds timestamps Kiro emits as
// `1.78e9`. Returns RFC3339 or empty when the value is zero / invalid.
func parseKiroResetTime(v float64) string {
	if v <= 0 {
		return ""
	}
	return time.Unix(int64(v), 0).UTC().Format(time.RFC3339)
}

// parseKiroExpiry handles the polymorphic freeTrialExpiry field which is
// sometimes a Unix-seconds number, sometimes an ISO date, sometimes nil.
func parseKiroExpiry(raw any, fallback string) string {
	switch v := raw.(type) {
	case float64:
		return parseKiroResetTime(v)
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return fallback
		}
		return s
	}
	return fallback
}

// pickFloat returns the first non-zero number, falling back to the next.
// AWS sometimes only populates the integer field for legacy account types.
func pickFloat(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// refreshKiroAccessTokenForAuth wraps RefreshIfExpired with persistence.
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

// pickString returns the first non-empty trimmed string from m for the
// supplied keys. Used by github_quota.go.
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

// pickNumber returns the first numeric value (int / int64 / float64 /
// json.Number) found at the given keys, plus a bool indicating success.
// Used by github_quota.go.
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
