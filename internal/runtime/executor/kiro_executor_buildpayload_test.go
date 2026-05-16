package executor

import (
	"encoding/json"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// jsonGet walks a path like "a.b[0].c" through a generic any tree.
func jsonGet(t *testing.T, root any, path string) any {
	t.Helper()
	cur := root
	for _, seg := range splitPath(path) {
		switch s := seg.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("jsonGet: expected map at %q in %v", s, root)
			}
			cur = m[s]
		case int:
			arr, ok := cur.([]any)
			if !ok {
				t.Fatalf("jsonGet: expected slice at index %d in %v", s, root)
			}
			cur = arr[s]
		}
	}
	return cur
}

func splitPath(path string) []any {
	var out []any
	for _, raw := range strings.Split(path, ".") {
		seg := raw
		for {
			lb := strings.Index(seg, "[")
			if lb < 0 {
				if seg != "" {
					out = append(out, seg)
				}
				break
			}
			if lb > 0 {
				out = append(out, seg[:lb])
			}
			rb := strings.Index(seg, "]")
			idx := 0
			for _, c := range seg[lb+1 : rb] {
				idx = idx*10 + int(c-'0')
			}
			out = append(out, idx)
			seg = seg[rb+1:]
		}
	}
	return out
}

func TestBuildKiroPayload_ToolsAttachedToCurrentMessage(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"list files"}],
		"tools":[{"type":"function","function":{"name":"list_dir","description":"List files","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]
	}`)
	exec := &KiroExecutor{}
	out, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("buildKiroPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tools := jsonGet(t, got, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools")
	arr, ok := tools.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("expected 1 tool spec on currentMessage, got %v", tools)
	}
	spec := jsonGet(t, arr[0], "toolSpecification")
	if name := jsonGet(t, spec, "name").(string); name != "list_dir" {
		t.Errorf("tool name: got %q want list_dir", name)
	}
	if desc := jsonGet(t, spec, "description").(string); desc != "List files" {
		t.Errorf("tool description: got %q", desc)
	}
	schema := jsonGet(t, spec, "inputSchema.json")
	if typ := jsonGet(t, schema, "type").(string); typ != "object" {
		t.Errorf("schema type: got %q want object", typ)
	}
	required, ok := jsonGet(t, schema, "required").([]any)
	if !ok || len(required) != 1 || required[0] != "path" {
		t.Errorf("schema required: got %v", required)
	}
}

func TestBuildKiroPayload_BackwardCompatNoTools(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"Hi"}
		]
	}`)
	exec := &KiroExecutor{}
	out, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("buildKiroPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// No tools, no userInputMessageContext on currentMessage.
	uim := jsonGet(t, got, "conversationState.currentMessage.userInputMessage").(map[string]any)
	if _, has := uim["userInputMessageContext"]; has {
		t.Errorf("currentMessage should not carry userInputMessageContext when no tools/results: got %v", uim["userInputMessageContext"])
	}
	content, _ := uim["content"].(string)
	// system+user both normalize to user role → merge into one turn,
	// which becomes currentMessage.
	if !strings.Contains(content, "Hi") {
		t.Errorf("currentMessage content missing user text, got %q", content)
	}
	if !strings.Contains(content, "You are helpful") {
		t.Errorf("currentMessage should also contain merged system text, got %q", content)
	}
	if !strings.Contains(content, "[Context: Current time is") {
		t.Errorf("currentMessage missing context marker, got %q", content)
	}

	// History should be empty since the only turn becomes currentMessage.
	history := jsonGet(t, got, "conversationState.history").([]any)
	if len(history) != 0 {
		t.Errorf("expected empty history when only system+user merge into one turn, got %d", len(history))
	}
}

func TestBuildKiroPayload_HistoryRetainedWithAssistantTurn(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello there"},
			{"role":"user","content":"thanks"}
		]
	}`)
	exec := &KiroExecutor{}
	out, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("buildKiroPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	history := jsonGet(t, got, "conversationState.history").([]any)
	if len(history) != 2 {
		t.Fatalf("expected 2 history entries (user + assistant), got %d: %#v", len(history), history)
	}
	if _, ok := history[0].(map[string]any)["userInputMessage"]; !ok {
		t.Errorf("history[0] should be userInputMessage")
	}
	if _, ok := history[1].(map[string]any)["assistantResponseMessage"]; !ok {
		t.Errorf("history[1] should be assistantResponseMessage")
	}
	curContent := jsonGet(t, got, "conversationState.currentMessage.userInputMessage.content").(string)
	if !strings.Contains(curContent, "thanks") {
		t.Errorf("currentMessage missing trailing user text: %q", curContent)
	}
}

func TestBuildKiroPayload_MultiTurnWithToolCallsAndResult(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"read README"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"# Project Title"},
			{"role":"user","content":"summarize it"}
		],
		"tools":[{"type":"function","function":{"name":"read_file","description":"Read file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]
	}`)
	exec := &KiroExecutor{}
	out, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("buildKiroPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	history := jsonGet(t, got, "conversationState.history").([]any)
	if len(history) < 2 {
		t.Fatalf("expected >=2 history entries, got %d: %#v", len(history), history)
	}

	// First history entry: user "read README". History user turns must
	// NOT carry tools (those moved to currentMessage).
	first := history[0].(map[string]any)
	uim, ok := first["userInputMessage"].(map[string]any)
	if !ok {
		t.Fatalf("history[0] not a userInputMessage: %#v", first)
	}
	if c, _ := uim["content"].(string); !strings.Contains(c, "read README") {
		t.Errorf("history[0] missing initial user text: %q", c)
	}
	if ctx, has := uim["userInputMessageContext"].(map[string]any); has {
		if _, hasTools := ctx["tools"]; hasTools {
			t.Errorf("history[0] should not carry tools spec; got %v", ctx["tools"])
		}
	}

	// Second history entry: assistant with tool_calls -> assistantResponseMessage.toolUses
	second := history[1].(map[string]any)
	arm, ok := second["assistantResponseMessage"].(map[string]any)
	if !ok {
		t.Fatalf("history[1] not assistantResponseMessage: %#v", second)
	}
	tu, ok := arm["toolUses"].([]any)
	if !ok || len(tu) != 1 {
		t.Fatalf("assistant.toolUses missing or wrong: %#v", arm["toolUses"])
	}
	tuMap := tu[0].(map[string]any)
	if id, _ := tuMap["toolUseId"].(string); id != "call_1" {
		t.Errorf("toolUseId: got %q want call_1", id)
	}
	if name, _ := tuMap["name"].(string); name != "read_file" {
		t.Errorf("toolUse.name: got %q want read_file", name)
	}
	if input, ok := tuMap["input"].(map[string]any); !ok || input["path"] != "README.md" {
		t.Errorf("toolUse.input parse failed: %v", tuMap["input"])
	}

	// currentMessage should be the trailing user "summarize it" with
	// toolResults for call_1 attached (from the tool role) — and the
	// tools spec re-attached so upstream sees it.
	curUIM := jsonGet(t, got, "conversationState.currentMessage.userInputMessage").(map[string]any)
	curContent, _ := curUIM["content"].(string)
	if !strings.Contains(curContent, "summarize it") {
		t.Errorf("currentMessage content missing trailing user text: %q", curContent)
	}
	curCtx, ok := curUIM["userInputMessageContext"].(map[string]any)
	if !ok {
		t.Fatalf("currentMessage missing userInputMessageContext: %#v", curUIM)
	}
	results, ok := curCtx["toolResults"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("currentMessage toolResults missing or wrong: %#v", curCtx["toolResults"])
	}
	rMap := results[0].(map[string]any)
	if id, _ := rMap["toolUseId"].(string); id != "call_1" {
		t.Errorf("toolResult.toolUseId: got %q want call_1", id)
	}
	innerArr, _ := rMap["content"].([]any)
	if len(innerArr) != 1 {
		t.Fatalf("toolResult.content malformed: %#v", rMap["content"])
	}
	if txt, _ := innerArr[0].(map[string]any)["text"].(string); txt != "# Project Title" {
		t.Errorf("toolResult text: got %q", txt)
	}
	if _, hasTools := curCtx["tools"]; !hasTools {
		t.Errorf("currentMessage should re-carry tools spec for upstream")
	}
}

func TestBuildKiroPayload_DeterministicConversationID(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	exec := &KiroExecutor{}
	out1, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	out2, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	var p1, p2 map[string]any
	_ = json.Unmarshal(out1, &p1)
	_ = json.Unmarshal(out2, &p2)
	id1 := jsonGet(t, p1, "conversationState.conversationId").(string)
	id2 := jsonGet(t, p2, "conversationState.conversationId").(string)
	if id1 != id2 {
		t.Errorf("conversationId not deterministic: %q vs %q", id1, id2)
	}
}

func TestBuildKiroPayload_ProfileArnPropagated(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	exec := &KiroExecutor{}
	out, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{
		Metadata: map[string]any{"profile_arn": "arn:aws:iam::1234:role/dev"},
	})
	if err != nil {
		t.Fatalf("buildKiroPayload: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	arn, _ := got["profileArn"].(string)
	if arn != "arn:aws:iam::1234:role/dev" {
		t.Errorf("profileArn not propagated: %q", arn)
	}
}

func TestBuildKiroPayload_ImageBase64Forwarded(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this?"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANS"}}
		]}]
	}`)
	exec := &KiroExecutor{}
	out, err := exec.buildKiroPayload(body, "claude-opus-4.5", &cliproxyauth.Auth{})
	if err != nil {
		t.Fatalf("buildKiroPayload: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	uim := jsonGet(t, got, "conversationState.currentMessage.userInputMessage").(map[string]any)
	imgs, ok := uim["images"].([]any)
	if !ok || len(imgs) != 1 {
		t.Fatalf("currentMessage.images missing: %#v", uim)
	}
	img := imgs[0].(map[string]any)
	if format, _ := img["format"].(string); format != "png" {
		t.Errorf("image format: got %q", format)
	}
	src, _ := img["source"].(map[string]any)
	if bytes, _ := src["bytes"].(string); bytes != "iVBORw0KGgoAAAANS" {
		t.Errorf("image bytes: got %q", bytes)
	}
}

// Regression: the OpenAI->Claude translator hard-rejects any chunk that
// does not start with "data:" and only emits message_stop when the
// terminating chunk carries a non-null usage block. Both invariants must
// be honored by the helpers Kiro's stream path uses.
func TestBuildSSEChunk_HasDataPrefix(t *testing.T) {
	chunk := buildSSEChunk("id-1", 1700000000, "claude-opus-4.6", map[string]any{"role": "assistant"}, "")
	if !strings.HasPrefix(string(chunk), "data: ") {
		t.Fatalf("buildSSEChunk missing 'data: ' prefix: %q", string(chunk))
	}
}

func TestBuildSSEChunkWithUsage_FinalChunkCarriesUsage(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":     int64(10),
		"completion_tokens": int64(5),
		"total_tokens":      int64(15),
	}
	chunk := buildSSEChunkWithUsage("id-2", 1700000000, "claude-opus-4.6", nil, "tool_calls", usage)
	if !strings.HasPrefix(string(chunk), "data: ") {
		t.Fatalf("missing 'data: ' prefix: %q", string(chunk))
	}
	// Strip the SSE prefix and decode the payload.
	body := strings.TrimPrefix(string(chunk), "data: ")
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gotUsage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("final chunk missing usage: %#v", got)
	}
	if v, _ := gotUsage["total_tokens"].(float64); v != 15 {
		t.Errorf("total_tokens: got %v", gotUsage["total_tokens"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %#v", choices)
	}
	choice := choices[0].(map[string]any)
	if reason, _ := choice["finish_reason"].(string); reason != "tool_calls" {
		t.Errorf("finish_reason: got %q want tool_calls", reason)
	}
}
