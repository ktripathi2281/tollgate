package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/budget"
	"github.com/ktripathi2281/tollgate/internal/idempotency"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/router"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client has gone; there is nobody to tell.
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

// handleChatCompletions serves POST /v1/chat/completions. The steps follow
// the request pipeline in brief section 5; authentication and the
// requests-per-minute limit (steps 3 and 4) have already run, as
// middleware.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	received := s.now()
	ctx := r.Context()
	key := keyFrom(ctx)

	// Parse and validate the body, size-limited.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			api.WriteError(w, &api.Error{
				Status: http.StatusRequestEntityTooLarge, Type: api.TypeInvalidRequest, Code: api.CodeRequestTooLarge,
				Message: fmt.Sprintf("The request body is larger than %d bytes.", s.cfg.MaxBodyBytes),
			})
			return
		}
		// Reading failed for another reason, usually a client that went
		// away mid-upload. There is nobody to answer.
		s.log.Debug("reading request body", "request_id", RequestID(r.Context()), "error", err)
		return
	}
	req, err := api.ParseChatCompletionRequest(body)
	if err != nil {
		api.WriteError(w, toAPIError(err))
		return
	}

	// An alias the key may not use looks the same as one that doesn't
	// exist, so a key can't discover what other keys can use.
	alias, ok := s.router.Alias(req.Model)
	if !ok || !key.Allows(alias.Name) {
		api.WriteError(w, &api.Error{
			Status: http.StatusNotFound, Type: api.TypeInvalidRequest, Code: api.CodeModelNotFound, Param: "model",
			Message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", req.Model),
		})
		return
	}

	// Every upstream call gets a real limit on output tokens: the client's,
	// or the alias default. The budget hold depends on this bound.
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = alias.DefaultMaxTokens
	}
	if maxTokens > alias.MaxTokensCeiling {
		api.WriteError(w, &api.Error{
			Status: http.StatusBadRequest, Type: api.TypeInvalidRequest, Code: api.CodeInvalidValue, Param: "max_tokens",
			Message: fmt.Sprintf("Invalid token limit: model `%s` allows at most %d output tokens, got %d.",
				alias.Name, alias.MaxTokensCeiling, maxTokens),
		})
		return
	}

	// From here on the request takes things that must be given back:
	// an idempotency record, a token reservation, a budget hold. Each is
	// given back by a deferred call registered as it is taken, so the
	// unwind happens in reverse order on every exit path, panics included.
	// The normal paths call a.finish first, so the outcome is saved before
	// the client sees the response; the deferred calls then do nothing.
	a := s.newAccounting(r, key)

	// Step 6: idempotency. A retry of a completed request gets the stored
	// response, with no provider call, no reservation and no hold.
	if v := r.Header.Get(idempotency.Header); v != "" {
		idemKey, err := idempotency.ParseKey(v)
		if err != nil {
			api.WriteError(w, &api.Error{
				Status: http.StatusBadRequest, Type: api.TypeInvalidRequest, Code: api.CodeInvalidIdempotency,
				Message: err.Error() + ".",
			})
			return
		}
		res, err := s.idem.Begin(ctx, key.ID, idemKey, body, a.requestID)
		if err != nil {
			s.unavailable(w, r, api.CodeIdempotencyUnavailable, "Idempotency keys", err)
			return
		}
		switch res.Outcome {
		case idempotency.Replay:
			s.replay(w, res, req.Stream, req.IncludeUsage)
			return
		case idempotency.Proceed:
			a.idem = res.Lock
			defer a.endIdempotency(0, nil)
		default:
			api.WriteError(w, idempotencyError(res))
			return
		}
	}

	// Step 7: reserve tokens per minute, the estimated prompt plus the most
	// the reply may use. What the request didn't use is refunded once the
	// response is written (step 12).
	a.reserved = usage.EstimatePrompt(req.Messages) + maxTokens
	if apiErr := s.reserveTokens(key, a.reserved); apiErr != nil {
		a.finish(0, nil)
		api.WriteError(w, apiErr)
		return
	}
	defer a.refundTokens()

	// Step 8: hold the worst-case cost against the key's budget.
	if key.Budget > 0 {
		amount, err := s.worstCase(alias, req.Messages, maxTokens)
		if err != nil {
			// Config validation gives every target a price, so this is a bug.
			a.finish(0, nil)
			s.internalError(w, r, err)
			return
		}
		hold, err := s.budgets.Hold(ctx, key, amount, a.requestID)
		switch {
		case errors.Is(err, budget.ErrExceeded):
			a.finish(0, nil)
			api.WriteError(w, budgetExceeded(key, amount, received))
			return
		case err != nil:
			a.finish(0, nil)
			s.unavailable(w, r, api.CodeBudgetUnavailable, "Budgets", err)
			return
		}
		a.hold = hold
		defer a.endHold()
	}

	// Step 9: route and call the provider.
	creq := provider.ChatRequest{
		Messages:    req.Messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	if req.Stream {
		s.streamChat(w, r, alias, creq, req.IncludeUsage, received, a)
		return
	}

	resp, result, err := s.router.Chat(ctx, alias, creq)
	s.logTries(ctx, alias.Name, result, err)
	w.Header().Set(HeaderProvider, result.Provider)
	w.Header().Set(HeaderAttempts, strconv.Itoa(result.Attempts))
	if err != nil {
		a.finish(0, nil)
		if apiErr := s.upstreamError(ctx, err); apiErr != nil {
			api.WriteError(w, apiErr)
		}
		return
	}

	// Steps 10 and 11: save the outcome, then send it. If the process
	// stopped in between, a retry with the same idempotency key would get
	// the stored response instead of a second provider call.
	u, _ := usageOrEstimate(resp.Usage, creq, resp.Content)
	a.recordUsage(result.Provider, result.Model, u)
	out, err := json.Marshal(api.NewChatCompletion("chatcmpl-"+rand.Text(), received, resp, u))
	if err != nil {
		a.finish(0, nil)
		s.internalError(w, r, err)
		return
	}
	a.finish(http.StatusOK, out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// internalError answers with a 500 for a bug, and logs it.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "internal error", "request_id", RequestID(r.Context()), "error", err)
	api.WriteError(w, &api.Error{
		Status: http.StatusInternalServerError, Type: api.TypeServer, Code: api.CodeInternal,
		Message: "The gateway hit an internal error.",
	})
}

// unavailable answers a request when a database-backed step failed for a
// reason other than the client going away.
func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, code, what string, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	s.log.ErrorContext(r.Context(), "database step failed", "request_id", RequestID(r.Context()), "code", code, "error", err)
	api.WriteError(w, &api.Error{
		Status: http.StatusServiceUnavailable, Type: api.TypeServer, Code: code,
		Message: what + " can't be checked right now. Retry shortly.", RetryAfter: time.Second,
	})
}

// errShuttingDown is the cause given to every request context still running
// when the shutdown grace period ends.
var errShuttingDown = errors.New("the gateway is shutting down")

// upstreamError turns a failed upstream call into the error to send the
// client (brief sections 6 and 7). It returns nil when the client has gone,
// since there is nobody left to tell. The client gets a generic message,
// except when the problem is its own request; the detail is in the log.
func (s *Server) upstreamError(ctx context.Context, err error) *api.Error {
	// The request can't go to one of the alias's targets as it is.
	if verr, ok := errors.AsType[*router.InvalidRequestError](err); ok {
		return &api.Error{
			Status: http.StatusBadRequest, Type: api.TypeInvalidRequest, Code: api.CodeInvalidValue,
			Param: verr.Param, Message: fmt.Sprintf("Model `%s` can't take this request: %s", verr.Alias, verr.Message),
		}
	}

	class := provider.ClassOf(err)
	if class == provider.Cancelled {
		if errors.Is(context.Cause(ctx), errShuttingDown) {
			return &api.Error{
				Status: http.StatusServiceUnavailable, Type: api.TypeServer, Code: api.CodeShuttingDown,
				Message: "The gateway is shutting down. Retry the request.", RetryAfter: time.Second,
			}
		}
		return nil
	}

	// Every target failed or was skipped. Capacity problems, an open
	// breaker or a rate limit, can clear up, so they are a 503 with a hint
	// of when; an outright failure is a 502, or a 504 if time ran out.
	if ex, ok := errors.AsType[*router.ExhaustedError](err); ok {
		switch {
		case ex.CapacityOnly:
			apiErr := &api.Error{
				Status: http.StatusServiceUnavailable, Type: api.TypeServer, Code: api.CodeUpstreamUnavailable,
				Message:    "Every upstream provider for this model is unavailable right now. Retry later.",
				RetryAfter: max(ex.RetryAfter, time.Second),
			}
			if ex.RateLimited {
				apiErr.Code = api.CodeUpstreamRateLimited
				apiErr.Message = "Every upstream provider for this model is rate limited. Retry later."
			}
			return apiErr
		case errors.Is(ex.Last, context.DeadlineExceeded):
			return &api.Error{
				Status: http.StatusGatewayTimeout, Type: api.TypeServer, Code: api.CodeUpstreamTimeout,
				Message: "The upstream provider timed out.",
			}
		default:
			return &api.Error{
				Status: http.StatusBadGateway, Type: api.TypeServer, Code: api.CodeUpstreamError,
				Message: "The upstream provider failed.",
			}
		}
	}

	// A single upstream error: a bad request, which the router never
	// retries, or a stream that failed after the commit point.
	perr, _ := errors.AsType[*provider.Error](err)
	switch {
	case class == provider.BadRequest && perr != nil:
		return &api.Error{
			Status: http.StatusBadRequest, Type: api.TypeInvalidRequest,
			Message: "The upstream provider rejected the request: " + perr.Message,
		}
	case class == provider.RateLimited:
		retryAfter := time.Second
		if perr != nil && perr.RetryAfter > 0 {
			retryAfter = perr.RetryAfter
		}
		return &api.Error{
			Status: http.StatusServiceUnavailable, Type: api.TypeServer, Code: api.CodeUpstreamRateLimited,
			Message: "Every upstream provider for this model is rate limited. Retry later.", RetryAfter: retryAfter,
		}
	case errors.Is(err, context.DeadlineExceeded):
		return &api.Error{
			Status: http.StatusGatewayTimeout, Type: api.TypeServer, Code: api.CodeUpstreamTimeout,
			Message: "The upstream provider timed out.",
		}
	default:
		return &api.Error{
			Status: http.StatusBadGateway, Type: api.TypeServer, Code: api.CodeUpstreamError,
			Message: "The upstream provider failed.",
		}
	}
}

// logTries logs the router's per-attempt story when there is one worth
// telling: a failure, or a success that took more than one try. Any target
// with an auth or config problem is logged at error level, even if a
// fallback saved the request: the gateway's own key or model ID is wrong.
func (s *Server) logTries(ctx context.Context, alias string, res router.Result, err error) {
	if provider.ClassOf(err) == provider.Cancelled && err != nil {
		s.log.InfoContext(ctx, "client went away", "request_id", RequestID(ctx))
		return
	}
	if err == nil && len(res.Tries) <= 1 {
		return
	}
	// A request the router rejected before any attempt is the client's
	// mistake, not an upstream outcome; the access log records the 400.
	if _, ok := errors.AsType[*router.InvalidRequestError](err); ok {
		return
	}
	level, msg := slog.LevelInfo, "upstream recovered"
	if err != nil {
		level, msg = slog.LevelWarn, "upstream failed"
	}
	for _, try := range res.Tries {
		if try.Outcome == provider.AuthOrConfig.String() {
			level = slog.LevelError
		}
	}
	attrs := []any{"request_id", RequestID(ctx), "model", alias, "attempts", res.Attempts, "tries", res.Tries}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.log.Log(ctx, level, msg, attrs...)
}

// handleModels serves GET /v1/models: the configured aliases.
// Only the aliases the request's key may use are listed.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r.Context())
	names := slices.DeleteFunc(s.router.Names(), func(alias string) bool { return !key.Allows(alias) })
	api.WriteJSON(w, http.StatusOK, api.NewModelList(names, s.started))
}

// handleUnknown answers any other /v1/ path with OpenAI's 404 error.
func handleUnknown(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, &api.Error{
		Status: http.StatusNotFound, Type: api.TypeInvalidRequest,
		Message: fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path),
	})
}

// toAPIError returns err as an *api.Error, or a generic 500 if it is
// something else, which would be a bug.
func toAPIError(err error) *api.Error {
	if apiErr, ok := errors.AsType[*api.Error](err); ok {
		return apiErr
	}
	return &api.Error{
		Status: http.StatusInternalServerError, Type: api.TypeServer, Code: api.CodeInternal,
		Message: "The gateway hit an internal error.",
	}
}
