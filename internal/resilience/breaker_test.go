package resilience

import (
	"testing"
	"time"
)

func TestBreaker_StaysClosedBelowThreshold(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 3, ResetAfter: time.Second, HalfOpenProbeSuccess: 1})
	for i := 0; i < 2; i++ {
		b.RecordFailure()
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("expected CLOSED below threshold, got %s", got)
	}
	if !b.Allow() {
		t.Fatal("CLOSED breaker should allow traffic")
	}
}

func TestBreaker_TripsAtThreshold(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 3, ResetAfter: time.Hour, HalfOpenProbeSuccess: 1})
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("expected OPEN at threshold, got %s", got)
	}
	if b.Allow() {
		t.Fatal("OPEN breaker should reject traffic")
	}
}

func TestBreaker_SuccessResetsCounter(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 3, ResetAfter: time.Hour, HalfOpenProbeSuccess: 1})
	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess() // resets
	b.RecordFailure()
	b.RecordFailure() // 2 fails, still below threshold of 3
	if got := b.State(); got != StateClosed {
		t.Fatalf("expected CLOSED after success reset, got %s", got)
	}
}

func TestBreaker_HalfOpenAfterReset(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, ResetAfter: 10 * time.Millisecond, HalfOpenProbeSuccess: 1})
	b.RecordFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("expected OPEN, got %s", got)
	}
	time.Sleep(20 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected Allow to transition OPEN→HALF_OPEN after reset")
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN, got %s", got)
	}
}

func TestBreaker_ProbeSuccessClosesBreaker(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, ResetAfter: 5 * time.Millisecond, HalfOpenProbeSuccess: 2})
	b.RecordFailure()
	time.Sleep(15 * time.Millisecond)
	_ = b.Allow() // → HALF_OPEN
	b.RecordSuccess()
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("one probe success should not close breaker (need 2), got %s", got)
	}
	b.RecordSuccess()
	if got := b.State(); got != StateClosed {
		t.Fatalf("expected CLOSED after %d probe successes, got %s", b.cfg.HalfOpenProbeSuccess, got)
	}
}

func TestBreaker_ProbeFailureReopens(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, ResetAfter: 5 * time.Millisecond, HalfOpenProbeSuccess: 2})
	b.RecordFailure()
	time.Sleep(15 * time.Millisecond)
	_ = b.Allow() // → HALF_OPEN
	b.RecordFailure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("probe failure should reopen, got %s", got)
	}
}

func TestBreaker_ForceClosedIgnoresFailures(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, ResetAfter: time.Hour, HalfOpenProbeSuccess: 1})
	b.ForceClosed()
	for i := 0; i < 10; i++ {
		b.RecordFailure()
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("forced-closed breaker should ignore failures, got %s", got)
	}
}

func TestBreaker_ForceOpenBlocksTraffic(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 100, ResetAfter: time.Millisecond, HalfOpenProbeSuccess: 1})
	b.ForceOpen()
	if b.Allow() {
		t.Fatal("forced-open breaker should reject traffic")
	}
	time.Sleep(5 * time.Millisecond)
	// Even past ResetAfter, forced-open stays OPEN.
	if b.Allow() {
		t.Fatal("forced-open should not auto-transition to HALF_OPEN")
	}
}

func TestManager_AllowsUnknownAuth(t *testing.T) {
	m := NewDefaultManager()
	if !m.Allow("brand-new-id", "oauth") {
		t.Fatal("first request through a new credential should be allowed")
	}
}

func TestManager_PerCredentialIsolation(t *testing.T) {
	m := NewManager(
		Config{FailureThreshold: 1, ResetAfter: time.Hour, HalfOpenProbeSuccess: 1},
		Config{FailureThreshold: 5, ResetAfter: time.Hour, HalfOpenProbeSuccess: 1},
	)
	m.RecordFailure("a", "oauth")    // trips at 1
	m.RecordFailure("b", "api_key")  // 1 of 5
	if m.Allow("a", "oauth") {
		t.Fatal("auth a (oauth, threshold 1) should be tripped")
	}
	if !m.Allow("b", "api_key") {
		t.Fatal("auth b (api_key, threshold 5) should still be CLOSED")
	}
}

func TestManager_RemoveDropsState(t *testing.T) {
	m := NewManager(Config{FailureThreshold: 1, ResetAfter: time.Hour}, DefaultAPIKeyConfig())
	m.RecordFailure("vanish", "oauth")
	if m.Allow("vanish", "oauth") {
		t.Fatal("breaker should be tripped before Remove")
	}
	m.Remove("vanish")
	if !m.Allow("vanish", "oauth") {
		t.Fatal("after Remove, the next call should construct a fresh CLOSED breaker")
	}
}
