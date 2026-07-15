package logging

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGinLoggerPreservesIncomingRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const requestID = "gateway-request-123"
	var contextID string
	router := gin.New()
	router.Use(GinLogrusLogger())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		contextID = GetRequestID(c.Request.Context())
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Request-ID", requestID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if contextID != requestID || rec.Header().Get("X-Request-ID") != requestID {
		t.Fatalf("context=%q response=%q", contextID, rec.Header().Get("X-Request-ID"))
	}
}
