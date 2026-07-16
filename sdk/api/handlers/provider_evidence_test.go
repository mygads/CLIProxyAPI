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
	for chunk := range prependProviderStartedMarker(context.Background(), data, "openai") {
		got = append(got, chunk...)
	}
	want := append(providerStartedChunk("openai"), []byte("data: payload\n\n")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("marked stream = %q, want %q", got, want)
	}
}

func TestProviderStartedChunkMatchesHandlerFraming(t *testing.T) {
	if got := string(providerStartedChunk("openai")); got != `{"type":"genfity.provider_started","genfity_internal":true}` {
		t.Fatalf("OpenAI marker = %q", got)
	}
	if got := string(providerStartedChunk("claude")); got != "event: genfity.provider_started\ndata: {\"type\":\"genfity.provider_started\",\"genfity_internal\":true}\n\n" {
		t.Fatalf("Claude marker = %q", got)
	}
}
