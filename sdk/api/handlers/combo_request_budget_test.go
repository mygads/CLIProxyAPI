package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func comboBudgetTestContext(t *testing.T, budget string) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if budget != "" {
		ginCtx.Request.Header.Set(comboRequestBudgetHeader, budget)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestComboAttemptIndexForBudgetReservesFinalFallback(t *testing.T) {
	h := &BaseAPIHandler{}
	attempts := []modelAttempt{
		{Model: "primary"},
		{Model: "middle-one"},
		{Model: "middle-two"},
		{Model: "final", IsLast: true},
	}
	ctx := comboBudgetTestContext(t, "330000")

	if got := h.comboAttemptIndexForBudget(ctx, time.Now().Add(-150*time.Second), attempts, 1, "combo"); got != len(attempts)-1 {
		t.Fatalf("attempt index=%d want final index=%d", got, len(attempts)-1)
	}
	if got := h.comboAttemptIndexForBudget(ctx, time.Now().Add(-140*time.Second), attempts, 1, "combo"); got != 1 {
		t.Fatalf("attempt index=%d want current index=1 while enough budget remains", got)
	}
}

func TestComboAttemptIndexForBudgetAlwaysPreservesPrimaryAndNoHeaderBehavior(t *testing.T) {
	h := &BaseAPIHandler{}
	attempts := []modelAttempt{{Model: "primary"}, {Model: "middle"}, {Model: "final", IsLast: true}}
	started := time.Now().Add(-5 * time.Minute)

	if got := h.comboAttemptIndexForBudget(comboBudgetTestContext(t, "330000"), started, attempts, 0, "combo"); got != 0 {
		t.Fatalf("primary was skipped: index=%d", got)
	}
	if got := h.comboAttemptIndexForBudget(comboBudgetTestContext(t, ""), started, attempts, 1, "combo"); got != 1 {
		t.Fatalf("headerless request changed behavior: index=%d", got)
	}
}
