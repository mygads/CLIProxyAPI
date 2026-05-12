// Package combo implements virtual combo models — named aliases that expand
// into an ordered fallback chain of real prefixed models.
//
// A combo like "genfity-2.1" may list entries such as "cc/claude-opus-4-7",
// "cx/gpt-5.5", and "glm/glm-4.6". When a client asks for "genfity-2.1" the
// resolver picks the first entry, and on a retriable failure the caller loops
// to the next entry.
//
// Combos live alongside provider credentials in CLIProxyAPI — there is no
// equivalent layer in genfity-ai-gateway-service anymore (see PRD §3.3).
package combo

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Strategy governs how the resolver orders combo entries for a single request.
type Strategy string

const (
	// StrategyFallback tries entries strictly in priority order. The second
	// entry is only touched if the first one returns a retriable error.
	StrategyFallback Strategy = "fallback"

	// StrategyRoundRobin rotates through entries across requests. A single
	// request still falls through on error, but successive requests start at
	// different anchor points so load spreads across providers.
	StrategyRoundRobin Strategy = "round-robin"
)

// Status indicates whether a combo is published, in draft, or disabled.
type Status string

const (
	StatusActive   Status = "active"
	StatusDraft    Status = "draft"
	StatusDisabled Status = "disabled"
)

// Combo is a named virtual model backed by an ordered list of real models.
type Combo struct {
	Name        string         `json:"name" yaml:"name"`
	Description string         `json:"description,omitempty" yaml:"description,omitempty"`
	Status      Status         `json:"status" yaml:"status"`
	Strategy    Strategy       `json:"strategy" yaml:"strategy"`
	StickyLimit int            `json:"sticky_limit,omitempty" yaml:"sticky_limit,omitempty"`
	Entries     []Entry        `json:"entries" yaml:"entries"`
	Metadata    map[string]any `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at" yaml:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at" yaml:"updated_at"`
}

// Entry is a single fallback step.
type Entry struct {
	// Priority orders entries from lowest (tried first) to highest. Duplicate
	// priorities are tolerated — they are broken by declaration order.
	Priority int `json:"priority" yaml:"priority"`
	// Model is the prefixed upstream identifier (e.g. "cc/claude-opus-4-7").
	// Bare identifiers are rejected by Validate() to keep combos compatible
	// with strict prefix mode.
	Model string `json:"model" yaml:"model"`
	// TriggerOn lists substrings in the upstream error body that trigger
	// failover. An empty list means "fall through on any retriable error".
	TriggerOn []string `json:"trigger_on,omitempty" yaml:"trigger_on,omitempty"`
	// Weight is reserved for weighted round-robin and is currently unused.
	Weight int `json:"weight,omitempty" yaml:"weight,omitempty"`
}

// Validate checks that a combo is well-formed. It is called on every Upsert
// and on every resolver read, so it needs to be cheap.
func (c *Combo) Validate() error {
	if c == nil {
		return fmt.Errorf("combo is nil")
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return fmt.Errorf("combo name is required")
	}
	// Names cannot contain "/" because the request router also uses "/" to
	// split prefix/model. An accidental "team/my-combo" would otherwise be
	// mis-routed to a (non-existent) "team" credential.
	if strings.Contains(name, "/") {
		return fmt.Errorf("combo name %q must not contain '/'", name)
	}
	switch c.Strategy {
	case "":
		c.Strategy = StrategyFallback
	case StrategyFallback, StrategyRoundRobin:
	default:
		return fmt.Errorf("combo %q has unknown strategy %q", name, c.Strategy)
	}
	if c.Status == "" {
		c.Status = StatusActive
	}
	if len(c.Entries) == 0 {
		return fmt.Errorf("combo %q must have at least one entry", name)
	}
	seen := make(map[string]struct{}, len(c.Entries))
	for i, entry := range c.Entries {
		model := strings.TrimSpace(entry.Model)
		if model == "" {
			return fmt.Errorf("combo %q entry #%d is missing model", name, i)
		}
		// Strict prefix mode requires combo entries to reference prefixed
		// models. Otherwise one of the fallback steps could land on a
		// credential that has no canonical owner.
		if !strings.Contains(model, "/") {
			return fmt.Errorf("combo %q entry #%d model %q must include a provider prefix (e.g. \"cc/%s\")", name, i, model, model)
		}
		if _, dup := seen[model]; dup {
			return fmt.Errorf("combo %q has duplicate entry %q", name, model)
		}
		seen[model] = struct{}{}
	}
	return nil
}

// Candidate is one fallback step that the resolver hands back to the caller.
type Candidate struct {
	// Model is the prefixed upstream identifier to forward the request to.
	Model string
	// TriggerOn is copied from the entry. The caller consults it after an
	// upstream error to decide whether to continue iterating.
	TriggerOn []string
	// IsLast is true when this candidate is the final entry — the caller
	// should surface the upstream error as-is instead of promising another
	// attempt.
	IsLast bool
}

// Registry is an in-memory store for combos keyed by lowercase name. It is
// safe for concurrent access. Persistence is handled separately by Storage.
type Registry struct {
	mu        sync.RWMutex
	entries   map[string]*Combo
	rrCursors map[string]int // round-robin cursor per combo name
	rrUsed    map[string]int // sticky counter per combo name
}

// NewRegistry returns an empty in-memory registry.
func NewRegistry() *Registry {
	return &Registry{
		entries:   make(map[string]*Combo),
		rrCursors: make(map[string]int),
		rrUsed:    make(map[string]int),
	}
}

// Upsert inserts or replaces a combo. The caller must hold no other lock.
func (r *Registry) Upsert(combo *Combo) error {
	if combo == nil {
		return fmt.Errorf("combo is nil")
	}
	if err := combo.Validate(); err != nil {
		return err
	}
	key := strings.ToLower(strings.TrimSpace(combo.Name))

	clone := *combo
	clone.Entries = append([]Entry(nil), combo.Entries...)
	if combo.Metadata != nil {
		clone.Metadata = make(map[string]any, len(combo.Metadata))
		for k, v := range combo.Metadata {
			clone.Metadata[k] = v
		}
	}
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
	}
	clone.UpdatedAt = time.Now().UTC()

	r.mu.Lock()
	r.entries[key] = &clone
	r.mu.Unlock()
	return nil
}

// Delete removes a combo by name. It is a no-op when the combo is unknown.
func (r *Registry) Delete(name string) {
	key := strings.ToLower(strings.TrimSpace(name))
	r.mu.Lock()
	delete(r.entries, key)
	delete(r.rrCursors, key)
	delete(r.rrUsed, key)
	r.mu.Unlock()
}

// Get returns a clone of the combo for safe external inspection.
func (r *Registry) Get(name string) (*Combo, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	r.mu.RLock()
	combo, ok := r.entries[key]
	r.mu.RUnlock()
	if !ok || combo == nil {
		return nil, false
	}
	out := *combo
	out.Entries = append([]Entry(nil), combo.Entries...)
	return &out, true
}

// List returns all combos sorted by name.
func (r *Registry) List() []*Combo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Combo, 0, len(r.entries))
	for _, combo := range r.entries {
		if combo == nil {
			continue
		}
		clone := *combo
		clone.Entries = append([]Entry(nil), combo.Entries...)
		out = append(out, &clone)
	}
	return out
}

// Has returns true when name is a registered combo (ignoring status).
// Handlers use it to distinguish combo names from prefixed model names.
func (r *Registry) Has(name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	r.mu.RLock()
	_, ok := r.entries[key]
	r.mu.RUnlock()
	return ok
}

// ListNames returns the names of all active combos in sorted order. It is
// used by /v1/models to surface combo names alongside real upstream models.
func (r *Registry) ListNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for _, c := range r.entries {
		if c == nil || c.Status == StatusDisabled {
			continue
		}
		out = append(out, c.Name)
	}
	sortStrings(out)
	return out
}

// FirstCandidate returns the model name of the head entry after Resolve-level
// ordering (priority ascending, plus round-robin rotation when applicable).
// It returns "" when the combo is unknown, disabled, or empty.
//
// This is the minimal surface used by sdk/api/handlers.ComboResolver — it
// advances the round-robin cursor the same way Resolve does, so the two
// entry points share scheduling state.
func (r *Registry) FirstCandidate(name string) string {
	candidates, ok := r.Resolve(name)
	if !ok || len(candidates) == 0 {
		return ""
	}
	return candidates[0].Model
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// Resolve returns the ordered candidates for a request. The caller iterates
// them until one succeeds or the list is exhausted.
//
// For StrategyFallback the order is the entries sorted by priority ascending.
// For StrategyRoundRobin the list is rotated by a per-combo cursor, so the
// first candidate drifts across requests; within a single request the tail
// still follows priority order.
//
// Resolve returns (nil, false) when the combo is unknown, disabled, or has no
// entries — callers should treat that as "not a combo, pass through".
func (r *Registry) Resolve(name string) ([]Candidate, bool) {
	key := strings.ToLower(strings.TrimSpace(name))

	r.mu.Lock()
	defer r.mu.Unlock()
	combo, ok := r.entries[key]
	if !ok || combo == nil {
		return nil, false
	}
	if combo.Status == StatusDisabled || len(combo.Entries) == 0 {
		return nil, false
	}

	ordered := append([]Entry(nil), combo.Entries...)
	sortEntriesByPriority(ordered)

	if combo.Strategy == StrategyRoundRobin && len(ordered) > 1 {
		sticky := combo.StickyLimit
		if sticky < 1 {
			sticky = 1
		}
		cursor := r.rrCursors[key]
		used := r.rrUsed[key]
		if used >= sticky {
			cursor = (cursor + 1) % len(ordered)
			used = 0
		}
		r.rrCursors[key] = cursor
		r.rrUsed[key] = used + 1
		ordered = rotateEntries(ordered, cursor)
	}

	candidates := make([]Candidate, len(ordered))
	last := len(ordered) - 1
	for i, entry := range ordered {
		candidates[i] = Candidate{
			Model:     entry.Model,
			TriggerOn: append([]string(nil), entry.TriggerOn...),
			IsLast:    i == last,
		}
	}
	return candidates, true
}

// ShouldFallback reports whether an upstream response should trigger the next
// combo candidate. It combines two signals:
//
//   - A retriable HTTP status (429, 500, 502, 503, 504) always triggers.
//   - If the entry declares TriggerOn keywords, the response body must also
//     contain at least one of them (case-insensitive substring match).
//
// An empty TriggerOn slice means "any retriable status triggers" — this is
// the same behaviour the removed genfity-gateway combo had.
func ShouldFallback(status int, body []byte, triggers []string) bool {
	if !isRetriableStatus(status) {
		return false
	}
	if len(triggers) == 0 {
		return true
	}
	haystack := strings.ToLower(string(body))
	for _, t := range triggers {
		needle := strings.ToLower(strings.TrimSpace(t))
		if needle == "" {
			continue
		}
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func isRetriableStatus(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func sortEntriesByPriority(in []Entry) {
	// Insertion sort keeps stable order on equal priorities, which matters
	// because admins routinely create two entries at the same priority and
	// rely on declaration order. stdlib sort.SliceStable would also work;
	// the hand-rolled loop avoids a generic allocation in the hot path.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].Priority > in[j].Priority; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

func rotateEntries(in []Entry, offset int) []Entry {
	if len(in) == 0 || offset == 0 {
		return in
	}
	offset = offset % len(in)
	if offset < 0 {
		offset += len(in)
	}
	out := make([]Entry, 0, len(in))
	out = append(out, in[offset:]...)
	out = append(out, in[:offset]...)
	return out
}
