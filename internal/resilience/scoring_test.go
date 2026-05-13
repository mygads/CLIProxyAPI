package resilience

import (
	"testing"
	"time"
)

func TestScore_DefaultWeightsSumToOne(t *testing.T) {
	w := DefaultWeights()
	sum := w.Quota + w.Health + w.CostInv + w.LatencyInv + w.TaskFit +
		w.Stability + w.TierPriority + w.TierAffinity + w.SpecificityMatch
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("default weights should sum to 1.0, got %v", sum)
	}
}

func TestScore_PerfectCandidateScoresOne(t *testing.T) {
	f := Factors{
		Quota: 1, Health: 1, CostInv: 1, LatencyInv: 1, TaskFit: 1,
		Stability: 1, TierPriority: 1, TierAffinity: 1, SpecificityMatch: 1,
	}
	got := Score(f, nil)
	if got < 0.99 {
		t.Fatalf("perfect factors should score ~1.0, got %v", got)
	}
}

func TestScore_ZeroedCandidateScoresZero(t *testing.T) {
	got := Score(Factors{}, nil)
	if got != 0 {
		t.Fatalf("zero factors should score 0, got %v", got)
	}
}

func TestScore_HealthDominatesWhenOthersEqual(t *testing.T) {
	base := Factors{Quota: 0.5, CostInv: 0.5, LatencyInv: 0.5, TaskFit: 0.5, Stability: 0.5, TierPriority: 0.5, TierAffinity: 0.5, SpecificityMatch: 0.5}
	healthy := base
	healthy.Health = 1.0
	open := base
	open.Health = 0.0

	if Score(healthy, nil) <= Score(open, nil) {
		t.Fatal("OPEN breaker should score strictly lower than CLOSED for identical-other candidates")
	}
}

func TestScore_ClampsOutOfRangeInputs(t *testing.T) {
	f := Factors{Quota: 2, Health: -1, CostInv: 0.5}
	got := Score(f, nil)
	// Quota clamps to 1, Health clamps to 0 — resulting score must be
	// in [0..1].
	if got < 0 || got > 1 {
		t.Fatalf("score should be clamped to [0,1], got %v", got)
	}
}

func TestHealthFromState(t *testing.T) {
	cases := []struct {
		in   State
		want float64
	}{
		{StateClosed, 1.0},
		{StateHalfOpen, 0.5},
		{StateOpen, 0.0},
	}
	for _, tc := range cases {
		if got := HealthFromState(tc.in); got != tc.want {
			t.Fatalf("HealthFromState(%s) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLatencyInvFromP95(t *testing.T) {
	cases := []struct {
		p95  float64
		want float64
	}{
		{0, 1.0},
		{15, 0.5},
		{30, 0.0},
		{60, 0.0},
		{-1, 1.0},
	}
	for _, tc := range cases {
		got := LatencyInvFromP95(tc.p95)
		diff := got - tc.want
		if diff < -0.001 || diff > 0.001 {
			t.Fatalf("LatencyInvFromP95(%v) = %v, want %v", tc.p95, got, tc.want)
		}
	}
}

func TestSelfHealing_ExcludeEscalates(t *testing.T) {
	s := NewSelfHealing(SelfHealingConfig{
		BackoffLadder: []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second},
	})
	s.Exclude("cred1", "first failure")
	if got := s.exclusions["cred1"].Level; got != 0 {
		t.Fatalf("first exclude should be level 0, got %d", got)
	}
	s.Exclude("cred1", "second failure")
	if got := s.exclusions["cred1"].Level; got != 1 {
		t.Fatalf("second exclude should escalate to level 1, got %d", got)
	}
	s.Exclude("cred1", "third failure")
	s.Exclude("cred1", "fourth failure")
	if got := s.exclusions["cred1"].Level; got != 2 {
		t.Fatalf("level should cap at len(ladder)-1=2, got %d", got)
	}
}

func TestSelfHealing_CheckReturnsActiveStatus(t *testing.T) {
	s := NewSelfHealing(SelfHealingConfig{
		BackoffLadder: []time.Duration{50 * time.Millisecond},
	})
	s.Exclude("cred1", "boom")
	if excluded, _ := s.Check("cred1"); !excluded {
		t.Fatal("cred1 should be excluded immediately after Exclude")
	}
	time.Sleep(75 * time.Millisecond)
	if excluded, _ := s.Check("cred1"); excluded {
		t.Fatal("cred1 should no longer be excluded after expiry")
	}
}

func TestSelfHealing_MaybeExcludeHysteresis(t *testing.T) {
	s := NewSelfHealing(SelfHealingConfig{})
	// score 0.1 < default MinScoreToExclude (0.2) → exclude.
	excluded := s.MaybeExclude("cred1", 0.1, "low score")
	if !excluded {
		t.Fatal("score 0.1 should trigger exclusion (< 0.2 threshold)")
	}
	// score 0.25 is above exclude threshold but below re-enter (0.3)
	// — exclusion should persist.
	excluded = s.MaybeExclude("cred1", 0.25, "partial recovery")
	if !excluded {
		t.Fatal("hysteresis: score between exclude and reenter should stay excluded")
	}
	// score 0.35 ≥ 0.3 re-enter threshold → clear.
	excluded = s.MaybeExclude("cred1", 0.35, "recovered")
	if excluded {
		t.Fatal("score 0.35 should clear the exclusion (≥ 0.3 re-enter)")
	}
}

func TestSelfHealing_IncidentMode(t *testing.T) {
	s := NewSelfHealing(SelfHealingConfig{
		BackoffLadder:     []time.Duration{10 * time.Minute},
		IncidentThreshold: 0.5,
	})
	// 2 of 4 excluded = 50% → incident mode on.
	s.Exclude("a", "")
	s.Exclude("b", "")
	if !s.IncidentMode(4) {
		t.Fatal("2/4 excluded should be ≥ 0.5 threshold → incident mode")
	}
	if s.IncidentMode(10) {
		t.Fatal("2/10 excluded = 0.2 < 0.5 threshold → NOT incident mode")
	}
}

func TestLatencyTracker_BasicRecordSnapshot(t *testing.T) {
	lt := NewLatencyTracker()
	lt.Record(100*time.Millisecond, true)
	lt.Record(200*time.Millisecond, true)
	lt.Record(300*time.Millisecond, false)
	snap := lt.Snapshot()
	if snap.SampleCount != 3 {
		t.Fatalf("expected 3 samples, got %d", snap.SampleCount)
	}
	if snap.SuccessRate < 0.66 || snap.SuccessRate > 0.67 {
		t.Fatalf("success rate ~0.667 expected, got %v", snap.SuccessRate)
	}
	if snap.P50Sec == 0 {
		t.Fatal("p50 should be > 0 after recording latencies")
	}
}

func TestLatencyTracker_NilSafe(t *testing.T) {
	var lt *LatencyTracker
	lt.Record(time.Second, true)
	if s := lt.Snapshot(); s.SampleCount != 0 {
		t.Fatalf("nil tracker snapshot should be zero, got %+v", s)
	}
}
