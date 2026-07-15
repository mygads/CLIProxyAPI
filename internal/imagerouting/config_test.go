package imagerouting

import "testing"

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "empty disabled is valid",
			cfg:  Config{},
		},
		{
			name: "valid chain within cap",
			cfg: Config{
				Enabled:      true,
				RoutedCombos: []string{"genfity/claude-opus-4.8"},
				Chain: []Entry{
					{Priority: 0, Model: "genfity/vision-primary"},
					{Priority: 1, Model: "mk/mk/auto"},
				},
			},
		},
		{
			name: "entry without provider prefix rejected",
			cfg: Config{
				Enabled: true,
				Chain:   []Entry{{Priority: 0, Model: "no-prefix"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate chain entry rejected",
			cfg: Config{
				Enabled: true,
				Chain: []Entry{
					{Priority: 0, Model: "a/b"},
					{Priority: 1, Model: "a/b"},
				},
			},
			wantErr: true,
		},
		{
			name: "chain over cap rejected",
			cfg: Config{
				Enabled: true,
				Chain: []Entry{
					{Model: "a/1"}, {Model: "a/2"}, {Model: "a/3"},
					{Model: "a/4"}, {Model: "a/5"}, {Model: "a/6"}, {Model: "a/7"},
				},
			},
			wantErr: true,
		},
		{
			name: "enabled with routed combos but empty chain rejected",
			cfg: Config{
				Enabled:      true,
				RoutedCombos: []string{"genfity/x"},
				Chain:        nil,
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Normalize()
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestRegistryRoutedComboAndEnabled(t *testing.T) {
	r := NewRegistry()
	if r.Enabled() {
		t.Fatal("fresh registry must be disabled")
	}
	if r.IsRoutedCombo("genfity/claude-opus-4.8") {
		t.Fatal("nothing routed on fresh registry")
	}

	r.Set(&Config{
		Enabled:      true,
		RoutedCombos: []string{"Genfity/Claude-Opus-4.8"}, // mixed case
		Chain:        []Entry{{Priority: 0, Model: "vision/primary"}, {Priority: 1, Model: "mk/mk/auto"}},
	})
	if !r.Enabled() {
		t.Fatal("registry should be enabled after Set")
	}
	// Case-insensitive routed-combo match.
	if !r.IsRoutedCombo("genfity/claude-opus-4.8") {
		t.Fatal("routed combo lookup must be case-insensitive")
	}
	if r.IsRoutedCombo("genfity/gpt-5.5") {
		t.Fatal("non-routed combo must return false")
	}
	got := r.ChainModels()
	if len(got) != 2 || got[0] != "vision/primary" || got[1] != "mk/mk/auto" {
		t.Fatalf("ChainModels order wrong: %v", got)
	}

	// Disabling turns off routing even with a routed combo present.
	r.Set(&Config{Enabled: false, RoutedCombos: []string{"genfity/claude-opus-4.8"}, Chain: got2Entries()})
	if r.IsRoutedCombo("genfity/claude-opus-4.8") {
		t.Fatal("disabled registry must not route")
	}
}

func got2Entries() []Entry {
	return []Entry{{Priority: 0, Model: "a/b"}, {Priority: 1, Model: "c/d"}}
}
