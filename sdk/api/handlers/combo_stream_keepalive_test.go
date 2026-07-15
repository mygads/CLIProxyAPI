package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestForwardStreamAttemptKeepaliveDoesNotCommitCandidate(t *testing.T) {
	oldInterval := comboStreamKeepaliveInterval
	comboStreamKeepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { comboStreamKeepaliveInterval = oldInterval })

	subData := make(chan []byte)
	subErr := make(chan *interfaces.ErrorMessage, 1)
	data := make(chan []byte, 2)
	errs := make(chan *interfaces.ErrorMessage, 1)
	wantErr := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("provider unavailable")}

	type result struct {
		committed bool
		err       *interfaces.ErrorMessage
	}
	done := make(chan result, 1)
	go func() {
		committed, errMsg := forwardStreamAttemptOnCommit(
			context.Background(), subData, subErr, data, errs,
			make(http.Header), make(http.Header), newPublicStreamSanitizer("combo-test"),
			nil, nil, time.Second, []byte(": keep-alive\n\n"),
		)
		done <- result{committed: committed, err: errMsg}
	}()

	select {
	case got := <-data:
		if string(got) != ": keep-alive\n\n" {
			t.Fatalf("keepalive = %q, want SSE comment", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for combo bootstrap keepalive")
	}

	subErr <- wantErr
	close(subErr)
	close(subData)
	select {
	case got := <-done:
		if got.committed {
			t.Fatal("keepalive incorrectly committed the failed candidate")
		}
		if got.err != wantErr {
			t.Fatalf("error = %v, want original provider error", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not return after provider error")
	}
}
