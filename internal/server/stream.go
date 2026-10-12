package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/router"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// streamChat serves a streaming request, following the rules in brief
// section 8. It records what the request used in a and finishes a. A stream
// that completes is stored under its idempotency key before the final
// [DONE], so a client that saw the stream end can always replay it.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, alias *router.Alias,
	req provider.ChatRequest, includeUsage bool, received time.Time, a *accounting) {
	ctx := r.Context()

	stream, result, err := s.router.ChatStream(ctx, alias, req)
	s.logTries(ctx, alias.Name, result, err)
	w.Header().Set(HeaderProvider, result.Provider)
	w.Header().Set(HeaderAttempts, strconv.Itoa(result.Attempts))
	if err != nil {
		// Before the commit point the client has seen nothing, so the
		// failure is an ordinary error response.
		a.finish(0, nil)
		if apiErr := s.upstreamError(ctx, err); apiErr != nil {
			api.WriteError(w, apiErr)
		}
		return
	}
	defer stream.Close() // also cancels the upstream call if it is still running

	// The commit point. From here on the response is an event stream, and
	// a failure can only be reported inside it.
	startEventStream(w)
	out := &eventSender{sse: api.NewSSEWriter(w), writeTimeout: s.timeouts.Idle}
	defer out.clearDeadline()
	id := "chatcmpl-" + rand.Text()
	chunks := api.NewChunks(id, received, stream.Model())

	var (
		reply    strings.Builder // the text so far, to estimate usage if the provider reports none
		reported *provider.Usage
		finish   provider.FinishReason
	)
	// spent records what the stream used so far and logs how it ended.
	spent := func(outcome string) {
		u, estimated := usageOrEstimate(reported, req, reply.String())
		a.recordUsage(result.Provider, result.Model, u)
		s.log.InfoContext(ctx, "stream ended",
			slog.String("request_id", RequestID(ctx)),
			slog.String("provider", result.Provider),
			slog.String("model", stream.Model()),
			slog.String("outcome", outcome),
			slog.Int("prompt_tokens", u.PromptTokens),
			slog.Int("completion_tokens", u.CompletionTokens),
			slog.Bool("usage_estimated", estimated),
		)
	}
	// failed ends a stream that didn't complete. Tokens were still used,
	// so the hold is settled; nothing is stored for replay.
	failed := func(outcome string) {
		spent(outcome)
		a.finish(0, nil)
	}

	// One loop reads, translates and writes, so a slow client slows the
	// upstream read down instead of piling chunks up in memory.
	if out.send(chunks.Role()) != nil {
		failed("client_gone")
		return
	}
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A failure after the commit point: say so in a final event and
			// end the stream without [DONE], so the client can't take the
			// partial reply for a complete one.
			apiErr := s.upstreamError(ctx, err)
			if apiErr == nil {
				failed("client_gone")
				return
			}
			_ = out.send(apiErr.Envelope())
			failed(apiErr.Code)
			return
		}
		if chunk.Delta != "" {
			reply.WriteString(chunk.Delta)
			if out.send(chunks.Content(chunk.Delta)) != nil {
				failed("client_gone")
				return
			}
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			u := *chunk.Usage
			reported = &u
		}
	}

	// The upstream ended normally. A stream that ends without a reason
	// ended naturally.
	if finish == "" {
		finish = provider.FinishStop
	}
	if out.send(chunks.Finish(finish)) != nil {
		failed("client_gone")
		return
	}
	u, _ := usageOrEstimate(reported, req, reply.String())
	if includeUsage {
		if out.send(chunks.Usage(u)) != nil {
			failed("client_gone")
			return
		}
	}

	// Save the outcome, the assembled completion included, before the
	// final event.
	spent("completed")
	completion, err := json.Marshal(api.NewChatCompletion(id, received,
		&provider.ChatResponse{Model: stream.Model(), Content: reply.String(), FinishReason: finish}, u))
	if err != nil {
		completion = nil // can't happen; then nothing is stored
	}
	a.finish(http.StatusOK, completion)
	if out.done() != nil {
		s.log.InfoContext(ctx, "the client left before the final event", "request_id", RequestID(ctx))
	}
}

// startEventStream commits the response to an event stream.
func startEventStream(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // stop proxies such as nginx buffering the stream
	w.WriteHeader(http.StatusOK)
}

// eventSender writes events, each with a write deadline. A slow client
// makes writes block, which is the backpressure the stream loop relies on,
// but a client that has stopped reading must not hold the upstream stream
// open forever.
type eventSender struct {
	sse          *api.SSEWriter
	writeTimeout time.Duration
}

func (e *eventSender) send(v any) error {
	if err := e.sse.SetWriteDeadline(time.Now().Add(e.writeTimeout)); err != nil {
		return err
	}
	return e.sse.WriteJSON(v)
}

func (e *eventSender) done() error {
	if err := e.sse.SetWriteDeadline(time.Now().Add(e.writeTimeout)); err != nil {
		return err
	}
	return e.sse.WriteDone()
}

// clearDeadline removes the write deadline, because the connection may
// carry another request after this one.
func (e *eventSender) clearDeadline() {
	_ = e.sse.SetWriteDeadline(time.Time{})
}

// usageOrEstimate returns the provider's token counts, or estimates them
// when it reported none: an interrupted stream, or a provider that doesn't
// send usage. The second result says whether the counts are estimates.
func usageOrEstimate(reported *provider.Usage, req provider.ChatRequest, reply string) (provider.Usage, bool) {
	if reported != nil {
		return *reported, false
	}
	return provider.Usage{
		PromptTokens:     usage.EstimatePrompt(req.Messages),
		CompletionTokens: usage.EstimateTokens(reply),
	}, true
}
