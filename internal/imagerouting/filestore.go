package imagerouting

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStore persists the global image-routing config as a single JSON
// document. Writes are atomic (tmp + rename) so readers never observe a
// half-written file. Mirrors internal/combo.FileStore.
type FileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore returns a FileStore bound to the given absolute path. The
// parent directory must already exist (same auth-dir convention as combos).
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

type diskPayload struct {
	Version int     `json:"version"`
	Config  *Config `json:"config"`
}

// Load reads the persisted config into the registry. A missing file is
// treated as an empty (disabled) config — first-boot case. A malformed file
// returns an error so the server surfaces it at startup.
func (s *FileStore) Load(r *Registry) error {
	if s == nil || r == nil {
		return fmt.Errorf("imagerouting filestore: nil receiver or registry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("imagerouting filestore: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}

	var payload diskPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("imagerouting filestore: parse %s: %w", s.path, err)
	}
	if payload.Config == nil {
		return nil
	}
	r.Set(payload.Config)
	return nil
}

// Save writes the registry's current config to disk atomically.
func (s *FileStore) Save(r *Registry) error {
	if s == nil || r == nil {
		return fmt.Errorf("imagerouting filestore: nil receiver or registry")
	}
	payload := diskPayload{Version: 1, Config: r.Get()}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("imagerouting filestore: encode: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("imagerouting filestore: mkdir %s: %w", dir, err)
		}
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("imagerouting filestore: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("imagerouting filestore: rename: %w", err)
	}
	return nil
}
