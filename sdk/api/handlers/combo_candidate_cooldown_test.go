package handlers

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/resilience"
)

func TestComboCandidateCooldownTripsAndRecovers(t *testing.T) {
	r := newComboCandidateCooldownRegistry(resilience.Config{
		FailureThreshold:     3,
		ResetAfter:           5 * time.Millisecond,
		HalfOpenProbeSuccess: 1,
	})

	for i := 0; i < 2; i++ {
		r.record("combo-test", "server3/model", false, "empty_response")
		if !r.allow("combo-test", "server3/model") {
			t.Fatalf("candidate blocked after only %d failures", i+1)
		}
	}
	r.record("combo-test", "server3/model", false, "empty_response")
	if r.allow("combo-test", "server3/model") {
		t.Fatal("candidate allowed after reaching failure threshold")
	}

	time.Sleep(10 * time.Millisecond)
	if !r.allow("combo-test", "server3/model") {
		t.Fatal("candidate did not allow a half-open probe after cooldown")
	}
	r.record("combo-test", "server3/model", true, "")
	if !r.allow("combo-test", "server3/model") {
		t.Fatal("successful probe did not close candidate breaker")
	}
}

func TestComboCandidateCooldownIgnoresBadRequest(t *testing.T) {
	r := newComboCandidateCooldownRegistry(resilience.Config{
		FailureThreshold:     1,
		ResetAfter:           time.Hour,
		HalfOpenProbeSuccess: 1,
	})

	r.record("combo-client-error", "server3/model", false, "bad_request")
	if !r.allow("combo-client-error", "server3/model") {
		t.Fatal("client bad_request incorrectly opened provider cooldown")
	}
}

func TestRecordComboAttemptFeedsHandlerCooldown(t *testing.T) {
	h := NewBaseAPIHandlers(nil, nil)
	for i := 0; i < comboCandidateFailureThreshold; i++ {
		h.recordComboAttempt("combo-handler-wiring", 0, "server3/model", true, false, time.Now(), "upstream_unavailable")
	}
	if h.comboCandidateAvailable("combo-handler-wiring", "server3/model") {
		t.Fatal("handler still considers candidate available after repeated recorded failures")
	}
	// The final entry is deliberately exempted by the loop itself, not by the
	// registry, so a chain always makes at least one real upstream attempt.
	if !h.comboCandidateAvailable("combo-handler-wiring", "apg/model") {
		t.Fatal("unfailed next candidate should remain available")
	}
}

func TestComboCandidateCooldownFollowsModelAfterReorder(t *testing.T) {
	r := newComboCandidateCooldownRegistry(resilience.Config{
		FailureThreshold:     1,
		ResetAfter:           time.Hour,
		HalfOpenProbeSuccess: 1,
	})
	r.record("combo-reorder", "server3/model", false, "upstream_unavailable")

	if r.allow("combo-reorder", "server3/model") {
		t.Fatal("failed model lost its cooldown after moving positions")
	}
	if !r.allow("combo-reorder", "apg/model") {
		t.Fatal("healthy replacement model inherited another model's cooldown")
	}
}
