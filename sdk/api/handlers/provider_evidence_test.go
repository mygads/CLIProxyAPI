package handlers

import (
	"bytes"
	"testing"

	"golang.org/x/net/context"
)

func TestPrependProviderStartedMarker(t *testing.T) {
	data := make(chan []byte, 1)
	data <- []byte("data: payload\n\n")
	close(data)

	var got []byte
	for chunk := range prependProviderStartedMarker(context.Background(), data) {
		got = append(got, chunk...)
	}
	want := append([]byte(providerStartedSSEMarker), []byte("data: payload\n\n")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("marked stream = %q, want %q", got, want)
	}
}
