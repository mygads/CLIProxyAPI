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

func TestDefaultComboCandidateCooldownConfig(t *testing.T) {
	r := newDefaultComboCandidateCooldownRegistry()
	comboName := "combo-defaults"
	candidateModel := "server3/model"

	for i := 0; i < comboCandidateFailureThreshold; i++ {
		r.record(comboName, candidateModel, false, "upstream_unavailable")
	}

	snapshot, ok := r.breakers.Snapshots()[comboCandidateKey(comboName, candidateModel)]
	if !ok {
		t.Fatal("default cooldown breaker snapshot was not created")
	}
	if snapshot.Config.FailureThreshold != 3 {
		t.Fatalf("failure threshold = %d, want 3", snapshot.Config.FailureThreshold)
	}
	if snapshot.Config.ResetAfter != time.Minute {
		t.Fatalf("reset duration = %s, want 1m", snapshot.Config.ResetAfter)
	}
	if snapshot.State != resilience.StateOpen {
		t.Fatalf("breaker state = %s, want OPEN", snapshot.State)
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

func TestComboCandidateCooldownIgnoresClientCancellation(t *testing.T) {
	r := newComboCandidateCooldownRegistry(resilience.Config{
		FailureThreshold:     1,
		ResetAfter:           time.Minute,
		HalfOpenProbeSuccess: 1,
	})

	r.record("combo-client-cancel", "server3/model", false, "client_canceled")
	if !r.allow("combo-client-cancel", "server3/model") {
		t.Fatal("client cancellation must not cool down a provider candidate")
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
