package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestScheduler_BreakerGateSkipsOpenCredential verifies that when the
// scheduler's breakerGate rejects a credential (simulating an OPEN
// circuit breaker), the scheduler falls through to the next candidate
// instead of returning the tripped one.
//
// This is the core Phase 1A invariant: breakers are not just
// observability — they actually skip candidates.
func TestScheduler_BreakerGateSkipsOpenCredential(t *testing.T) {
	s := newAuthScheduler(&RoundRobinSelector{})

	// Two credentials for the same provider + model.
	authGood := &Auth{ID: "good", Provider: "claude"}
	authBad := &Auth{ID: "bad", Provider: "claude"}
	metaGood := &scheduledAuthMeta{
		auth:              authGood,
		providerKey:       "claude",
		priority:          100,
		supportedModelSet: map[string]struct{}{"claude-opus": {}},
	}
	metaBad := &scheduledAuthMeta{
		auth:              authBad,
		providerKey:       "claude",
		priority:          100,
		supportedModelSet: map[string]struct{}{"claude-opus": {}},
	}

	s.mu.Lock()
	ps := &providerScheduler{
		providerKey: "claude",
		auths:       map[string]*scheduledAuthMeta{"good": metaGood, "bad": metaBad},
		modelShards: map[string]*modelScheduler{},
	}
	s.providers["claude"] = ps
	s.authProviders["good"] = "claude"
	s.authProviders["bad"] = "claude"
	shard := ps.ensureModelLocked("claude-opus", time.Now())
	// Seed both entries as READY — isAuthBlockedForModel defaults to
	// blocked=false for auths with no explicit ModelStates.
	shard.upsertEntryLocked(metaGood, time.Now())
	shard.upsertEntryLocked(metaBad, time.Now())
	s.mu.Unlock()

	// Gate that rejects "bad". pickSingle should return "good".
	var mu sync.Mutex
	gateCalls := 0
	s.breakerGate = func(authID, kind string) bool {
		mu.Lock()
		gateCalls++
		mu.Unlock()
		return authID != "bad"
	}

	got, err := s.pickSingle(context.Background(), "claude", "claude-opus", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected good credential, got nil")
	}
	if got.ID != "good" {
		t.Fatalf("expected ID=good, got %q", got.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if gateCalls == 0 {
		t.Fatal("breakerGate was never consulted — predicate wiring is broken")
	}
}

// TestScheduler_BreakerGateAllowAllPicksAnyCandidate verifies that when
// the gate allows everyone (the default when no breaker has tripped),
// a credential is still returned. Regression guard against the gate
// accidentally filtering too aggressively.
func TestScheduler_BreakerGateAllowAllPicksAnyCandidate(t *testing.T) {
	s := newAuthScheduler(&RoundRobinSelector{})
	auth := &Auth{ID: "solo", Provider: "claude"}
	meta := &scheduledAuthMeta{
		auth:              auth,
		providerKey:       "claude",
		priority:          100,
		supportedModelSet: map[string]struct{}{"claude-opus": {}},
	}
	s.mu.Lock()
	ps := &providerScheduler{
		providerKey: "claude",
		auths:       map[string]*scheduledAuthMeta{"solo": meta},
		modelShards: map[string]*modelScheduler{},
	}
	s.providers["claude"] = ps
	s.authProviders["solo"] = "claude"
	shard := ps.ensureModelLocked("claude-opus", time.Now())
	shard.upsertEntryLocked(meta, time.Now())
	s.mu.Unlock()

	s.breakerGate = func(authID, kind string) bool { return true }

	got, err := s.pickSingle(context.Background(), "claude", "claude-opus", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle failed: %v", err)
	}
	if got == nil || got.ID != "solo" {
		t.Fatalf("expected ID=solo, got %+v", got)
	}
}

// TestScheduler_BreakerGateNilIsNoOp documents that a nil gate does not
// filter anything — matches the default zero-value configuration and
// keeps existing embedders working unchanged.
func TestScheduler_BreakerGateNilIsNoOp(t *testing.T) {
	s := newAuthScheduler(&RoundRobinSelector{})
	auth := &Auth{ID: "lone", Provider: "claude"}
	meta := &scheduledAuthMeta{
		auth:              auth,
		providerKey:       "claude",
		priority:          100,
		supportedModelSet: map[string]struct{}{"claude-opus": {}},
	}
	s.mu.Lock()
	ps := &providerScheduler{
		providerKey: "claude",
		auths:       map[string]*scheduledAuthMeta{"lone": meta},
		modelShards: map[string]*modelScheduler{},
	}
	s.providers["claude"] = ps
	s.authProviders["lone"] = "claude"
	shard := ps.ensureModelLocked("claude-opus", time.Now())
	shard.upsertEntryLocked(meta, time.Now())
	s.mu.Unlock()

	if s.breakerGate != nil {
		t.Fatal("default breakerGate should be nil")
	}

	got, err := s.pickSingle(context.Background(), "claude", "claude-opus", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle failed: %v", err)
	}
	if got == nil || got.ID != "lone" {
		t.Fatalf("expected ID=lone, got %+v", got)
	}
}
