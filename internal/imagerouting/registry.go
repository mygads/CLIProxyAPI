package imagerouting

import (
	"strings"
	"sync"
)

// Registry is the live, in-memory image-routing config the request path
// reads on every combo request. It is safe for concurrent use. Management
// handlers call Set to update it (changes are live immediately); the file
// store persists it separately.
type Registry struct {
	mu  sync.RWMutex
	cfg Config
	// routedIdx is a lowercased lookup set rebuilt on every Set so the
	// hot-path IsRoutedCombo is O(1) without allocating.
	routedIdx map[string]struct{}
}

// NewRegistry returns an empty registry (feature off until Set is called).
func NewRegistry() *Registry {
	return &Registry{routedIdx: map[string]struct{}{}}
}

// Set replaces the live config. The incoming config is normalized and cloned
// so the caller cannot mutate the registry's copy afterwards.
func (r *Registry) Set(cfg *Config) {
	if r == nil {
		return
	}
	c := cfg.Clone()
	c.Normalize()
	idx := make(map[string]struct{}, len(c.RoutedCombos))
	for _, name := range c.RoutedCombos {
		idx[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	r.mu.Lock()
	r.cfg = *c
	r.routedIdx = idx
	r.mu.Unlock()
}

// Get returns a deep copy of the current config for read-only use (e.g. the
// management GET endpoint and persistence).
func (r *Registry) Get() *Config {
	if r == nil {
		return &Config{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Clone()
}

// Enabled reports whether image routing is turned on.
func (r *Registry) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Enabled
}

// IsRoutedCombo reports whether the given combo name is flagged for image
// routing. Case-insensitive. Returns false when the feature is disabled.
func (r *Registry) IsRoutedCombo(name string) bool {
	if r == nil {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.cfg.Enabled {
		return false
	}
	_, ok := r.routedIdx[key]
	return ok
}

// ChainModels returns the ordered list of chain model names (target first,
// then fallbacks). The slice is a fresh copy safe to iterate without the
// lock held.
func (r *Registry) ChainModels() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.cfg.Chain) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.cfg.Chain))
	for _, e := range r.cfg.Chain {
		if m := strings.TrimSpace(e.Model); m != "" {
			out = append(out, m)
		}
	}
	return out
}
