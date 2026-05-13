// Package management exposes the circuit-breaker administration API.
//
// Endpoints (all behind the management secret, same auth as the rest of
// /v0/management):
//
//   GET  /v0/management/breakers                — list all breaker snapshots.
//   POST /v0/management/breakers/:auth_id/force — force a breaker state.
//
// The force endpoint takes a JSON body {"action":"open"|"closed"|"clear"}
// and returns 200 if applied or 404 if the auth ID is not known to the
// breaker manager (i.e. no request has gone through it yet).
package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ListBreakers returns every known circuit-breaker snapshot, keyed by
// auth ID. A breaker appears in the output as soon as the first request
// for that credential flows through the scheduler — before that it
// contributes no state and is treated as CLOSED.
func (h *Handler) ListBreakers(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{"breakers": map[string]any{}})
		return
	}
	mgr := h.authManager.Breakers()
	if mgr == nil {
		c.JSON(http.StatusOK, gin.H{"breakers": map[string]any{}})
		return
	}

	snapshots := mgr.Snapshots()
	out := make(map[string]any, len(snapshots))
	for id, snap := range snapshots {
		entry := map[string]any{
			"state":             snap.State.String(),
			"consecutive_fails": snap.ConsecutiveFails,
			"probe_successes":   snap.ProbeSuccesses,
			"forced_closed":     snap.ForcedClosed,
			"forced_open":       snap.ForcedOpen,
			"config": map[string]any{
				"failure_threshold":       snap.Config.FailureThreshold,
				"reset_after_ms":          snap.Config.ResetAfter.Milliseconds(),
				"half_open_probe_success": snap.Config.HalfOpenProbeSuccess,
			},
		}
		if !snap.OpenedAt.IsZero() {
			entry["opened_at"] = snap.OpenedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if !snap.LastTransition.IsZero() {
			entry["last_transition"] = snap.LastTransition.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if snap.ResetIn > 0 {
			entry["reset_in_ms"] = snap.ResetIn.Milliseconds()
		}
		// Cross-reference with auth metadata (label) so the UI has a
		// readable name. Lookup is best-effort; when the auth is gone
		// we just return the ID.
		if auth, ok := h.authManager.GetAuth(id); ok && auth != nil {
			entry["label"] = auth.Label
			entry["provider"] = auth.Provider
		}
		out[id] = entry
	}
	c.JSON(http.StatusOK, gin.H{"breakers": out})
}

// ForceBreakerRequest is the POST body for /breakers/:id/force.
type ForceBreakerRequest struct {
	// Action is "open" | "closed" | "clear". "clear" removes any prior
	// force override and hands control back to the automated state
	// machine (does NOT reset counters).
	Action string `json:"action"`
}

// ForceBreaker applies a manual override. Returns 400 on unknown
// action, 404 when the auth ID has no breaker, 200 on success.
func (h *Handler) ForceBreaker(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "breakers unavailable"})
		return
	}
	mgr := h.authManager.Breakers()
	if mgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "breakers unavailable"})
		return
	}

	authID := strings.TrimSpace(c.Param("auth_id"))
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id path param required"})
		return
	}

	var body ForceBreakerRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	switch action {
	case "open", "closed", "clear":
		// ok
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be one of: open, closed, clear"})
		return
	}

	if !mgr.ForceState(authID, action) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no breaker exists for auth_id yet (send a request through it first)"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "auth_id": authID, "action": action})
}

// ListExclusions returns active self-healing exclusions — credentials
// that the auto-combo engine has evicted with progressive backoff.
// Each entry carries the reason, ladder level, and when the exclusion
// lifts if untouched.
func (h *Handler) ListExclusions(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{"exclusions": map[string]any{}})
		return
	}
	sh := h.authManager.SelfHealing()
	if sh == nil {
		c.JSON(http.StatusOK, gin.H{"exclusions": map[string]any{}})
		return
	}
	snapshots := sh.Snapshots()
	out := make(map[string]any, len(snapshots))
	for id, ex := range snapshots {
		entry := map[string]any{
			"level":       ex.Level,
			"last_reason": ex.LastReason,
			"marked_at":   ex.MarkedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"expires_at":  ex.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
		if auth, ok := h.authManager.GetAuth(id); ok && auth != nil {
			entry["label"] = auth.Label
			entry["provider"] = auth.Provider
		}
		out[id] = entry
	}
	c.JSON(http.StatusOK, gin.H{"exclusions": out})
}

// ClearExclusion lifts the self-healing exclusion for the given auth
// immediately. Useful when operators have manually resolved the issue
// and want to put the credential back in rotation without waiting for
// the cooldown to elapse.
func (h *Handler) ClearExclusion(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "self-healing unavailable"})
		return
	}
	sh := h.authManager.SelfHealing()
	if sh == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "self-healing unavailable"})
		return
	}
	authID := strings.TrimSpace(c.Param("auth_id"))
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id required"})
		return
	}
	sh.Clear(authID)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "auth_id": authID})
}
