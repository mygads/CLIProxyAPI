package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestWriteErrorResponse_AddonHeadersDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":  {"30"},
			"X-Request-Id": {"req-1"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After should be empty when passthrough is disabled, got %q", got)
	}
	if got := recorder.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id should be empty when passthrough is disabled, got %q", got)
	}
}

func TestWriteErrorResponse_AddonHeadersEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Writer.Header().Set("X-Request-Id", "old-value")

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":  {"30"},
			"X-Request-Id": {"new-1", "new-2"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
	if got := recorder.Header().Values("X-Request-Id"); !reflect.DeepEqual(got, []string{"new-1", "new-2"}) {
		t.Fatalf("X-Request-Id = %#v, want %#v", got, []string{"new-1", "new-2"})
	}
}

func TestEnrichAuthSelectionError_DefaultsTo503WithContext(t *testing.T) {
	in := &coreauth.Error{Code: "auth_not_found", Message: "no auth available"}
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusServiceUnavailable)
	}
	// Must NOT expose internal provider/model/management details to customers.
	if strings.Contains(got.Message, "providers=") {
		t.Fatalf("message leaks provider context: %q", got.Message)
	}
	if strings.Contains(got.Message, "model=") {
		t.Fatalf("message leaks model context: %q", got.Message)
	}
	if strings.Contains(got.Message, "/v0/management/") {
		t.Fatalf("message leaks management endpoint: %q", got.Message)
	}
	if got.Message == "" {
		t.Fatalf("message should not be empty")
	}
}

func TestEnrichAuthSelectionError_PreservesExplicitStatus(t *testing.T) {
	in := &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusTooManyRequests}
	out := enrichAuthSelectionError(in, []string{"gemini"}, "gemini-2.5-pro")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusTooManyRequests)
	}
}

func TestEnrichAuthSelectionError_IgnoresOtherErrors(t *testing.T) {
	in := errors.New("boom")
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")
	if out != in {
		t.Fatalf("expected original error to be returned unchanged")
	}
}

func TestSanitizeErrorText_PreservesSafeMessages(t *testing.T) {
	tests := []struct {
		name    string
		errText string
		status  int
		want    string
	}{
		{
			name:    "unknown provider error is safe",
			errText: "unknown provider for model genfity/Qwen-3.7-Max",
			status:  http.StatusBadGateway,
			want:    "unknown provider for model genfity/Qwen-3.7-Max",
		},
		{
			name:    "litellm error contains internal leak",
			errText: "litellm.exceptions.APIConnectionError: Connection refused",
			status:  http.StatusInternalServerError,
			want:    customerGatewayBusyMessage,
		},
		{
			name:    "mtr prefix error contains internal leak",
			errText: "mtr/anthropic claude-3-opus-20240229 rate limit exceeded",
			status:  http.StatusTooManyRequests,
			want:    customerGatewayBusyMessage,
		},
		{
			name:    "generic error is safe",
			errText: "rate limit exceeded",
			status:  http.StatusTooManyRequests,
			want:    "rate limit exceeded",
		},
		{
			name:    "empty error",
			errText: "",
			status:  http.StatusBadRequest,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeErrorText(tt.errText, tt.status)
			if got != tt.want {
				t.Errorf("sanitizeErrorText(%q, %d) = %q, want %q", tt.errText, tt.status, got, tt.want)
			}
		})
	}
}

func TestBuildErrorResponseBodyMasksProviderCatalogError(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"code":"invalid_request_error","message":"Model \"deepseek-v4-pro\" is not available in current public model catalog.","type":"invalid_request_error","upstream_status":400}}`)

	if strings.Contains(string(body), "deepseek") || strings.Contains(string(body), "model catalog") {
		t.Fatalf("provider error leaked: %s", body)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != customerGatewayBusyMessage {
		t.Fatalf("message = %q, want %q", payload.Error.Message, customerGatewayBusyMessage)
	}
}

func TestSanitizePublicResponseRewritesModelAndDropsReasoningContent(t *testing.T) {
	body := []byte(`{"id":"x","object":"chat.completion","model":"deepseek/deepseek-v4-pro","choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"provider thoughts"}}]}`)
	out := SanitizePublicResponse(body, "genfity/claude-opus-4.7")

	if strings.Contains(string(out), "deepseek") || strings.Contains(string(out), "reasoning_content") || strings.Contains(string(out), "provider thoughts") {
		t.Fatalf("internal response detail leaked: %s", out)
	}
	if !strings.Contains(string(out), "genfity/claude-opus-4.7") {
		t.Fatalf("public model missing: %s", out)
	}
}

func TestSanitizePublicResponseStripsThinkingTagsFromContent(t *testing.T) {
	body := []byte(`{"id":"x","object":"chat.completion","model":"kr/claude-haiku-4.5-thinking-agentic","choices":[{"message":{"role":"assistant","content":"<thinking>\ninternal reasoning\n</thinking>\n\nok"}}]}`)
	out := SanitizePublicResponse(body, "genfity/claude-haiku-4.5")

	text := string(out)
	if strings.Contains(strings.ToLower(text), "<thinking>") || strings.Contains(text, "internal reasoning") {
		t.Fatalf("thinking leak remained: %s", out)
	}
	if !strings.Contains(text, `"content":"ok"`) {
		t.Fatalf("sanitized content missing: %s", out)
	}
	if !strings.Contains(text, "genfity/claude-haiku-4.5") {
		t.Fatalf("public model missing: %s", out)
	}
}

func TestSanitizePublicResponse_RewritesSSEChunkAndMasksProviderPrelude(t *testing.T) {
	body := []byte(": genflowaistreamopen\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"genflowai/claude-opus-4.8-thinking-agentic\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"The\",\"content\":\"<thinking>internal reasoning</thinking>ok\"},\"finish_reason\":null}]}\n\n")
	out := SanitizePublicResponse(body, "genfity/claude-opus-4.8")

	text := string(out)
	if strings.Contains(text, "genflowai/claude-opus-4.8-thinking-agentic") || strings.Contains(text, "reasoning_content") || strings.Contains(text, "internal reasoning") {
		t.Fatalf("internal stream detail leaked: %s", out)
	}
	if strings.Contains(text, "genflowaistreamopen") {
		t.Fatalf("provider prelude comment leaked: %s", out)
	}
	if !strings.Contains(text, "genfity/claude-opus-4.8") {
		t.Fatalf("public model missing from stream chunk: %s", out)
	}
	if !strings.Contains(text, `"content":"ok"`) {
		t.Fatalf("sanitized stream content missing: %s", out)
	}
}

func TestSanitizePublicStream_StripsThinkingAcrossChunks(t *testing.T) {
	in := make(chan []byte, 6)
	in <- []byte(": connected -----\n\n")
	in <- []byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}],\"model\":\"genflowai/claude-haiku-4.5-thinking-agentic\",\"object\":\"chat.completion.chunk\"}\n\n")
	in <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\"<thinking>internal\"},\"index\":0}],\"model\":\"genflowai/claude-haiku-4.5-thinking-agentic\",\"object\":\"chat.completion.chunk\"}\n\n")
	in <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\" reasoning\"},\"index\":0}],\"model\":\"genflowai/claude-haiku-4.5-thinking-agentic\",\"object\":\"chat.completion.chunk\"}\n\n")
	in <- []byte("data: {\"choices\":[{\"delta\":{\"content\":\"</thinking>ok\"},\"index\":0}],\"model\":\"genflowai/claude-haiku-4.5-thinking-agentic\",\"object\":\"chat.completion.chunk\"}\n\n")
	in <- []byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"model\":\"genflowai/claude-haiku-4.5-thinking-agentic\",\"object\":\"chat.completion.chunk\"}\n\n")
	close(in)

	var chunks []string
	for chunk := range sanitizePublicStream(in, "genfity/claude-haiku-4.5") {
		chunks = append(chunks, string(chunk))
	}
	joined := strings.Join(chunks, "")
	if strings.Contains(strings.ToLower(joined), "<thinking>") || strings.Contains(joined, "internal reasoning") {
		t.Fatalf("thinking leaked across chunks: %s", joined)
	}
	if strings.Contains(joined, ": connected") {
		t.Fatalf("provider prelude leaked: %s", joined)
	}
	if strings.Contains(joined, `"content":""`) {
		t.Fatalf("empty content chunks should be suppressed: %s", joined)
	}
	if strings.Contains(joined, "genflowai/claude-haiku-4.5-thinking-agentic") {
		t.Fatalf("upstream model leaked: %s", joined)
	}
	if !strings.Contains(joined, "genfity/claude-haiku-4.5") || !strings.Contains(joined, "\"finish_reason\":\"stop\"") || !strings.Contains(joined, "\"content\":\"ok\"") {
		t.Fatalf("expected sanitized visible stream payload, got %s", joined)
	}
}
