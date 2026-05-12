package combo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStore persists the combo registry as a single JSON document on disk.
// Writes are atomic: the payload is written to "<path>.tmp" and renamed over
// the original file so readers never observe a half-written file.
//
// The storage schema is intentionally JSON instead of YAML even though the
// wider config uses YAML. Combos are authored through the management API
// (and eventually through the admin UI), not edited by hand, so a stable
// machine-readable format trumps readability here.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore returns a FileStore that reads from and writes to the given
// absolute path. The parent directory must already exist — the caller is
// expected to honor the same auth-dir convention used by the rest of the
// proxy.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

type diskPayload struct {
	Version int      `json:"version"`
	Combos  []*Combo `json:"combos"`
}

// Load reads the persisted combos and applies them to the provided registry.
// A missing file is treated as an empty registry (first-boot case). A
// malformed file returns an error so the server can surface the problem at
// startup instead of silently losing the operator's combos.
func (s *FileStore) Load(r *Registry) error {
	if s == nil || r == nil {
		return fmt.Errorf("combo filestore: nil receiver or registry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("combo filestore: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}

	var payload diskPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("combo filestore: parse %s: %w", s.path, err)
	}
	for _, c := range payload.Combos {
		if c == nil {
			continue
		}
		if err := r.Upsert(c); err != nil {
			// Keep loading the rest — an invalid entry shouldn't block
			// others that are valid. The error is returned as a wrapper
			// so the caller can log it.
			return fmt.Errorf("combo filestore: invalid combo %q: %w", c.Name, err)
		}
	}
	return nil
}

// Save writes the current registry to disk atomically.
func (s *FileStore) Save(r *Registry) error {
	if s == nil || r == nil {
		return fmt.Errorf("combo filestore: nil receiver or registry")
	}
	payload := diskPayload{
		Version: 1,
		Combos:  r.List(),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("combo filestore: encode: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("combo filestore: mkdir %s: %w", dir, err)
		}
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("combo filestore: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("combo filestore: rename: %w", err)
	}
	return nil
}
