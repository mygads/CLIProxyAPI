package config

import (
	"testing"
	"time"
)

func TestComboAttemptTimeoutDefaultAllowsLongReasoning(t *testing.T) {
	var cfg SDKConfig
	if got := cfg.ComboAttemptTimeout(); got != 120*time.Second {
		t.Fatalf("ComboAttemptTimeout() = %v, want 120s", got)
	}
	if got := (&SDKConfig{ComboAttemptTimeoutSeconds: 45}).ComboAttemptTimeout(); got != 45*time.Second {
		t.Fatalf("configured ComboAttemptTimeout() = %v, want 45s", got)
	}
}
