package kiro

import (
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRefreshFailureStatus(t *testing.T) {
	cases := []struct {
		upstream int
		want     int
	}{
		{http.StatusTooManyRequests, http.StatusTooManyRequests}, // refresh rate-limited
		{http.StatusInternalServerError, http.StatusServiceUnavailable},
		{http.StatusBadGateway, http.StatusServiceUnavailable},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{http.StatusBadRequest, http.StatusUnauthorized},   // invalid_grant etc -> creds dead
		{http.StatusUnauthorized, http.StatusUnauthorized}, // revoked
		{http.StatusForbidden, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		if got := refreshFailureStatus(tc.upstream); got != tc.want {
			t.Errorf("refreshFailureStatus(%d) = %d, want %d", tc.upstream, got, tc.want)
		}
	}
}

// authError must satisfy the executor StatusError interface so the
// conductor's errors.AsType unwrap reads its code and cools the credential
// down (instead of treating a dead refresh as status 0 / no cooldown).
func TestAuthError_isStatusError(t *testing.T) {
	var err error = &authError{HTTPStatus: http.StatusUnauthorized, Message: "boom"}
	se, ok := err.(cliproxyexecutor.StatusError)
	if !ok {
		t.Fatal("authError does not satisfy executor.StatusError")
	}
	if se.StatusCode() != http.StatusUnauthorized {
		t.Errorf("StatusCode() = %d, want 401", se.StatusCode())
	}
}
