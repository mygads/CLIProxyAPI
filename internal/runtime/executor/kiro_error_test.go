package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyKiroError_rateLimit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"http 429", http.StatusTooManyRequests, `{"message":"slow down"}`},
		{"reached the limit (400 body)", http.StatusBadRequest, `{"message":"You have reached the limit for ..."}`},
		{"too many requests phrase", http.StatusBadRequest, "Too Many Requests"},
		{"rate limit phrase", 500, "upstream rate limit hit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, ra := classifyKiroError(tc.status, []byte(tc.body))
			if gotStatus != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", gotStatus)
			}
			if ra == nil {
				t.Fatal("rate-limit must carry a flat RetryAfter")
			}
			if *ra != kiroRateLimitCooldown {
				t.Errorf("RetryAfter = %s, want %s (flat, overlimit-aware)", *ra, kiroRateLimitCooldown)
			}
		})
	}
}

func TestClassifyKiroError_perAccountTransient(t *testing.T) {
	// Malformed framing / invalid model on THIS credential -> 503 so the
	// loop rotates to another account, but NOT a rate-limit cooldown.
	for _, body := range []string{
		"Improperly formed request.",
		`{"message":"invalid model: foo"}`,
	} {
		gotStatus, ra := classifyKiroError(http.StatusBadRequest, []byte(body))
		if gotStatus != http.StatusServiceUnavailable {
			t.Errorf("body %q: status = %d, want 503", body, gotStatus)
		}
		if ra != nil {
			t.Errorf("body %q: per-account transient must not set RetryAfter", body)
		}
	}
}

func TestClassifyKiroError_passthrough(t *testing.T) {
	// A genuine client/payload error (generic 400, 403) is passed through
	// unchanged so the conductor applies its normal handling.
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound} {
		gotStatus, ra := classifyKiroError(status, []byte(`{"message":"validation failed"}`))
		if gotStatus != status {
			t.Errorf("status %d passed through as %d", status, gotStatus)
		}
		if ra != nil {
			t.Errorf("status %d must not set RetryAfter", status)
		}
	}
}

func TestKiroRetryPlan(t *testing.T) {
	// 9router-faithful per-status Kiro retry config.
	cases := []struct {
		status   int
		wantN    int
		wantWait time.Duration
	}{
		{http.StatusTooManyRequests, 2, 2 * time.Second},
		{http.StatusBadGateway, 3, 3 * time.Second},
		{http.StatusServiceUnavailable, 3, 2 * time.Second},
		{http.StatusGatewayTimeout, 2, 3 * time.Second},
		{http.StatusUnauthorized, 0, 0}, // not retried in-place
		{http.StatusBadRequest, 0, 0},
		{http.StatusForbidden, 0, 0},
	}
	for _, tc := range cases {
		n, wait := kiroRetryPlan(tc.status)
		if n != tc.wantN || wait != tc.wantWait {
			t.Errorf("kiroRetryPlan(%d) = (%d, %s), want (%d, %s)", tc.status, n, wait, tc.wantN, tc.wantWait)
		}
	}
}
