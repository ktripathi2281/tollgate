package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client has gone; there is nobody to tell.
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

// handleChatCompletions serves POST /v1/chat/completions. The steps follow
// the request pipeline in brief section 5; auth, limits, idempotency and
// budgets slot in between them in later milestones.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	received := s.now()

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

	alias, ok := s.router.Alias(req.Model)
	if !ok {
		api.WriteError(w, &api.Error{
			Status: http.StatusNotFound, Type: api.TypeInvalidRequest, Code: api.CodeModelNotFound, Param: "model",
			Message: fmt.Sprintf("The model `%s` does not exist.", req.Model),
		})
		return
	}

	// Every upstream call gets a real limit on output tokens: the client's,
	// or the alias default. The budget hold (M5) depends on this bound.
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

	creq := provider.ChatRequest{
		Messages:    req.Messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	if req.Stream {
		s.streamChat(w, r, alias, creq, req.IncludeUsage, received)
		return
	}

	resp, result, err := s.router.Chat(r.Context(), alias, creq)
	w.Header().Set(HeaderProvider, result.Provider)
	w.Header().Set(HeaderAttempts, strconv.Itoa(result.Attempts))
	if err != nil {
		if apiErr := s.upstreamError(r.Context(), err); apiErr != nil {
			api.WriteError(w, apiErr)
		}
		return
	}

	api.WriteJSON(w, http.StatusOK, api.NewChatCompletion("chatcmpl-"+rand.Text(), received, resp))
}

// errShuttingDown is the cause given to every request context still running
// when the shutdown grace period ends.
var errShuttingDown = errors.New("the gateway is shutting down")

// upstreamError turns a failed upstream call into the error to send the
// client, according to its class (brief section 6). It returns nil when the
// client has gone, since there is nobody left to tell. Details go to the
// log; the client gets a generic message, except for a bad request, which is
// about the client's own input.
func (s *Server) upstreamError(ctx context.Context, err error) *api.Error {
	class := provider.ClassOf(err)
	if class == provider.Cancelled {
		if errors.Is(context.Cause(ctx), errShuttingDown) {
			return &api.Error{
				Status: http.StatusServiceUnavailable, Type: api.TypeServer, Code: api.CodeShuttingDown,
				Message: "The gateway is shutting down. Retry the request.", RetryAfter: time.Second,
			}
		}
		s.log.InfoContext(ctx, "client went away", "request_id", RequestID(ctx))
		return nil
	}

	level := slog.LevelWarn
	if class == provider.AuthOrConfig {
		level = slog.LevelError // the gateway's own key or config is wrong
	}
	s.log.Log(ctx, level, "upstream call failed",
		"request_id", RequestID(ctx), "class", class.String(), "error", err)

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

// handleModels serves GET /v1/models: the configured aliases.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	api.WriteJSON(w, http.StatusOK, api.NewModelList(s.router.Names(), s.started))
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
