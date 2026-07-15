// Package imagerouting implements a global image-request override for combo
// models. When a request that CARRIES AN IMAGE targets a combo the operator
// has flagged, the request is re-routed onto a dedicated fallback chain
// instead of the combo's normal chain. Text-only requests are unaffected.
//
// The scheme is intentionally GLOBAL: a single chain + a set of routed combo
// names, shared by every flagged combo. It lives alongside combos.json in the
// auth dir (see filestore.go) so it is durable and R2-synced.
package imagerouting

import (
	"fmt"
	"sort"
	"strings"
)

// MaxChainEntries caps the image chain at one target plus five fallbacks.
const MaxChainEntries = 6

// Entry is one step in the image fallback chain. Model may be a plain
// prefixed upstream (e.g. "mk/mk/auto") or a combo name — combos are
// flattened by the resolver exactly like combo entries.
type Entry struct {
	Priority int    `json:"priority" yaml:"priority"`
	Model    string `json:"model" yaml:"model"`
}

// Config is the single global image-routing scheme.
type Config struct {
	Enabled      bool     `json:"enabled" yaml:"enabled"`
	RoutedCombos []string `json:"routed_combos" yaml:"routed_combos"`
	Chain        []Entry  `json:"chain" yaml:"chain"`
}

// Clone returns a deep copy so callers can hand out or store the config
// without sharing the underlying slices with the live registry.
func (c *Config) Clone() *Config {
	if c == nil {
		return &Config{}
	}
	out := &Config{Enabled: c.Enabled}
	if len(c.RoutedCombos) > 0 {
		out.RoutedCombos = append([]string(nil), c.RoutedCombos...)
	}
	if len(c.Chain) > 0 {
		out.Chain = append([]Entry(nil), c.Chain...)
	}
	return out
}

// Normalize trims and de-dups routed combo names and sorts the chain by
// priority. It does not validate — call Validate for that.
func (c *Config) Normalize() {
	if c == nil {
		return
	}
	seen := make(map[string]struct{}, len(c.RoutedCombos))
	deduped := make([]string, 0, len(c.RoutedCombos))
	for _, name := range c.RoutedCombos {
		n := strings.TrimSpace(name)
		if n == "" {
			continue
		}
		key := strings.ToLower(n)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, n)
	}
	c.RoutedCombos = deduped
	sort.SliceStable(c.Chain, func(i, j int) bool {
		return c.Chain[i].Priority < c.Chain[j].Priority
	})
}

// Validate checks the config is well-formed. An empty/disabled config is
// valid (feature off). When enabled with routed combos, the chain must be
// non-empty, within the cap, and every entry model must carry a provider
// prefix ("/") — matching combo.Combo.Validate's rule.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("image routing config is nil")
	}
	if len(c.Chain) > MaxChainEntries {
		return fmt.Errorf("image chain has %d entries; max is %d (1 target + 5 fallback)", len(c.Chain), MaxChainEntries)
	}
	seen := make(map[string]struct{}, len(c.Chain))
	for i, e := range c.Chain {
		model := strings.TrimSpace(e.Model)
		if model == "" {
			return fmt.Errorf("image chain entry #%d is missing model", i)
		}
		if !strings.Contains(model, "/") {
			return fmt.Errorf("image chain entry #%d model %q must include a provider prefix", i, model)
		}
		key := strings.ToLower(model)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("image chain has duplicate entry %q", model)
		}
		seen[key] = struct{}{}
	}
	// An enabled scheme that actually routes something needs a chain to
	// route to — otherwise a flagged combo's image requests would resolve
	// to nothing and 502. Guard against that misconfiguration.
	if c.Enabled && len(c.RoutedCombos) > 0 && len(c.Chain) == 0 {
		return fmt.Errorf("image routing is enabled with routed combos but the chain is empty")
	}
	return nil
}
