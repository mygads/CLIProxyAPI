package combo

import (
	"path/filepath"
	"testing"
)

func TestFileStore_roundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "combos.json")

	r1 := NewRegistry()
	err := r1.Upsert(&Combo{
		Name:        "genfity-2.1",
		Description: "flagship",
		Strategy:    StrategyFallback,
		Entries: []Entry{
			{Priority: 1, Model: "cc/claude-opus-4-7"},
			{Priority: 2, Model: "cx/gpt-5.5", TriggerOn: []string{"quota"}},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	store := NewFileStore(path)
	if err := store.Save(r1); err != nil {
		t.Fatalf("save: %v", err)
	}

	r2 := NewRegistry()
	store2 := NewFileStore(path)
	if err := store2.Load(r2); err != nil {
		t.Fatalf("load: %v", err)
	}

	got, ok := r2.Get("genfity-2.1")
	if !ok {
		t.Fatal("combo missing after reload")
	}
	if got.Description != "flagship" {
		t.Errorf("description: want flagship, got %q", got.Description)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries: want 2, got %d", len(got.Entries))
	}
	if got.Entries[0].Model != "cc/claude-opus-4-7" {
		t.Errorf("entry 0: got %q", got.Entries[0].Model)
	}
	if len(got.Entries[1].TriggerOn) != 1 || got.Entries[1].TriggerOn[0] != "quota" {
		t.Errorf("trigger_on lost: %#v", got.Entries[1].TriggerOn)
	}
}

func TestFileStore_missingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	r := NewRegistry()
	if err := NewFileStore(path).Load(r); err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(r.List()) != 0 {
		t.Error("registry should be empty")
	}
}
