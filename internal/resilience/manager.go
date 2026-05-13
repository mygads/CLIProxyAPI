package resilience

import (
	"sync"
)

// Manager owns one Breaker per credential ID and routes Record/Allow
// calls by ID. Callers (the scheduler, executors) talk to Manager
// rather than constructing Breakers directly so the lifecycle stays
// in one place.
//
// The Manager is safe to use from multiple goroutines. Lookups are
// double-checked: a fast read under RLock, a slow path under Lock that
// constructs the breaker on first use.
type Manager struct {
	oauthCfg  Config
	apikeyCfg Config

	mu       sync.RWMutex
	breakers map[string]*Breaker
}

// NewManager builds a Manager with the supplied per-auth-type configs.
// Either config can be zero-valued — defaults will be applied.
func NewManager(oauth, apikey Config) *Manager {
	return &Manager{
		oauthCfg:  oauth,
		apikeyCfg: apikey,
		breakers:  make(map[string]*Breaker),
	}
}

// NewDefaultManager returns a Manager with the package's recommended
// defaults. Suitable for tests and as the canonical fallback when
// config.yaml does not specify a circuit-breaker section.
func NewDefaultManager() *Manager {
	return NewManager(DefaultOAuthConfig(), DefaultAPIKeyConfig())
}

// Get returns the breaker for the given credential, constructing it on
// first call. authKind is "oauth" or "api_key" (case-insensitive); any
// other value defaults to OAuth config (the more cautious default).
func (m *Manager) Get(authID, authKind string) *Breaker {
	if m == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	b, ok := m.breakers[authID]
	m.mu.RUnlock()
	if ok {
		return b
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok = m.breakers[authID]; ok {
		return b
	}
	cfg := m.oauthCfg
	if normalized := normalizeAuthKind(authKind); normalized == "api_key" {
		cfg = m.apikeyCfg
	}
	b = NewBreaker(cfg)
	m.breakers[authID] = b
	return b
}

// Allow returns true when the credential's breaker permits a request.
// Returns true when the breaker has not been seen before — the first
// request through a brand-new credential is always allowed.
func (m *Manager) Allow(authID, authKind string) bool {
	if m == nil {
		return true
	}
	b := m.Get(authID, authKind)
	if b == nil {
		return true
	}
	return b.Allow()
}

// RecordSuccess and RecordFailure forward to the credential's breaker.
// They are no-ops when authID is empty so they are safe to call on
// every request without nil-checking.
func (m *Manager) RecordSuccess(authID, authKind string) {
	if m == nil || authID == "" {
		return
	}
	m.Get(authID, authKind).RecordSuccess()
}

func (m *Manager) RecordFailure(authID, authKind string) {
	if m == nil || authID == "" {
		return
	}
	m.Get(authID, authKind).RecordFailure()
}

// Remove drops the breaker for a credential — call this when a
// credential is deleted so we do not leak state across credential
// reincarnations with the same ID.
func (m *Manager) Remove(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.breakers, authID)
}

// Snapshots returns a copy of every breaker's state, keyed by auth ID.
// Used by the management UI to render the health page.
func (m *Manager) Snapshots() map[string]Snapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Snapshot, len(m.breakers))
	for id, b := range m.breakers {
		out[id] = b.Snapshot()
	}
	return out
}

// ForceState applies a manual override. action is "open", "closed", or
// "clear" (clear removes any prior override). Unknown actions are
// ignored. Returns true if the breaker existed and the action applied.
func (m *Manager) ForceState(authID, action string) bool {
	if m == nil || authID == "" {
		return false
	}
	m.mu.RLock()
	b, ok := m.breakers[authID]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	switch action {
	case "open":
		b.ForceOpen()
	case "closed":
		b.ForceClosed()
	case "clear":
		b.ClearForced()
	default:
		return false
	}
	return true
}

func normalizeAuthKind(kind string) string {
	switch kind {
	case "api_key", "API_KEY", "apikey", "APIKEY":
		return "api_key"
	default:
		return "oauth"
	}
}
