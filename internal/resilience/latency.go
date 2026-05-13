// latency.go — per-credential rolling latency + error-rate samples.
//
// This file is separate from breaker.go so the scoring engine in
// scoring.go can consume metrics without coupling to the state machine.
// A breaker's purpose is binary (allow or not); the scoring engine
// needs graded signals (p95, error rate).
//
// Design: one LatencyTracker per credential ID, bounded ring of
// samples. Read paths compute quantiles via sort — fine for the sample
// size we keep (<= 256). No streaming histogram complexity.
package resilience

import (
	"sort"
	"sync"
	"time"
)

// LatencyWindow bounds how far back samples are kept. Anything older
// than this is treated as stale and evicted on read.
var LatencyWindow = 5 * time.Minute

// MaxLatencySamples caps per-credential memory. 256 samples × 16
// bytes × thousands of credentials stays under a few MB worst case.
const MaxLatencySamples = 256

// LatencyTracker records request outcomes for one credential. Thread-
// safe. The zero value is NOT usable — always construct via NewLatencyTracker.
type LatencyTracker struct {
	mu      sync.Mutex
	samples []latencySample
}

type latencySample struct {
	ts         time.Time
	latencySec float64
	success    bool
}

// NewLatencyTracker returns an empty tracker.
func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{}
}

// Record appends one observation. latency is wall-clock time from
// request send to response close. Zero-latency is a valid record
// (e.g. when we fail before sending) so we keep it.
func (t *LatencyTracker) Record(latency time.Duration, success bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = append(t.samples, latencySample{
		ts:         time.Now(),
		latencySec: latency.Seconds(),
		success:    success,
	})
	if len(t.samples) > MaxLatencySamples {
		drop := len(t.samples) - MaxLatencySamples
		t.samples = t.samples[drop:]
	}
}

// Stats is a value-type aggregate view of the tracker's current state.
// Error rate is in [0..1]; p50/p95/p99 are seconds. SampleCount is the
// number of samples in the retention window.
type Stats struct {
	SampleCount int
	SuccessRate float64 // 0..1
	ErrorRate   float64 // 0..1
	P50Sec      float64
	P95Sec      float64
	P99Sec      float64
	// MostRecent is the wall-clock timestamp of the newest sample.
	// Callers use this to detect cold credentials (no traffic recently).
	MostRecent time.Time
}

// Snapshot returns the current stats. Prunes expired samples before
// computing so readers don't see stale data.
func (t *LatencyTracker) Snapshot() Stats {
	if t == nil {
		return Stats{}
	}
	cutoff := time.Now().Add(-LatencyWindow)

	t.mu.Lock()
	defer t.mu.Unlock()
	firstValid := sort.Search(len(t.samples), func(i int) bool {
		return !t.samples[i].ts.Before(cutoff)
	})
	if firstValid > 0 {
		t.samples = t.samples[firstValid:]
	}
	if len(t.samples) == 0 {
		return Stats{}
	}

	successes := 0
	lat := make([]float64, 0, len(t.samples))
	for _, s := range t.samples {
		if s.success {
			successes++
		}
		lat = append(lat, s.latencySec)
	}
	sort.Float64s(lat)
	stats := Stats{
		SampleCount: len(t.samples),
		SuccessRate: float64(successes) / float64(len(t.samples)),
		P50Sec:      quantile(lat, 0.50),
		P95Sec:      quantile(lat, 0.95),
		P99Sec:      quantile(lat, 0.99),
		MostRecent:  t.samples[len(t.samples)-1].ts,
	}
	stats.ErrorRate = 1 - stats.SuccessRate
	return stats
}

func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	rank := int(float64(n-1) * q)
	if rank < 0 {
		rank = 0
	}
	if rank >= n {
		rank = n - 1
	}
	return sorted[rank]
}
