package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
)

// pipeListener is an in-memory network for one server. Each connection is a
// net.Pipe pair, so inside a synctest bubble the server, the client and
// every timer between them run on the fake clock.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial connects a new client connection to the listener. It has the
// signature of http.Transport.DialContext.
func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *pipeListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: l.dial}}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// serveOnPipe runs s on an in-memory listener. Cancel the returned context
// to start a shutdown, as SIGTERM does in main; the channel receives
// Serve's result.
func serveOnPipe(t *testing.T, s *Server) (*pipeListener, context.CancelFunc, <-chan error) {
	t.Helper()
	ln := newPipeListener()
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()
	return ln, stop, served
}

// eventReader reads server-sent events one at a time.
type eventReader struct{ r *bufio.Reader }

func newEventReader(r io.Reader) *eventReader { return &eventReader{bufio.NewReader(r)} }

// next returns the data of the next event, or io.EOF when the stream ends.
func (e *eventReader) next() (string, error) {
	var data string
	for {
		line, err := e.r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if data != "" {
				return data, nil
			}
			continue
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data = d
		}
	}
}

// all returns the data of every remaining event.
func (e *eventReader) all(t *testing.T) []string {
	t.Helper()
	var events []string
	for {
		data, err := e.next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("reading events: %v", err)
		}
		events = append(events, data)
	}
}

func streamBody(extra string) string {
	return `{"model": "mock-fast", "stream": true, ` + extra +
		`"messages": [{"role": "user", "content": "Hello there!"}]}`
}

func postStream(t *testing.T, client *http.Client, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func decodeChunk(t *testing.T, data string) api.ChatCompletionChunk {
	t.Helper()
	var c api.ChatCompletionChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("event %q is not a chunk: %v", data, err)
	}
	return c
}

// errorCode returns the code of an error event, or "" if data isn't one.
func errorCode(data string) string {
	var env api.Envelope
	if json.Unmarshal([]byte(data), &env) != nil || env.Error.Code == nil {
		return ""
	}
	return *env.Error.Code
}

func TestStreamResponse(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 3, Seed: 1}), nil)
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", streamBody(`"stream_options": {"include_usage": true},`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
		HeaderProvider:      "mock",
		HeaderAttempts:      "1",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	events := newEventReader(rec.Body).all(t)
	// role, 3 words, finish, usage, [DONE]
	if len(events) != 7 || events[6] != "[DONE]" {
		t.Fatalf("got %d events, want 7 ending in [DONE]:\n%s", len(events), strings.Join(events, "\n"))
	}
	chunks := make([]api.ChatCompletionChunk, 6)
	for i := range chunks {
		chunks[i] = decodeChunk(t, events[i])
		c := chunks[i]
		if c.ID != chunks[0].ID || !strings.HasPrefix(c.ID, "chatcmpl-") || c.Created != fixedTime.Unix() || c.Model != "mock-1" {
			t.Errorf("chunk %d: id %q, created %d, model %q; want the stream's id, the request time and mock-1", i, c.ID, c.Created, c.Model)
		}
	}
	if d := chunks[0].Choices[0].Delta; d.Role != "assistant" {
		t.Errorf("first delta = %+v, want the assistant role", d)
	}
	if f := chunks[4].Choices[0].FinishReason; f == nil || *f != "stop" {
		t.Errorf("finish chunk = %+v", chunks[4])
	}
	// "Hello there!" is 12 characters, about 3 prompt tokens.
	if u := chunks[5].Usage; u == nil || *u != (api.Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6}) {
		t.Errorf("usage chunk = %+v", chunks[5])
	}
}

func TestStreamSendsUsageOnlyWhenAsked(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 2}), nil)
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", streamBody(""))
	events := newEventReader(rec.Body).all(t)
	// role, 2 words, finish, [DONE]
	if len(events) != 5 || strings.Contains(rec.Body.String(), `"usage"`) {
		t.Errorf("got events %q, want 5 and no usage", events)
	}
}

// fixedStreams is a provider whose streams return fixed chunks, then end.
type fixedStreams struct{ chunks []*provider.Chunk }

func (fixedStreams) Name() string { return "fixed" }

func (fixedStreams) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("fixed: Chat is not used")
}

func (f fixedStreams) ChatStream(context.Context, *provider.ChatRequest) (provider.Stream, error) {
	return &sliceStream{chunks: slices.Clone(f.chunks)}, nil
}

type sliceStream struct{ chunks []*provider.Chunk }

func (s *sliceStream) Next() (*provider.Chunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *sliceStream) Close() error { return nil }

func TestStreamEstimatesMissingUsage(t *testing.T) {
	// The provider reports no usage and no finish reason.
	p := fixedStreams{chunks: []*provider.Chunk{{Delta: "Hello"}, {Delta: " world"}}}
	s := testServer(t, p, nil)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewJSONHandler(&logs, nil))

	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", streamBody(`"stream_options": {"include_usage": true},`))
	events := newEventReader(rec.Body).all(t)
	if len(events) != 6 {
		t.Fatalf("got events %q, want role, 2 words, finish, usage, [DONE]", events)
	}
	if f := decodeChunk(t, events[3]).Choices[0].FinishReason; f == nil || *f != "stop" {
		t.Errorf("a stream that ends without a reason should finish with stop, got %v", f)
	}
	// "Hello there!" is 12 characters and "Hello world" 11: 3 tokens each.
	if u := decodeChunk(t, events[4]).Usage; u == nil || *u != (api.Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6}) {
		t.Errorf("usage = %+v, want the estimate 3+3", u)
	}
	if !strings.Contains(logs.String(), `"usage_estimated":true`) || !strings.Contains(logs.String(), `"outcome":"completed"`) {
		t.Errorf("stream log doesn't record estimated usage:\n%s", logs.String())
	}
}

// Acceptance (M2): a failure before the commit point gives a normal error
// response, and the first-token timeout fires.
func TestStreamFailureBeforeCommitIsAnErrorResponse(t *testing.T) {
	timeouts := config.Timeouts{FirstToken: 2 * time.Second, Idle: time.Second, Total: time.Minute}
	tests := []struct {
		name           string
		mock           mock.Config
		wantStatus     int
		wantCode       string
		wantRetryAfter string
		wantAfter      time.Duration
	}{
		{"upstream 500", mock.Config{Status: 500}, 502, api.CodeUpstreamError, "", 0},
		{"upstream 429", mock.Config{Status: 429}, 503, api.CodeUpstreamRateLimited, "1", 0},
		{"error event before any content", mock.Config{TTFT: 100 * time.Millisecond, FailAtChunk: 1}, 502, api.CodeUpstreamError, "", 100 * time.Millisecond},
		{"first-token timeout", mock.Config{StallAtChunk: 1}, 504, api.CodeUpstreamTimeout, "", 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tt.mock.OutputTokens = 5
				s := testServerWith(t, mock.New("mock", tt.mock), nil, timeouts)

				start := time.Now()
				rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", streamBody(""))

				if rec.Code != tt.wantStatus || rec.Header().Get("Content-Type") != "application/json" {
					t.Errorf("status %d, Content-Type %q; want %d and an ordinary JSON error",
						rec.Code, rec.Header().Get("Content-Type"), tt.wantStatus)
				}
				if e := decodeError(t, rec); deref(e.Code) != tt.wantCode {
					t.Errorf("code = %q, want %q", deref(e.Code), tt.wantCode)
				}
				if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
					t.Errorf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
				}
				if elapsed := time.Since(start); elapsed != tt.wantAfter {
					t.Errorf("answered after %v, want %v", elapsed, tt.wantAfter)
				}
			})
		})
	}
}

// Acceptance (M2): a failure after the commit point gives an SSE error
// event, and the idle timeout fires.
func TestStreamFailureAfterCommitIsAnErrorEvent(t *testing.T) {
	tests := []struct {
		name        string
		mock        mock.Config
		timeouts    config.Timeouts
		wantContent int
		wantCode    string
		wantAfter   time.Duration
	}{
		{
			name:        "error event mid-stream",
			mock:        mock.Config{TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond, FailAtChunk: 3},
			timeouts:    config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: time.Minute},
			wantContent: 2, wantCode: api.CodeUpstreamError, wantAfter: 120 * time.Millisecond,
		},
		{
			name:        "idle timeout",
			mock:        mock.Config{TTFT: 100 * time.Millisecond, TokenInterval: 10 * time.Millisecond, StallAtChunk: 3},
			timeouts:    config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: time.Minute},
			wantContent: 2, wantCode: api.CodeUpstreamTimeout, wantAfter: 110*time.Millisecond + time.Second,
		},
		{
			name:        "total timeout",
			mock:        mock.Config{TTFT: 300 * time.Millisecond, TokenInterval: 300 * time.Millisecond},
			timeouts:    config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: time.Second},
			wantContent: 3, wantCode: api.CodeUpstreamTimeout, wantAfter: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tt.mock.OutputTokens = 10
				s := testServerWith(t, mock.New("mock", tt.mock), nil, tt.timeouts)

				start := time.Now()
				rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", streamBody(""))
				elapsed := time.Since(start)

				if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
					t.Fatalf("status %d, Content-Type %q; want a committed stream", rec.Code, rec.Header().Get("Content-Type"))
				}
				events := newEventReader(rec.Body).all(t)
				// role, the content that arrived, then the error event
				if len(events) != tt.wantContent+2 {
					t.Fatalf("got events %q, want role, %d words and an error", events, tt.wantContent)
				}
				last := events[len(events)-1]
				if code := errorCode(last); code != tt.wantCode {
					t.Errorf("last event %q, want an error with code %q", last, tt.wantCode)
				}
				if slices.Contains(events, "[DONE]") {
					t.Error("a cut-short stream must not end with [DONE]")
				}
				if elapsed != tt.wantAfter {
					t.Errorf("stream ended after %v, want %v", elapsed, tt.wantAfter)
				}
			})
		})
	}
}

// Acceptance (M2): events are flushed one at a time; the test reads them as
// they arrive.
func TestStreamEventsArriveOneAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := mock.New("mock", mock.Config{TTFT: 100 * time.Millisecond, TokenInterval: 50 * time.Millisecond, OutputTokens: 3})
		ln, stop, served := serveOnPipe(t, testServer(t, p, nil))
		client := ln.client()

		start := time.Now()
		resp := postStream(t, client, streamBody(""))
		events := newEventReader(resp.Body)

		// The role and first word go out together at the first token, then
		// one word every 50ms. On the fake clock the arrival times are
		// exact, and they can only be this exact if each event is flushed
		// as soon as it is written.
		want := []struct {
			at   time.Duration
			kind string
		}{
			{100 * time.Millisecond, "role"},
			{100 * time.Millisecond, "word"},
			{150 * time.Millisecond, "word"},
			{200 * time.Millisecond, "word"},
			{200 * time.Millisecond, "finish"},
			{200 * time.Millisecond, "done"},
		}
		for i, w := range want {
			data, err := events.next()
			if err != nil {
				t.Fatalf("event %d (%s): %v", i, w.kind, err)
			}
			if got := time.Since(start); got != w.at {
				t.Errorf("event %d (%s) arrived at %v, want %v", i, w.kind, got, w.at)
			}
			if w.kind == "done" && data != "[DONE]" {
				t.Errorf("last event = %q, want [DONE]", data)
			}
		}

		_ = resp.Body.Close()
		client.CloseIdleConnections()
		stop()
		if err := <-served; err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	})
}

// Acceptance (M2): a client disconnect cancels the upstream call within
// 100ms. This test uses a real TCP connection and the real clock, because
// it measures how quickly net/http notices a closed connection.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	p := mock.New("mock", mock.Config{OutputTokens: 100, StallAtChunk: 2})
	cancelled := make(chan time.Time, 1)
	p.OnCancel = func() { cancelled <- time.Now() }
	s := testServer(t, p, nil)
	srv := httptest.NewServer(s.http.Handler)
	defer srv.Close()

	ctx, disconnect := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(streamBody("")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := newEventReader(resp.Body)
	for range 2 { // the role and the first word; then the upstream stalls
		if _, err := events.next(); err != nil {
			t.Fatal(err)
		}
	}

	disconnectedAt := time.Now()
	disconnect() // closes the client's connection
	select {
	case at := <-cancelled:
		if took := at.Sub(disconnectedAt); took > 100*time.Millisecond {
			t.Errorf("the upstream saw the cancellation after %v, want within 100ms", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never saw the client disconnect")
	}
}

// Acceptance (M2): SIGTERM drains an in-flight stream.
func TestShutdownDrainsStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := mock.New("mock", mock.Config{TTFT: time.Second, TokenInterval: time.Second, OutputTokens: 5})
		s := testServer(t, p, func(c *config.Server) { c.ShutdownGrace = 30 * time.Second })
		ln, stop, served := serveOnPipe(t, s)
		client := ln.client()

		start := time.Now()
		resp := postStream(t, client, streamBody(""))
		events := newEventReader(resp.Body)
		if _, err := events.next(); err != nil { // the role, at 1s
			t.Fatal(err)
		}

		stop() // what SIGTERM does in main
		synctest.Wait()
		if _, err := ln.dial(t.Context(), "", ""); err == nil {
			t.Error("a new connection was accepted after shutdown began")
		}

		rest := events.all(t)
		if len(rest) != 7 || rest[6] != "[DONE]" {
			t.Errorf("after shutdown began got %q, want the other 5 words, finish and [DONE]", rest)
		}
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Errorf("stream ended after %v, want 5s: it should run to the end", elapsed)
		}
		_ = resp.Body.Close()
		client.CloseIdleConnections()
		if err := <-served; err != nil {
			t.Errorf("Serve = %v, want nil after a full drain", err)
		}
	})
}

// Acceptance (M2): after the grace period, stragglers are cancelled.
func TestShutdownCancelsStragglers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstreamCancelled := false
		p := mock.New("mock", mock.Config{TTFT: time.Second, TokenInterval: 700 * time.Millisecond, OutputTokens: 100})
		p.OnCancel = func() { upstreamCancelled = true }
		s := testServer(t, p, func(c *config.Server) { c.ShutdownGrace = 2500 * time.Millisecond })
		ln, stop, served := serveOnPipe(t, s)
		client := ln.client()

		resp := postStream(t, client, streamBody(""))
		events := newEventReader(resp.Body)
		if _, err := events.next(); err != nil { // the role, at 1s
			t.Fatal(err)
		}
		stop()
		shutdownAt := time.Now()

		// Words keep coming for the 2.5s grace period, then the stream ends
		// with an error event saying why.
		rest := events.all(t)
		if len(rest) < 2 {
			t.Fatalf("got %q, want words and then an error event", rest)
		}
		if code := errorCode(rest[len(rest)-1]); code != api.CodeShuttingDown {
			t.Errorf("last event %q, want an error with code %q", rest[len(rest)-1], api.CodeShuttingDown)
		}
		if words := len(rest) - 1; words != 4 { // at 1.0s, 1.7s, 2.4s and 3.1s
			t.Errorf("got %d words before the cancellation, want 4", words)
		}
		if elapsed := time.Since(shutdownAt); elapsed != 2500*time.Millisecond {
			t.Errorf("stream was cancelled %v after shutdown began, want 2.5s", elapsed)
		}
		if !upstreamCancelled {
			t.Error("the upstream call was not cancelled")
		}
		_ = resp.Body.Close()
		client.CloseIdleConnections()
		if err := <-served; err == nil {
			t.Error("Serve = nil, want an error: requests had to be cancelled")
		}
	})
}

// ctxRecorder wraps a provider and keeps the context of the last stream it
// opened.
type ctxRecorder struct {
	provider.Provider
	ctx context.Context
}

func (c *ctxRecorder) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.Stream, error) {
	c.ctx = ctx
	return c.Provider.ChatStream(ctx, req)
}

// A client that stops reading must not hold the upstream stream open: the
// write deadline (the idle timeout) ends the stream.
func TestStreamCutsOffAStuckClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &ctxRecorder{Provider: mock.New("mock", mock.Config{TokenInterval: 10 * time.Millisecond, OutputTokens: 100000})}
		timeouts := config.Timeouts{FirstToken: time.Second, Idle: time.Second, Total: time.Hour}
		ln, stop, served := serveOnPipe(t, testServerWith(t, p, nil, timeouts))
		client := ln.client()

		resp := postStream(t, client, streamBody(""))
		if _, err := newEventReader(resp.Body).next(); err != nil {
			t.Fatal(err)
		}
		// Stop reading. net.Pipe has no buffer, so the server's next write
		// blocks until its deadline.
		time.Sleep(5 * time.Second)
		if p.ctx.Err() == nil {
			t.Error("the upstream stream is still open 5s after the client stopped reading")
		}

		_ = resp.Body.Close()
		client.CloseIdleConnections()
		stop()
		<-served
	})
}
