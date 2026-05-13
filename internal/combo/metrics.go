// Package combo's metrics module records per-entry success/failure
// and latency samples so operators can see which combo entries pull
// their weight and which are dragging down p95.
//
// Design:
//
//   - In-memory rolling window per (comboName, entryIndex). Samples older
//     than RetentionWindow are evicted at read time (lazy GC keeps the
//     record path lock-free on the happy path).
//   - Latency samples stored as raw float64 seconds. Quantiles computed
//     at read time via sort; sample cap bounds memory.
//   - Zero allocations on Record when the combo is unknown — metrics
//     exist only for registered combos.
//
// What this is NOT:
//
//   - Persistent storage. Metrics reset on restart. Durable backend is
//     tracked in PRD v2 Phase 3.
//   - A streaming histogram. Simple sorted slice is good enough for the
//     event volume we see per combo (thousands/min, not millions).
package combo

import (
	"sort"
	"sync"
	"time"
)

// RetentionWindow bounds how far back we keep samples. Shorter = more
// reactive dashboards, longer = smoother numbers. 1h matches the
// default time horizon the UI shows.
var RetentionWindow = 1 * time.Hour

// MaxSamplesPerEntry caps per-entry memory. With 10 combos × 5 entries
// × 500 samples × 32 bytes = ~800KB worst case — bounded and fine.
const MaxSamplesPerEntry = 500

// MetricsRegistry stores per-combo-entry rolling metrics. Safe for
// concurrent use. Obtain a shared instance from NewMetricsRegistry and
// reuse it across the process.
type MetricsRegistry struct {
	mu      sync.Mutex
	entries map[string]*entryMetrics // key: combo name + "#" + entry index
}

// entryMetrics holds the rolling data for one (combo, entry).
type entryMetrics struct {
	// samples is a sorted-by-time ring of observations. We append
	// in time order and rely on the caller's monotonic clock.
	samples []sample
}

type sample struct {
	ts         time.Time
	success    bool
	latencySec float64
	// triggerReason, when non-empty, records why we fell through to
	// this entry (e.g. "quota_exceeded"). Populated for head entries
	// on failure, and for non-head entries on successful fallback.
	triggerReason string
}

// NewMetricsRegistry builds a registry with no data. The zero-value is
// also usable — mu is a sync.Mutex, entries is lazily initialized.
func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{entries: make(map[string]*entryMetrics)}
}

// Record appends one observation. latency is wall-clock time spent on
// the upstream call (dial + request + response). triggerReason is
// optional; pass "" when the entry succeeded as primary.
func (r *MetricsRegistry) Record(comboName string, entryIndex int, success bool, latency time.Duration, triggerReason string) {
	if r == nil || comboName == "" {
		return
	}
	key := metricsKey(comboName, entryIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*entryMetrics)
	}
	em, ok := r.entries[key]
	if !ok {
		em = &entryMetrics{}
		r.entries[key] = em
	}
	em.samples = append(em.samples, sample{
		ts:            time.Now(),
		success:       success,
		latencySec:    latency.Seconds(),
		triggerReason: triggerReason,
	})
	// Cap samples — evict oldest when over the limit.
	if len(em.samples) > MaxSamplesPerEntry {
		drop := len(em.samples) - MaxSamplesPerEntry
		em.samples = em.samples[drop:]
	}
}

// EntrySnapshot is a value-type view of one entry's aggregated metrics.
// TotalRequests is the sample count in the retention window (not
// all-time). Quantile latencies are in seconds; 0 when no samples.
type EntrySnapshot struct {
	ComboName     string
	EntryIndex    int
	TotalRequests int
	SuccessCount  int
	FailureCount  int
	SuccessRate   float64 // [0..1]
	LatencyP50Sec float64
	LatencyP95Sec float64
	LatencyP99Sec float64
	// TriggerReasons counts how often each fallback reason fired. Empty
	// when the entry served successfully as primary every time.
	TriggerReasons map[string]int
	// OldestSample / NewestSample bound the retention window actually
	// observed (useful to detect cold entries).
	OldestSample time.Time
	NewestSample time.Time
}

// Snapshot returns aggregated metrics for one combo entry. Reads GC
// expired samples before computing so callers don't see stale data.
func (r *MetricsRegistry) Snapshot(comboName string, entryIndex int) EntrySnapshot {
	if r == nil || comboName == "" {
		return EntrySnapshot{ComboName: comboName, EntryIndex: entryIndex}
	}
	cutoff := time.Now().Add(-RetentionWindow)
	key := metricsKey(comboName, entryIndex)

	r.mu.Lock()
	defer r.mu.Unlock()
	em, ok := r.entries[key]
	if !ok {
		return EntrySnapshot{ComboName: comboName, EntryIndex: entryIndex, TriggerReasons: map[string]int{}}
	}
	// Lazy GC: drop expired samples. Binary search the cutoff so the
	// drop is O(log n) not O(n).
	firstValid := sort.Search(len(em.samples), func(i int) bool {
		return !em.samples[i].ts.Before(cutoff)
	})
	if firstValid > 0 {
		em.samples = em.samples[firstValid:]
	}
	if len(em.samples) == 0 {
		return EntrySnapshot{ComboName: comboName, EntryIndex: entryIndex, TriggerReasons: map[string]int{}}
	}

	snap := EntrySnapshot{
		ComboName:      comboName,
		EntryIndex:     entryIndex,
		TotalRequests:  len(em.samples),
		TriggerReasons: map[string]int{},
		OldestSample:   em.samples[0].ts,
		NewestSample:   em.samples[len(em.samples)-1].ts,
	}
	lat := make([]float64, 0, len(em.samples))
	for _, s := range em.samples {
		if s.success {
			snap.SuccessCount++
		} else {
			snap.FailureCount++
		}
		lat = append(lat, s.latencySec)
		if s.triggerReason != "" {
			snap.TriggerReasons[s.triggerReason]++
		}
	}
	if snap.TotalRequests > 0 {
		snap.SuccessRate = float64(snap.SuccessCount) / float64(snap.TotalRequests)
	}
	sort.Float64s(lat)
	snap.LatencyP50Sec = quantile(lat, 0.50)
	snap.LatencyP95Sec = quantile(lat, 0.95)
	snap.LatencyP99Sec = quantile(lat, 0.99)
	return snap
}

// SnapshotAll returns one snapshot per entry for the combo with the
// given name. Returns nil when the combo has no metrics yet.
func (r *MetricsRegistry) SnapshotAll(comboName string) []EntrySnapshot {
	if r == nil || comboName == "" {
		return nil
	}
	r.mu.Lock()
	// Collect matching entry keys under the lock, then release and
	// snapshot each — avoids holding the mutex across quantile calc.
	keys := make([]string, 0)
	for key := range r.entries {
		if len(key) <= len(comboName) {
			continue
		}
		if key[:len(comboName)] == comboName && key[len(comboName)] == '#' {
			keys = append(keys, key)
		}
	}
	r.mu.Unlock()

	sort.Strings(keys)
	out := make([]EntrySnapshot, 0, len(keys))
	for _, key := range keys {
		idx := parseEntryIndex(key, comboName)
		if idx < 0 {
			continue
		}
		out = append(out, r.Snapshot(comboName, idx))
	}
	return out
}

// Reset drops every sample for the given combo (all entries). Used by
// management endpoints when a combo is deleted so memory isn't
// permanently pinned by dead combos.
func (r *MetricsRegistry) Reset(comboName string) {
	if r == nil || comboName == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.entries {
		if len(key) > len(comboName) && key[:len(comboName)] == comboName && key[len(comboName)] == '#' {
			delete(r.entries, key)
		}
	}
}

func metricsKey(name string, idx int) string {
	return name + "#" + itoa(idx)
}

func parseEntryIndex(key, comboName string) int {
	if len(key) <= len(comboName)+1 {
		return -1
	}
	suffix := key[len(comboName)+1:]
	var idx int
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return -1
		}
		idx = idx*10 + int(c-'0')
	}
	return idx
}

// itoa is a small non-allocating integer-to-string for the metricsKey
// path. strconv.Itoa allocates; this one uses a fixed-size buffer on
// the stack. Handles negative indices for robustness even though we
// don't expect them.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// quantile computes the q-quantile of a sorted float64 slice using the
// nearest-rank method (good enough for p50/p95/p99). Returns 0 on
// empty input.
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
