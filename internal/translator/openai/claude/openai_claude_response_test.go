package claude

import (
	"bytes"
	"context"
	"testing"
)

func TestConvertOpenAIResponseToClaude_StreamIgnoresNullToolNameDelta(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	firstChunks := ConvertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		&param,
	)
	firstOutput := bytes.Join(firstChunks, nil)
	if !bytes.Contains(firstOutput, []byte(`"name":"read_file"`)) {
		t.Fatalf("expected first chunk to start read_file tool block, got %s", string(firstOutput))
	}

	secondChunks := ConvertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{\"path\":\"/tmp/a\"}"}}]},"finish_reason":null}]}`),
		&param,
	)
	secondOutput := bytes.Join(secondChunks, nil)
	if bytes.Contains(secondOutput, []byte(`content_block_start`)) {
		t.Fatalf("did not expect null tool name delta to start a new content block, got %s", string(secondOutput))
	}
	if bytes.Contains(secondOutput, []byte(`"name":""`)) {
		t.Fatalf("did not expect null tool name delta to emit an empty tool name, got %s", string(secondOutput))
	}
}

// Regression: tool args must stream as input_json_delta chunks as they
// arrive, not buffered until finish_reason. The buffered behavior caused
// long Write/Edit calls to look stalled to Anthropic-shape clients and
// raised the risk of upstream proxy-read timeouts (Cloudflare 524) on
// non-streaming paths.
func TestConvertOpenAIResponseToClaude_StreamsToolArgsIncrementally(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	// Open the tool block.
	startChunks := ConvertOpenAIResponseToClaude(
		context.Background(), "test-model", originalRequest, nil,
		[]byte(`data: {"id":"x","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Write","arguments":""}}]},"finish_reason":null}]}`),
		&param,
	)
	if !bytes.Contains(bytes.Join(startChunks, nil), []byte(`content_block_start`)) {
		t.Fatalf("expected content_block_start in first chunk")
	}

	// First args delta.
	deltaA := ConvertOpenAIResponseToClaude(
		context.Background(), "test-model", originalRequest, nil,
		[]byte(`data: {"id":"x","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"file_path\":"}}]},"finish_reason":null}]}`),
		&param,
	)
	deltaAOut := bytes.Join(deltaA, nil)
	if !bytes.Contains(deltaAOut, []byte(`input_json_delta`)) {
		t.Fatalf("expected input_json_delta in first args chunk, got %s", string(deltaAOut))
	}
	if !bytes.Contains(deltaAOut, []byte(`{\"file_path\":`)) {
		t.Fatalf("expected first delta to carry partial args, got %s", string(deltaAOut))
	}

	// Second args delta — must also emit a delta event, not be buffered.
	deltaB := ConvertOpenAIResponseToClaude(
		context.Background(), "test-model", originalRequest, nil,
		[]byte(`data: {"id":"x","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"a.txt\"}"}}]},"finish_reason":null}]}`),
		&param,
	)
	deltaBOut := bytes.Join(deltaB, nil)
	if !bytes.Contains(deltaBOut, []byte(`input_json_delta`)) {
		t.Fatalf("expected input_json_delta in second args chunk, got %s", string(deltaBOut))
	}

	// finish_reason must NOT replay the entire accumulated args; the
	// closing flush is a no-op once everything has been streamed.
	finishChunks := ConvertOpenAIResponseToClaude(
		context.Background(), "test-model", originalRequest, nil,
		[]byte(`data: {"id":"x","model":"m","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
		&param,
	)
	finishOut := bytes.Join(finishChunks, nil)
	if bytes.Contains(finishOut, []byte(`{\"file_path\":`)) {
		t.Fatalf("finish_reason flush must not replay already-streamed args, got %s", string(finishOut))
	}
	if !bytes.Contains(finishOut, []byte(`content_block_stop`)) {
		t.Fatalf("finish_reason chunk should still emit content_block_stop, got %s", string(finishOut))
	}
	if !bytes.Contains(finishOut, []byte(`message_delta`)) {
		t.Fatalf("finish_reason chunk with usage should emit message_delta, got %s", string(finishOut))
	}
	if !bytes.Contains(finishOut, []byte(`message_stop`)) {
		t.Fatalf("finish_reason chunk with usage should emit message_stop, got %s", string(finishOut))
	}
}
