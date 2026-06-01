package kiro

// refresh_helper.go — refresh-on-demand helper extracted from
// runtime/executor/kiro_executor.go::ensureAccessToken so management handlers
// (quota probes, $TOKEN$ substitution) can reuse the same logic without
// pulling in the executor package.
//
// The two refresh paths and field names mirror OmniRoute's
// src/lib/oauth/services/kiro.ts: SSO OIDC when providerSpecificData carries
// clientId+clientSecret, otherwise the social-auth /refreshToken fallback.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// refreshSkewSeconds is the early-rotation window. We refresh when the
// cached access token has less than this many seconds left, so callers do
// not race the clock and end up firing a request with an already-expired
// token. 60s matches the executor.
const refreshSkewSeconds = 60

// muRegistry serializes refreshes per credential ID. Without this two
// concurrent quota probes (or a quota probe + an executor request) would
// each call /refreshToken, and the second response would clobber the first
// rotated refresh_token — bricking the credential.
var muRegistry sync.Map

func lockFor(authID string) *sync.Mutex {
	muAny, _ := muRegistry.LoadOrStore(authID, &sync.Mutex{})
	return muAny.(*sync.Mutex)
}

// readUnix coerces the various numeric shapes that survive a JSON round-trip
// (int / int64 / float64) into a Unix-seconds value. Anything else returns 0.
func readUnix(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case int:
		return int64(t)
	}
	return 0
}

// readNestedString walks a map[string]any by key path. Returns the trimmed
// value and true only if every step is a map and the leaf is a non-empty
// string. Used for provider_specific_data.{clientId,clientSecret} which the
// management layer stores as a nested map to mirror OmniRoute.
func readNestedString(root map[string]any, keys ...string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	var cur any = root
	for i, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		v, exists := m[k]
		if !exists {
			return "", false
		}
		if i == len(keys)-1 {
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return "", false
			}
			return strings.TrimSpace(s), true
		}
		cur = v
	}
	return "", false
}

// RefreshResult is what RefreshIfExpired hands back. Rotated is true when
// any persisted field changed (access_token / refresh_token / expires_at /
// region / profile_arn) — callers persist to disk only on Rotated.
type RefreshResult struct {
	AccessToken string
	Rotated     bool
}

// RefreshIfExpired ensures auth.Metadata["access_token"] is valid, mutating
// the metadata in place. The caller owns persistence — this helper does not
// touch disk. Returns the access token plus a Rotated flag indicating
// whether any field was changed (so callers can avoid a useless
// authManager.Update on the hot path).
//
// The function is safe to call concurrently from any number of goroutines:
// per-auth.ID locking serializes refreshes and re-checks expiry under the
// lock so the second caller takes the cached value rather than a duplicate
// network round-trip.
func RefreshIfExpired(ctx context.Context, authID string, metadata map[string]any) (RefreshResult, error) {
	if metadata == nil {
		return RefreshResult{}, fmt.Errorf("kiro: nil metadata")
	}
	if accessToken, _ := metadata["access_token"].(string); accessToken != "" {
		expUnix := readUnix(metadata["expires_at"])
		if expUnix > time.Now().Unix()+refreshSkewSeconds {
			return RefreshResult{AccessToken: accessToken}, nil
		}
	}

	mu := lockFor(authID)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the lock; another goroutine may have just refreshed.
	accessToken, _ := metadata["access_token"].(string)
	expUnix := readUnix(metadata["expires_at"])
	if accessToken != "" && expUnix > time.Now().Unix()+refreshSkewSeconds {
		return RefreshResult{AccessToken: accessToken}, nil
	}

	refreshToken, _ := metadata["refresh_token"].(string)
	if refreshToken == "" {
		return RefreshResult{}, &authError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "kiro: missing refresh_token in credential metadata; user must log in again",
		}
	}

	region, _ := metadata["region"].(string)
	ssoClientID, _ := readNestedString(metadata, "provider_specific_data", "clientId")
	ssoClientSecret, _ := readNestedString(metadata, "provider_specific_data", "clientSecret")

	// Path 1 — SSO OIDC (AWS Identity Center). Only attempted when both
	// clientId and clientSecret are present in provider_specific_data, since
	// /token returns InvalidClientException otherwise.
	if ssoClientID != "" && ssoClientSecret != "" {
		ssoResp, err := RefreshSSO(ctx, region, ssoClientID, ssoClientSecret, refreshToken)
		if err != nil {
			return RefreshResult{}, fmt.Errorf("kiro: sso refresh: %w", err)
		}
		metadata["access_token"] = ssoResp.AccessToken
		if ssoResp.ExpiresIn > 0 {
			metadata["expires_at"] = time.Now().Unix() + ssoResp.ExpiresIn
		}
		if rotated := strings.TrimSpace(ssoResp.RefreshToken); rotated != "" {
			metadata["refresh_token"] = rotated
		}
		metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
		return RefreshResult{AccessToken: ssoResp.AccessToken, Rotated: true}, nil
	}

	// Path 2 — social auth fallback. The default desktop-Kiro flow and the
	// only one exposed by the management browser-login today.
	resp, err := Refresh(ctx, refreshToken)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("kiro: refresh: %w", err)
	}
	metadata["access_token"] = resp.AccessToken
	if resp.ExpiresAt > 0 {
		metadata["expires_at"] = resp.ExpiresAt
	}
	if rotated := strings.TrimSpace(resp.RefreshToken); rotated != "" {
		metadata["refresh_token"] = rotated
	}
	if resp.Region != "" {
		metadata["region"] = resp.Region
	}
	if resp.ProfileArn != "" {
		metadata["profile_arn"] = resp.ProfileArn
	}
	if resp.Email != "" {
		metadata["email"] = resp.Email
	}
	metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	return RefreshResult{AccessToken: resp.AccessToken, Rotated: true}, nil
}

// authError mirrors sdk/cliproxy/auth.Error without depending on it. The
// kiroauth package must stay free of cliproxy/auth imports because the
// executor package depends on both — adding the dependency would create a
// cycle.
type authError struct {
	HTTPStatus int
	Message    string
}

func (e *authError) Error() string { return e.Message }

// StatusCode exposes the HTTP-like status so the conductor's
// errors.AsType[StatusError] unwrap (through fmt.Errorf %w chains) can
// read it and apply the right cooldown. Without this a refresh failure
// surfaced as status 0 (default bucket → NO cooldown), so a credential
// whose refresh token is dead got re-selected on every request — the
// "zombie credential" gap.
func (e *authError) StatusCode() int { return e.HTTPStatus }

// refreshFailureStatus maps a refresh-endpoint HTTP status onto the
// credential-level status the rotation layer should act on:
//   - 429            → 429 (refresh itself rate-limited; short cooldown)
//   - 5xx            → 503 (transient; short cooldown, retry soon)
//   - anything else  → 401 (refresh rejected = credential can't
//     authenticate, e.g. invalid_grant/revoked → unauthorized cooldown)
func refreshFailureStatus(upstream int) int {
	switch {
	case upstream == http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case upstream >= 500:
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnauthorized
	}
}
