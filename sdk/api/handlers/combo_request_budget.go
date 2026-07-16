package handlers

import (
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

const comboRequestBudgetHeader = "X-Genfity-Request-Budget-Ms"

var (
	comboFinalAttemptReserve    = 60 * time.Second
	comboBudgetTransportReserve = 5 * time.Second
)

// comboRequestBudget returns the caller's total upstream budget. The gateway
// sets this internal header from its HTTP client timeout so CLIProxy can avoid
// spending the entire window on middle candidates and starving the final
// fallback. Untrusted or unreasonable values are ignored/capped.
func comboRequestBudget(ctx context.Context) time.Duration {
	headers := headersFromContext(ctx)
	if headers == nil {
		return 0
	}
	milliseconds, err := strconv.ParseInt(strings.TrimSpace(headers.Get(comboRequestBudgetHeader)), 10, 64)
	if err != nil || milliseconds < 1_000 {
		return 0
	}
	maximum := 30 * time.Minute
	if milliseconds > maximum.Milliseconds() {
		return maximum
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// comboAttemptIndexForBudget preserves the first candidate, then jumps to the
// configured final fallback when another full middle attempt would consume
// the time reserved for that final candidate. Attempts remain sequential, so
// this improves reachability without introducing double-billing from hedging.
func (h *BaseAPIHandler) comboAttemptIndexForBudget(ctx context.Context, started time.Time, attempts []modelAttempt, index int, routeName string) int {
	last := len(attempts) - 1
	if index <= 0 || index >= last {
		return index
	}
	budget := comboRequestBudget(ctx)
	if budget <= 0 {
		return index
	}
	remaining := budget - time.Since(started) - comboBudgetTransportReserve
	if deadline, ok := ctx.Deadline(); ok {
		if deadlineRemaining := time.Until(deadline) - comboBudgetTransportReserve; deadlineRemaining < remaining {
			remaining = deadlineRemaining
		}
	}
	required := h.comboAttemptTimeout() + comboFinalAttemptReserve
	if remaining > required {
		return index
	}
	log.WithFields(log.Fields{
		"request_id":       logging.GetRequestID(ctx),
		"combo":            routeName,
		"routing_reason":   "reserve_final_budget",
		"skipped_entries":  last - index,
		"remaining_budget": remaining,
		"required_budget":  required,
		"final_candidate":  attempts[last].Model,
	}).Info("skipping middle combo candidates to preserve final fallback budget")
	return last
}
