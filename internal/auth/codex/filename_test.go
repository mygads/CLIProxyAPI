package codex

import "testing"

func TestCredentialFileNameTeamScopedPlans(t *testing.T) {
	tests := []struct {
		name, email, plan, hash, want string
	}{
		{"team", "user@example.com", "team", "abc12345", "codex-abc12345-user@example.com-team.json"},
		{"k12", "user@example.com", "k12", "def67890", "codex-def67890-user@example.com-k12.json"},
		{"k12 without hash", "user@example.com", "k12", "", "codex-user@example.com-k12.json"},
		{"plus ignores hash", " user@example.com ", "Plus", "abc12345", "codex-user@example.com-plus.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CredentialFileName(tt.email, tt.plan, tt.hash, true); got != tt.want {
				t.Fatalf("CredentialFileName() = %q, want %q", got, tt.want)
			}
		})
	}
}
