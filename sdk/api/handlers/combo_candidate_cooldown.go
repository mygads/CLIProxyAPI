package handlers

import (
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/resilience"
)

const (
	comboCandidateFailureThreshold = 3
	comboCandidateResetAfter       = 2 * time.Minute
)

// comboCandidateCooldownRegistry is a circuit breaker for failures detected
// at the combo layer. Credential breakers cannot see failures such as an
// upstream stream that opens successfully but never produces visible content,
// because credential execution has already been reported as successful by the
// time the combo handler detects the empty response.
type comboCandidateCooldownRegistry struct {
	breakers *resilience.Manager
}

func newComboCandidateCooldownRegistry(cfg resilience.Config) *comboCandidateCooldownRegistry {
	return &comboCandidateCooldownRegistry{
		breakers: resilience.NewManager(cfg, cfg),
	}
}

func newDefaultComboCandidateCooldownRegistry() *comboCandidateCooldownRegistry {
	return newComboCandidateCooldownRegistry(resilience.Config{
		FailureThreshold:     comboCandidateFailureThreshold,
		ResetAfter:           comboCandidateResetAfter,
		HalfOpenProbeSuccess: 1,
	})
}

func comboCandidateKey(comboName string, entryIndex int) string {
	return comboName + "#" + strconv.Itoa(entryIndex)
}

func (r *comboCandidateCooldownRegistry) allow(comboName string, entryIndex int) bool {
	if r == nil || r.breakers == nil || comboName == "" || entryIndex < 0 {
		return true
	}
	return r.breakers.Allow(comboCandidateKey(comboName, entryIndex), "api_key")
}

func (r *comboCandidateCooldownRegistry) record(comboName string, entryIndex int, success bool, triggerReason string) {
	if r == nil || r.breakers == nil || comboName == "" || entryIndex < 0 {
		return
	}
	key := comboCandidateKey(comboName, entryIndex)
	if success {
		r.breakers.RecordSuccess(key, "api_key")
		return
	}
	if comboFailureCountsForCooldown(triggerReason) {
		r.breakers.RecordFailure(key, "api_key")
	}
}

func (h *BaseAPIHandler) comboCandidateAvailable(comboName string, entryIndex int) bool {
	if h == nil || h.comboCooldowns == nil {
		return true
	}
	return h.comboCooldowns.allow(comboName, entryIndex)
}

func (h *BaseAPIHandler) recordComboCandidateHealth(comboName string, entryIndex int, success bool, triggerReason string) {
	if h == nil || h.comboCooldowns == nil {
		return
	}
	h.comboCooldowns.record(comboName, entryIndex, success, triggerReason)
}

// Clear client-payload failures must not poison provider health. Everything
// else recorded by the combo path means that this candidate could not provide
// a usable response and should contribute to its cooldown.
func comboFailureCountsForCooldown(triggerReason string) bool {
	return triggerReason != "" && triggerReason != "bad_request"
}
