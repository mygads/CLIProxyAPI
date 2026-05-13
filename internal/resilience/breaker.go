// Package resilience implements per-credential circuit breakers and
// lightweight self-healing for the CLIProxyAPI scheduler.
//
// Scope: this package is deliberately narrow. It exposes three things:
//
//   1. A state machine (CLOSED → OPEN → HALF_OPEN → CLOSED) per credential.
//   2. A rolling-window failure counter that decides when to trip.
//   3. A reset/probe lifecycle so a tripped credential gets a chance to
//      recover without permanent exclusion.
//
// The scheduler owns the rest — scoring, combo selection, and fallback
// ordering live in the combo package. This split keeps the resilience
// state portable across all three strategies (fallback, round-robin,
// auto) and avoids bundling scoring into the hot path.
//
// Design notes:
//
//   - Rolling window is intentionally simple: consecutive-failure count
//     with a per-window reset, not a sliding histogram. OmniRoute uses a
//     sliding window in tokenFeedback.ts but it only matters under high
//     QPS; for proxy-scale traffic the cheaper counter is enough.
//   - HALF_OPEN emits a single probe permit per cycle. If the probe
//     succeeds we count it; we need `halfOpenProbeSuccess` probes in a
//     row to close. A single failed probe reopens immediately.
//   - Manual overrides (force CLOSE / OPEN / clear exclusion) are
//     deliberately exposed as package functions — the management UI
//     calls them when operators need to override automated decisions.
package resilience

import (
	"sync"
	"sync/atomic"
	"time"
)

// State is the circuit-breaker state for one credential.
type State int32

const (
	// StateClosed is the healthy state. All traffic flows.
	StateClosed State = iota
	// StateOpen means the breaker tripped; no traffic until the reset
	// timer elapses.
	StateOpen
	// StateHalfOpen permits a limited number of probe requests to test
	// whether the credential recovered.
	StateHalfOpen
)

// String returns the human-readable state name (for logs and UI).
func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// Config tunes the breaker. Values come from config.yaml's
// circuit-breaker section (different defaults for oauth vs apikey).
type Config struct {
	// FailureThreshold is the consecutive-failure count that trips the
	// breaker. Below the threshold, errors are tolerated.
	FailureThreshold int
	// ResetAfter is the time a tripped breaker stays OPEN before
	// transitioning to HALF_OPEN for probe traffic.
	ResetAfter time.Duration
	// HalfOpenProbeSuccess is the number of consecutive successful
	// probes required to close a HALF_OPEN breaker.
	HalfOpenProbeSuccess int
}

// DefaultOAuthConfig mirrors the OmniRoute tokenFeedback defaults
// (2026-05). OAuth credentials are more fragile than API keys (token
// refresh failures, fingerprint gating) so the threshold is lower.
func DefaultOAuthConfig() Config {
	return Config{
		FailureThreshold:     8,
		ResetAfter:           60 * time.Second,
		HalfOpenProbeSuccess: 3,
	}
}

// DefaultAPIKeyConfig is the looser default for plain API keys.
func DefaultAPIKeyConfig() Config {
	return Config{
		FailureThreshold:     12,
		ResetAfter:           30 * time.Second,
		HalfOpenProbeSuccess: 2,
	}
}

// Breaker is a single credential's circuit-breaker state. Zero-value is
// a CLOSED breaker with OAuth defaults — callers that care about the
// config should construct via NewBreaker.
type Breaker struct {
	cfg Config

	// state is accessed atomically on the hot path (AllowRequest) so we
	// avoid locking when the breaker is CLOSED.
	state atomic.Int32

	mu               sync.Mutex
	consecutiveFails int
	probeSuccesses   int
	openedAt         time.Time
	forcedOpen       bool
	forcedClosed     bool
	lastTransition   time.Time
}

// NewBreaker constructs a breaker with the supplied config. Zero-valued
// fields in cfg fall back to OAuth defaults so a partial override does
// not accidentally disable the threshold.
func NewBreaker(cfg Config) *Breaker {
	defaults := DefaultOAuthConfig()
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = defaults.FailureThreshold
	}
	if cfg.ResetAfter <= 0 {
		cfg.ResetAfter = defaults.ResetAfter
	}
	if cfg.HalfOpenProbeSuccess <= 0 {
		cfg.HalfOpenProbeSuccess = defaults.HalfOpenProbeSuccess
	}
	b := &Breaker{cfg: cfg}
	b.state.Store(int32(StateClosed))
	return b
}

// State returns the breaker's current state.
func (b *Breaker) State() State {
	if b == nil {
		return StateClosed
	}
	return State(b.state.Load())
}

// Allow returns true if a request may be sent through this breaker.
// Called on the hot path, so the fast path (CLOSED) is lock-free. When
// OPEN, it transitions to HALF_OPEN if enough time has elapsed.
//
// Forced states (via ForceClosed / ForceOpen) short-circuit the
// auto-transition logic: a forced breaker stays in its forced state
// until cleared.
func (b *Breaker) Allow() bool {
	if b == nil {
		return true
	}
	switch State(b.state.Load()) {
	case StateClosed:
		return true
	case StateOpen:
		// Check if reset deadline elapsed.
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.forcedOpen {
			return false
		}
		if !b.openedAt.IsZero() && time.Since(b.openedAt) >= b.cfg.ResetAfter {
			b.transitionLocked(StateHalfOpen)
			b.probeSuccesses = 0
			return true
		}
		return false
	case StateHalfOpen:
		// HALF_OPEN permits every probe; the success counter decides
		// when to close. A failure will reopen via RecordFailure.
		return true
	}
	return true
}

// RecordSuccess notifies the breaker that a request completed
// successfully. In CLOSED state this resets the failure counter; in
// HALF_OPEN it advances the probe counter and may close the breaker.
func (b *Breaker) RecordSuccess() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.forcedClosed {
		b.consecutiveFails = 0
		return
	}
	switch State(b.state.Load()) {
	case StateClosed:
		b.consecutiveFails = 0
	case StateHalfOpen:
		b.probeSuccesses++
		if b.probeSuccesses >= b.cfg.HalfOpenProbeSuccess {
			b.transitionLocked(StateClosed)
			b.consecutiveFails = 0
			b.probeSuccesses = 0
		}
	case StateOpen:
		// Success while OPEN is unusual (means Allow returned false
		// but a request snuck through). Treat it as a probe success.
		b.transitionLocked(StateHalfOpen)
		b.probeSuccesses = 1
	}
}

// RecordFailure notifies the breaker of a failed request. The "failed"
// definition is the caller's: typically a 5xx, 429, auth failure, or
// network timeout. 4xx other than 429 should NOT be reported — they are
// client errors, not provider health signals.
func (b *Breaker) RecordFailure() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.forcedClosed {
		return
	}
	switch State(b.state.Load()) {
	case StateClosed:
		b.consecutiveFails++
		if b.consecutiveFails >= b.cfg.FailureThreshold {
			b.transitionLocked(StateOpen)
			b.openedAt = time.Now()
		}
	case StateHalfOpen:
		// One failed probe reopens immediately — HALF_OPEN is a strict
		// probationary state, not a partial-traffic mode.
		b.transitionLocked(StateOpen)
		b.openedAt = time.Now()
		b.probeSuccesses = 0
	case StateOpen:
		// Refresh the openedAt timestamp so a flapping credential does
		// not prematurely move to HALF_OPEN.
		b.openedAt = time.Now()
	}
}

// ForceClosed pins the breaker to CLOSED and ignores failure events
// until ClearForced() is called. Used by operators to override an
// automated trip while investigating.
func (b *Breaker) ForceClosed() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forcedClosed = true
	b.forcedOpen = false
	b.transitionLocked(StateClosed)
	b.consecutiveFails = 0
}

// ForceOpen pins the breaker to OPEN until ClearForced() is called.
// Used to drain traffic from a credential that is misbehaving in a way
// the threshold has not caught yet (e.g. silent data corruption).
func (b *Breaker) ForceOpen() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forcedOpen = true
	b.forcedClosed = false
	b.transitionLocked(StateOpen)
	b.openedAt = time.Now()
}

// ClearForced removes any manual override and returns the breaker to
// automated control. The current state is kept — next Allow/Record call
// resumes the state machine.
func (b *Breaker) ClearForced() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forcedClosed = false
	b.forcedOpen = false
}

// Snapshot returns a value-type view of the breaker's state for the
// management UI. Holds the mutex briefly — safe to call from any goroutine.
func (b *Breaker) Snapshot() Snapshot {
	if b == nil {
		return Snapshot{State: StateClosed}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var resetIn time.Duration
	if State(b.state.Load()) == StateOpen && !b.openedAt.IsZero() {
		remaining := b.cfg.ResetAfter - time.Since(b.openedAt)
		if remaining > 0 {
			resetIn = remaining
		}
	}
	return Snapshot{
		State:            State(b.state.Load()),
		ConsecutiveFails: b.consecutiveFails,
		ProbeSuccesses:   b.probeSuccesses,
		OpenedAt:         b.openedAt,
		LastTransition:   b.lastTransition,
		ForcedClosed:     b.forcedClosed,
		ForcedOpen:       b.forcedOpen,
		ResetIn:          resetIn,
		Config:           b.cfg,
	}
}

// Snapshot is an immutable view of a breaker's state.
type Snapshot struct {
	State            State
	ConsecutiveFails int
	ProbeSuccesses   int
	OpenedAt         time.Time
	LastTransition   time.Time
	ForcedClosed     bool
	ForcedOpen       bool
	ResetIn          time.Duration
	Config           Config
}

// transitionLocked updates the atomic state and stamps lastTransition.
// Caller must hold b.mu.
func (b *Breaker) transitionLocked(s State) {
	if State(b.state.Load()) == s {
		return
	}
	b.state.Store(int32(s))
	b.lastTransition = time.Now()
}
