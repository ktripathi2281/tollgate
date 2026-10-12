package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/budget"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/idempotency"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/router"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// Budgets places and resolves spend holds. budget.Budgets is the
// implementation.
type Budgets interface {
	Hold(ctx context.Context, key auth.Key, amount money.Micros, requestID string) (*budget.Hold, error)
	Settle(ctx context.Context, h *budget.Hold, cost money.Micros)
	Release(ctx context.Context, h *budget.Hold)
}

// Idempotency handles requests that send an Idempotency-Key.
// idempotency.Keys is the implementation.
type Idempotency interface {
	Begin(ctx context.Context, keyID int64, idemKey string, body []byte, requestID string) (idempotency.Result, error)
	Complete(ctx context.Context, l *idempotency.Lock, status int, body []byte) error
	Abandon(ctx context.Context, l *idempotency.Lock) error
}

// HeaderReplayed marks a response replayed from an idempotency key.
const HeaderReplayed = "Idempotent-Replayed"

// persistTimeout bounds each database write that persists a request's
// outcome. Those writes use a context that isn't cancelled with the
// request: a client that hangs up must not stop its hold being settled.
const persistTimeout = 10 * time.Second

// accounting is what a request has taken since it was admitted, in
// pipeline steps 6 to 8, and what it spent. Each thing taken is given back
// exactly once: by finish, before the response is written, or by the
// calls deferred as it was taken, on any other path, panics included.
// The deferred calls run in reverse order, which is the unwind rule.
type accounting struct {
	s         *Server
	ctx       context.Context // not cancelled with the request
	key       auth.Key
	requestID string

	idem     *idempotency.Lock // step 6; nil without an Idempotency-Key
	idemDone bool
	reserved int          // step 7: tokens per minute
	hold     *budget.Hold // step 8; nil without a budget, or for a free request
	holdDone bool

	used  int          // tokens used, once known
	cost  money.Micros // what they cost
	spent bool         // whether used and cost are known
}

func (s *Server) newAccounting(r *http.Request, key auth.Key) *accounting {
	return &accounting{
		s:         s,
		ctx:       context.WithoutCancel(r.Context()),
		key:       key,
		requestID: RequestID(r.Context()),
	}
}

// recordUsage notes what the request used, priced at the target that
// served it.
func (a *accounting) recordUsage(providerName, model string, u provider.Usage) {
	a.used = u.PromptTokens + u.CompletionTokens
	a.cost = usage.Cost(a.s.pricing[config.PriceKey(providerName, model)], u.PromptTokens, u.CompletionTokens)
	a.spent = true
}

// finish persists the request's outcome (pipeline step 10). The hold is
// settled with the cost, or released if nothing was spent. A response body
// is stored under the idempotency key; with no body, the key's record is
// removed so the client can retry, since only successes are stored.
func (a *accounting) finish(status int, body []byte) {
	a.endHold()
	a.endIdempotency(status, body)
}

// endHold settles or releases the hold, once.
func (a *accounting) endHold() {
	if a.hold == nil || a.holdDone {
		return
	}
	a.holdDone = true
	ctx, cancel := context.WithTimeout(a.ctx, persistTimeout)
	defer cancel()
	if a.spent && a.used > 0 {
		a.s.budgets.Settle(ctx, a.hold, a.cost)
	} else {
		a.s.budgets.Release(ctx, a.hold)
	}
}

// endIdempotency completes or removes the idempotency record, once.
func (a *accounting) endIdempotency(status int, body []byte) {
	if a.idem == nil || a.idemDone {
		return
	}
	a.idemDone = true
	ctx, cancel := context.WithTimeout(a.ctx, persistTimeout)
	defer cancel()
	if body != nil {
		if err := a.s.idem.Complete(ctx, a.idem, status, body); err != nil {
			a.s.log.ErrorContext(ctx, "storing the response under its idempotency key failed; "+
				"a retry with the key runs the request again once the lock expires",
				"request_id", a.requestID, "error", err)
		}
		return
	}
	if err := a.s.idem.Abandon(ctx, a.idem); err != nil {
		a.s.log.ErrorContext(ctx, "removing a failed request's idempotency key failed; "+
			"retries with the key get 409 until the lock expires",
			"request_id", a.requestID, "error", err)
	}
}

// refundTokens gives back the reserved tokens the request didn't use
// (pipeline step 12).
func (a *accounting) refundTokens() {
	a.s.limiter.RefundTokens(a.key.ID, limitsOf(a.key), a.reserved, a.used)
}

// worstCase returns the most a request can cost on any of the alias's
// targets: an upper bound on its input at the input price, plus maxTokens
// at the output price (brief section 9.1).
func (s *Server) worstCase(alias *router.Alias, messages []provider.Message, maxTokens int) (money.Micros, error) {
	bound := usage.InputBound(messages)
	var worst money.Micros
	for _, t := range alias.Targets {
		key := config.PriceKey(t.Provider.Name(), t.Model)
		price, ok := s.pricing[key]
		if !ok {
			return 0, fmt.Errorf("no price for %s", key)
		}
		worst = max(worst, usage.Cost(price, bound, maxTokens))
	}
	return worst, nil
}

// budgetExceeded is the 402 for a request the key's budget can't cover.
func budgetExceeded(key auth.Key, amount money.Micros, now time.Time) *api.Error {
	reset := budget.PeriodEnd(key.BudgetPeriod, budget.PeriodStart(key.BudgetPeriod, now))
	return &api.Error{
		Status: http.StatusPaymentRequired, Type: api.TypeInsufficientQuota, Code: api.CodeInsufficientQuota,
		Message: fmt.Sprintf("This key's %s budget of %s can't cover this request, which could cost up to %s. "+
			"The budget resets at %s.", key.BudgetPeriod, key.Budget, amount, reset.Format(time.RFC3339)),
	}
}

// idempotencyError is the response to a request whose Idempotency-Key
// can't be used now, or nil for Proceed and Replay.
func idempotencyError(res idempotency.Result) *api.Error {
	switch res.Outcome {
	case idempotency.InProgress:
		return &api.Error{
			Status: http.StatusConflict, Type: api.TypeInvalidRequest, Code: api.CodeIdempotencyInProgress,
			Message:    "A request with this Idempotency-Key is still running. Retry after it finishes.",
			RetryAfter: res.RetryAfter,
		}
	case idempotency.Mismatch:
		return &api.Error{
			Status: http.StatusUnprocessableEntity, Type: api.TypeInvalidRequest, Code: api.CodeIdempotencyKeyReused,
			Message: "This Idempotency-Key was already used with a different request body. Use a new key.",
		}
	case idempotency.NotStored:
		return &api.Error{
			Status: http.StatusUnprocessableEntity, Type: api.TypeInvalidRequest, Code: api.CodeIdempotencyKeyReused,
			Message: "A request with this Idempotency-Key completed, but its response was too large to store, " +
				"so it can't be replayed. Use a new key.",
		}
	}
	return nil
}

// replay sends a stored response again. A streaming request gets it as a
// minimal stream: the whole reply in one content event. The same request
// body always asks for the same form, so the request decides.
func (s *Server) replay(w http.ResponseWriter, res idempotency.Result, stream, includeUsage bool) {
	h := w.Header()
	h.Set(HeaderReplayed, "true")
	h.Set(HeaderAttempts, "0")
	if !stream {
		h.Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
		return
	}

	var c api.ChatCompletion
	if err := json.Unmarshal(res.Body, &c); err != nil || len(c.Choices) != 1 {
		s.log.Error("a stored response can't be replayed as a stream", "error", err)
		api.WriteError(w, &api.Error{
			Status: http.StatusInternalServerError, Type: api.TypeServer, Code: api.CodeInternal,
			Message: "The stored response for this Idempotency-Key can't be replayed.",
		})
		return
	}
	startEventStream(w)
	out := &eventSender{sse: api.NewSSEWriter(w), writeTimeout: s.timeouts.Idle}
	defer out.clearDeadline()
	chunks := api.NewChunks(c.ID, time.Unix(c.Created, 0), c.Model)
	events := []any{
		chunks.Role(),
		chunks.Content(c.Choices[0].Message.Content),
		chunks.Finish(provider.FinishReason(c.Choices[0].FinishReason)),
	}
	if includeUsage {
		events = append(events, chunks.Usage(provider.Usage{
			PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens,
		}))
	}
	for _, e := range events {
		if out.send(e) != nil {
			return
		}
	}
	_ = out.done()
}
