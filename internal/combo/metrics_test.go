package combo

import (
	"testing"
	"time"
)

func TestMetrics_RecordAndSnapshot(t *testing.T) {
	m := NewMetricsRegistry()
	m.Record("c1", 0, true, 100*time.Millisecond, "")
	m.Record("c1", 0, true, 120*time.Millisecond, "")
	m.Record("c1", 0, false, 500*time.Millisecond, "")
	m.Record("c1", 1, true, 200*time.Millisecond, "quota_exceeded")

	snap := m.Snapshot("c1", 0)
	if snap.TotalRequests != 3 {
		t.Fatalf("expected 3 requests, got %d", snap.TotalRequests)
	}
	if snap.SuccessCount != 2 || snap.FailureCount != 1 {
		t.Fatalf("expected 2/1 success/failure, got %d/%d", snap.SuccessCount, snap.FailureCount)
	}
	if snap.SuccessRate < 0.65 || snap.SuccessRate > 0.68 {
		t.Fatalf("expected success rate ~0.667, got %v", snap.SuccessRate)
	}
	if snap.LatencyP50Sec == 0 {
		t.Fatal("p50 should be non-zero after 3 samples")
	}

	snap2 := m.Snapshot("c1", 1)
	if snap2.TotalRequests != 1 {
		t.Fatalf("entry 1 should have 1 sample, got %d", snap2.TotalRequests)
	}
	if snap2.TriggerReasons["quota_exceeded"] != 1 {
		t.Fatalf("expected 1 quota_exceeded trigger, got %d", snap2.TriggerReasons["quota_exceeded"])
	}
}

func TestMetrics_RetentionWindow(t *testing.T) {
	m := NewMetricsRegistry()
	// Inject an old sample by crafting the entry directly.
	m.mu.Lock()
	m.entries["c2#0"] = &entryMetrics{
		samples: []sample{
			{ts: time.Now().Add(-2 * time.Hour), success: true, latencySec: 0.1},
			{ts: time.Now().Add(-30 * time.Minute), success: true, latencySec: 0.2},
		},
	}
	m.mu.Unlock()

	snap := m.Snapshot("c2", 0)
	// Default retention is 1 hour — the 2h-old sample must be GC'd,
	// only the 30min-old sample should remain.
	if snap.TotalRequests != 1 {
		t.Fatalf("expected 1 sample after GC, got %d", snap.TotalRequests)
	}
}

func TestMetrics_SnapshotAll(t *testing.T) {
	m := NewMetricsRegistry()
	m.Record("multi", 0, true, 50*time.Millisecond, "")
	m.Record("multi", 1, true, 60*time.Millisecond, "")
	m.Record("multi", 2, false, 70*time.Millisecond, "error")
	m.Record("other", 0, true, 10*time.Millisecond, "")

	snaps := m.SnapshotAll("multi")
	if len(snaps) != 3 {
		t.Fatalf("expected 3 snapshots for combo 'multi', got %d", len(snaps))
	}
	for i, s := range snaps {
		if s.EntryIndex != i {
			t.Fatalf("snapshot %d should have EntryIndex %d, got %d", i, i, s.EntryIndex)
		}
	}
}

func TestMetrics_Reset(t *testing.T) {
	m := NewMetricsRegistry()
	m.Record("drop", 0, true, 10*time.Millisecond, "")
	m.Record("drop", 1, true, 20*time.Millisecond, "")
	m.Record("keep", 0, true, 30*time.Millisecond, "")

	m.Reset("drop")
	if got := m.SnapshotAll("drop"); len(got) != 0 {
		t.Fatalf("reset should have dropped all entries; got %d", len(got))
	}
	if got := m.SnapshotAll("keep"); len(got) != 1 {
		t.Fatalf("reset should not touch other combos; got %d", len(got))
	}
}

func TestRegistryUpsertResetsIndexMetricsOnReplacement(t *testing.T) {
	r := NewRegistry()
	first := &Combo{
		Name:   "reordered",
		Status: StatusActive,
		Entries: []Entry{
			{Priority: 0, Model: "server3/kr/auto"},
			{Priority: 1, Model: "mk/mk/auto"},
		},
	}
	if err := r.Upsert(first); err != nil {
		t.Fatal(err)
	}
	r.Metrics().Record(first.Name, 0, false, 0, "incompatible_payload")
	if got := r.Metrics().Snapshot(first.Name, 0).TotalRequests; got != 1 {
		t.Fatalf("precondition total=%d", got)
	}

	reordered := &Combo{
		Name:   first.Name,
		Status: StatusActive,
		Entries: []Entry{
			{Priority: 0, Model: "mk/mk/auto"},
			{Priority: 1, Model: "server3/kr/auto"},
		},
	}
	if err := r.Upsert(reordered); err != nil {
		t.Fatal(err)
	}
	if got := r.Metrics().Snapshot(first.Name, 0).TotalRequests; got != 0 {
		t.Fatalf("stale metrics survived combo replacement: total=%d", got)
	}
}

func TestMetrics_CapSamples(t *testing.T) {
	m := NewMetricsRegistry()
	for i := 0; i < MaxSamplesPerEntry*2; i++ {
		m.Record("cap", 0, true, time.Millisecond, "")
	}
	snap := m.Snapshot("cap", 0)
	if snap.TotalRequests > MaxSamplesPerEntry {
		t.Fatalf("sample cap violated: %d > %d", snap.TotalRequests, MaxSamplesPerEntry)
	}
}

func TestMetrics_NilSafe(t *testing.T) {
	var m *MetricsRegistry
	m.Record("any", 0, true, 0, "") // should not panic
	snap := m.Snapshot("any", 0)
	if snap.TotalRequests != 0 {
		t.Fatalf("nil registry should return zero-value snapshot, got %+v", snap)
	}
}
