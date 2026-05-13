// scoring.go — 9-factor scoring engine for the auto-combo strategy.
//
// Ports OmniRoute's open-sse/services/autoCombo/scoring.ts (2026-05).
// Used by combo.StrategyAuto to pick the best candidate dynamically
// per request. Pure functions only — no I/O, no state mutation.
//
// Factor weights are fixed (tuned in OmniRoute production). Operators
// can override at runtime via config.yaml's self-healing block (Phase
// 3G), but the defaults work out of the box.
//
// All factor outputs are clamped to [0..1] before weighting so a
// pathological signal (e.g. negative latency from a clock skew) cannot
// push a candidate's score outside the canonical range.
package resilience

import (
	"math"
)

// Factors carries the nine inputs to the score function. Pass values
// in [0..1] — the function clamps but does not interpret out-of-range
// values.
type Factors struct {
	// Quota is residual quota / token budget. 1.0 = brand-new
	// credential, 0.0 = exhausted.
	Quota float64
	// Health is breaker health: CLOSED=1.0, HALF_OPEN=0.5, OPEN=0.0.
	Health float64
	// CostInv is inverse cost. Cheaper providers score higher; 1.0 =
	// free / cheapest in the pool, 0.0 = most expensive.
	CostInv float64
	// LatencyInv is inverse p95 latency. Faster providers score
	// higher; 1.0 = fastest in the pool, 0.0 = slowest.
	LatencyInv float64
	// TaskFit is model × task fitness from the task-fitness matrix
	// (Phase 3F). 1.0 = perfect match, 0.0 = unsuitable.
	TaskFit float64
	// Stability is how consistent recent latency variance is. 1.0 =
	// rock-solid, 0.0 = wild swings.
	Stability float64
	// TierPriority — provider tier (ultra=1.0, pro=0.75, standard=0.5,
	// free=0.25, deprecated=0.0).
	TierPriority float64
	// TierAffinity — does the requesting plan match the provider tier
	// the manifest expects? 1.0 = perfect, 0.0 = wrong tier.
	TierAffinity float64
	// SpecificityMatch — model specificity match. 1.0 = exact model,
	// lower for generic fallbacks.
	SpecificityMatch float64
}

// Weights mirrors OmniRoute's defaults. The numbers sum to 1.0. Keep
// the public field names stable — config.yaml can override individual
// weights without breaking schema.
type Weights struct {
	Quota            float64
	Health           float64
	CostInv          float64
	LatencyInv       float64
	TaskFit          float64
	Stability        float64
	TierPriority     float64
	TierAffinity     float64
	SpecificityMatch float64
}

// DefaultWeights are the proven values from OmniRoute's production
// experience. Sum = 1.00.
func DefaultWeights() Weights {
	return Weights{
		Quota:            0.17,
		Health:           0.22,
		CostInv:          0.17,
		LatencyInv:       0.13,
		TaskFit:          0.08,
		Stability:        0.05,
		TierPriority:     0.05,
		TierAffinity:     0.05,
		SpecificityMatch: 0.08,
	}
}

// Score computes the weighted score for a candidate. Output is in
// [0..1]; higher is better. Use w == nil to fall back to DefaultWeights.
//
// The formula is a straightforward weighted sum — no log scaling, no
// non-linearities. Predictable behavior is more important than squeezing
// the last percent of accuracy out of a heuristic.
func Score(f Factors, w *Weights) float64 {
	weights := DefaultWeights()
	if w != nil {
		weights = *w
	}
	score := clamp01(f.Quota)*weights.Quota +
		clamp01(f.Health)*weights.Health +
		clamp01(f.CostInv)*weights.CostInv +
		clamp01(f.LatencyInv)*weights.LatencyInv +
		clamp01(f.TaskFit)*weights.TaskFit +
		clamp01(f.Stability)*weights.Stability +
		clamp01(f.TierPriority)*weights.TierPriority +
		clamp01(f.TierAffinity)*weights.TierAffinity +
		clamp01(f.SpecificityMatch)*weights.SpecificityMatch
	return clamp01(score)
}

// HealthFromState converts breaker State into a [0..1] health factor.
// Mirrors OmniRoute's mapping. Inlined in callers when possible to
// avoid an extra function call on the hot path.
func HealthFromState(s State) float64 {
	switch s {
	case StateClosed:
		return 1.0
	case StateHalfOpen:
		return 0.5
	case StateOpen:
		return 0.0
	default:
		return 0.0
	}
}

// LatencyInvFromP95 converts p95 latency to inverse-latency factor.
// p95 0s → 1.0, p95 ≥ 30s → 0.0. Linear in between.
//
// 30s is the worst-case acceptable p95 we set as the floor; anything
// past that is a degraded credential regardless of error rate.
func LatencyInvFromP95(p95Sec float64) float64 {
	const cap = 30.0
	if p95Sec <= 0 {
		return 1.0
	}
	if p95Sec >= cap {
		return 0.0
	}
	return 1.0 - (p95Sec / cap)
}

// StabilityFromVariance converts a coefficient-of-variation into a
// [0..1] stability score. CoV 0 = perfectly consistent (1.0), CoV ≥ 1.0
// = wildly variable (0.0).
//
// Stability matters more for SLA-sensitive callers; the default weight
// is small (0.05) so this only nudges the winner when other factors
// tie.
func StabilityFromVariance(cov float64) float64 {
	if math.IsNaN(cov) || cov < 0 {
		return 0
	}
	if cov >= 1.0 {
		return 0
	}
	return 1.0 - cov
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
