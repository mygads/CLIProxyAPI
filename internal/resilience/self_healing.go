// self_healing.go — progressive-backoff exclusion manager.
//
// When a credential's score falls below a threshold OR its breaker
// trips, the exclusion manager marks it excluded for a cooldown
// period. Repeated failures lengthen the cooldown: 5min → 10min →
// 20min → 30min max. A successful probe clears the exclusion.
//
// The manager is separate from the circuit breaker because breakers
// are binary (OPEN/CLOSED) while exclusions track escalation level.
// A credential with a tripped breaker is always excluded; a credential
// with a low score may be excluded even with a healthy breaker.
//
// Ported from OmniRoute open-sse/services/autoCombo/selfHealing.ts
// (2026-05).
package resilience

import (
	"sync"
	"time"
)

// DefaultBackoffLadder is the cooldown escalation in order. Each
// failure while already excluded advances to the next rung; clamped
// at the last rung (doesn't grow unboundedly).
var DefaultBackoffLadder = []time.Duration{
	5 * time.Minute,
	10 * time.Minute,
	20 * time.Minute,
	30 * time.Minute,
}

// DefaultMinScoreToExclude is the score threshold below which a
// credential is auto-excluded by the scoring engine. Matches
// OmniRoute's selfHealing.ts (0.2).
const DefaultMinScoreToExclude = 0.2

// DefaultMinScoreToReenter is the score a credential must clear before
// the exclusion is lifted. Higher than the exclude threshold to prevent
// flapping (hysteresis).
const DefaultMinScoreToReenter = 0.3

// SelfHealingConfig tunes the exclusion manager. Zero-valued fields
// fall back to defaults.
type SelfHealingConfig struct {
	BackoffLadder     []time.Duration
	MinScoreToExclude float64
	MinScoreToReenter float64
	// IncidentThreshold is the fraction of total credentials that must
	// be excluded before we switch into "incident mode": no exploration,
	// all-exploitation of the best-scored remaining entries. 0 disables.
	IncidentThreshold float64
}

// applyDefaults mutates cfg to fill in any zero-valued fields.
func (c *SelfHealingConfig) applyDefaults() {
	if len(c.BackoffLadder) == 0 {
		c.BackoffLadder = DefaultBackoffLadder
	}
	if c.MinScoreToExclude <= 0 {
		c.MinScoreToExclude = DefaultMinScoreToExclude
	}
	if c.MinScoreToReenter <= 0 {
		c.MinScoreToReenter = DefaultMinScoreToReenter
	}
	if c.IncidentThreshold <= 0 {
		c.IncidentThreshold = 0.5
	}
}

// Exclusion is a single credential's exclusion record.
type Exclusion struct {
	// Level is the current ladder rung (0-indexed). Capped at
	// len(BackoffLadder)-1.
	Level int
	// ExpiresAt is when the exclusion lifts if untouched.
	ExpiresAt time.Time
	// LastReason is operator-facing text describing why the
	// exclusion was applied (score threshold, breaker trip, manual).
	LastReason string
	// MarkedAt is when the current exclusion started. Survives
	// re-triggers; escalation advances ExpiresAt only.
	MarkedAt time.Time
}

// Active returns true if the exclusion has not yet expired.
func (e *Exclusion) Active() bool {
	if e == nil {
		return false
	}
	return time.Now().Before(e.ExpiresAt)
}

// SelfHealing manages per-credential exclusions. Safe for concurrent
// use. Nil-safe methods where it makes sense (Check on a nil receiver
// returns "not excluded").
type SelfHealing struct {
	cfg SelfHealingConfig

	mu         sync.Mutex
	exclusions map[string]*Exclusion
}

// NewSelfHealing builds an exclusion manager with the supplied config.
func NewSelfHealing(cfg SelfHealingConfig) *SelfHealing {
	cfg.applyDefaults()
	return &SelfHealing{
		cfg:        cfg,
		exclusions: make(map[string]*Exclusion),
	}
}

// NewDefaultSelfHealing returns a manager with the canonical defaults.
func NewDefaultSelfHealing() *SelfHealing {
	return NewSelfHealing(SelfHealingConfig{})
}

// Exclude marks authID excluded at the next ladder rung. The first
// call lands at level 0 (5 min); subsequent calls escalate. Reason is
// stored for the UI.
func (s *SelfHealing) Exclude(authID, reason string) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, exists := s.exclusions[authID]
	now := time.Now()
	if !exists {
		ex = &Exclusion{MarkedAt: now}
		s.exclusions[authID] = ex
	} else {
		// If previous exclusion has already expired, start fresh at
		// level 0 — a recovered credential should not carry forward
		// stale escalation.
		if !ex.Active() {
			ex.Level = 0
			ex.MarkedAt = now
		} else {
			ex.Level++
			if ex.Level >= len(s.cfg.BackoffLadder) {
				ex.Level = len(s.cfg.BackoffLadder) - 1
			}
		}
	}
	ex.LastReason = reason
	ex.ExpiresAt = now.Add(s.cfg.BackoffLadder[ex.Level])
}

// Clear lifts the exclusion immediately. Level resets so the next
// failure starts at rung 0 again.
func (s *SelfHealing) Clear(authID string) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.exclusions, authID)
}

// Check returns (excluded, reason). Reason is empty when not excluded.
// GC-prunes expired entries so memory stays bounded.
func (s *SelfHealing) Check(authID string) (bool, string) {
	if s == nil || authID == "" {
		return false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok := s.exclusions[authID]
	if !ok {
		return false, ""
	}
	if !ex.Active() {
		// Expired — drop it so the Check result is stable.
		delete(s.exclusions, authID)
		return false, ""
	}
	return true, ex.LastReason
}

// MaybeExclude inspects the score and either excludes (score below
// threshold) or — if score is above the re-entry threshold — clears a
// stale exclusion. Returns true if the credential is currently excluded
// after the operation.
func (s *SelfHealing) MaybeExclude(authID string, score float64, reason string) bool {
	if s == nil || authID == "" {
		return false
	}
	if score < s.cfg.MinScoreToExclude {
		s.Exclude(authID, reason)
		return true
	}
	// Recovered — lift the exclusion if one was active and score is
	// above re-entry threshold.
	if score >= s.cfg.MinScoreToReenter {
		s.Clear(authID)
		return false
	}
	excluded, _ := s.Check(authID)
	return excluded
}

// IncidentMode returns true when >=IncidentThreshold of totalCredentials
// are currently excluded. Callers use this to switch from exploration
// (try diverse candidates) to exploitation (only use best-scored).
func (s *SelfHealing) IncidentMode(totalCredentials int) bool {
	if s == nil || totalCredentials <= 0 || s.cfg.IncidentThreshold <= 0 {
		return false
	}
	excluded := s.ActiveCount()
	return float64(excluded)/float64(totalCredentials) >= s.cfg.IncidentThreshold
}

// ActiveCount returns the number of currently-excluded credentials.
// GC-prunes expired entries on the way.
func (s *SelfHealing) ActiveCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active := 0
	for id, ex := range s.exclusions {
		if ex.Active() {
			active++
		} else {
			delete(s.exclusions, id)
		}
	}
	return active
}

// Snapshots returns a copy of every exclusion record. Used by the
// management UI to render the health page's self-healing section.
func (s *SelfHealing) Snapshots() map[string]Exclusion {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Exclusion, len(s.exclusions))
	for id, ex := range s.exclusions {
		if !ex.Active() {
			continue
		}
		out[id] = *ex
	}
	return out
}
