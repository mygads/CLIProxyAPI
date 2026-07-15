package config

import (
	"testing"
	"time"
)

func TestComboAttemptTimeoutDefaultFitsCommonClientIdleWindow(t *testing.T) {
	var cfg SDKConfig
	if got := cfg.ComboAttemptTimeout(); got != 30*time.Second {
		t.Fatalf("ComboAttemptTimeout() = %v, want 30s", got)
	}
	if got := (&SDKConfig{ComboAttemptTimeoutSeconds: 45}).ComboAttemptTimeout(); got != 45*time.Second {
		t.Fatalf("configured ComboAttemptTimeout() = %v, want 45s", got)
	}
}
