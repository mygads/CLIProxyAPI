package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSemanticPayloadErrorFromJSON_ParsesStructuredError(t *testing.T) {
	payload := []byte(`{"error":{"code":"rate_limit_exceeded","message":"quota exhausted","type":"rate_limit_error"}}`)
	err := semanticPayloadErrorFromJSON(payload)
	if err == nil {
		t.Fatal("expected structured payload error")
	}
	if err.Code != "rate_limit_exceeded" {
		t.Fatalf("Code = %q, want rate_limit_exceeded", err.Code)
	}
	if err.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want %d", err.HTTPStatus, http.StatusTooManyRequests)
	}
}

func TestInspectPayloadSemanticState_RoleOnlyChunkNotSubstantive(t *testing.T) {
	state := inspectPayloadSemanticState([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	if state.Error != nil {
		t.Fatalf("unexpected error: %v", state.Error)
	}
	if state.Substantive {
		t.Fatal("role-only bootstrap chunk must not commit stream")
	}
}

func TestInspectPayloadSemanticState_PlainPayloadIsSubstantive(t *testing.T) {
	state := inspectPayloadSemanticState([]byte("partial"))
	if state.Error != nil {
		t.Fatalf("unexpected error: %v", state.Error)
	}
	if !state.Substantive {
		t.Fatal("plain payload must commit stream")
	}
}

func TestReadStreamBootstrap_FallsBackOnEmbeddedErrorBeforeContent(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"error\":{\"code\":\"upstream_error\",\"message\":\"provider failed\",\"type\":\"server_error\"}}\n\n")}
	close(ch)

	buffered, closed, err := readStreamBootstrap(context.Background(), ch)
	if err == nil {
		t.Fatal("expected embedded SSE error to fail bootstrap")
	}
	if closed {
		t.Fatal("closed should be false when bootstrap exits on error")
	}
	if len(buffered) != 0 {
		t.Fatalf("buffered = %d, want 0 because fallback should discard bootstrap payload", len(buffered))
	}
	if statusCodeFromError(err) != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", statusCodeFromError(err), http.StatusBadGateway)
	}
}

func TestReadStreamBootstrap_CommitsOnContentChunk(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")}
	close(ch)

	buffered, closed, err := readStreamBootstrap(context.Background(), ch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed {
		t.Fatal("closed should be false when substantive content is found")
	}
	if len(buffered) != 2 {
		t.Fatalf("buffered = %d, want 2", len(buffered))
	}
}
