package server

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/budget"
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/idempotency"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/provider"
	"github.com/ktripathi2281/tollgate/internal/provider/mock"
	"github.com/ktripathi2281/tollgate/internal/store"
	"github.com/ktripathi2281/tollgate/internal/store/storetest"
)

// These tests need Postgres: see storetest.EnvURL.

// mclock is a fake clock for budgets and idempotency.
type mclock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mclock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mclock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// moneyFixture is a test server with real budgets and idempotency keys on
// a Postgres schema of its own.
type moneyFixture struct {
	s       *Server
	store   *store.Store
	url     string
	budgets *budget.Budgets
	idem    *idempotency.Keys
	clock   *mclock
	key     auth.Key
}

const (
	testHoldTTL = 10 * time.Minute
	testLockFor = 6 * time.Minute
)

// withMoney gives s a key in a fresh database with the given daily budget
// (0 for none), and real budgets and idempotency keys. The aliases' target
// costs $1 per million input tokens and $2 per million output tokens.
func withMoney(t *testing.T, s *Server, providerName string, amount money.Micros) *moneyFixture {
	t.Helper()
	st, url := storetest.OpenWithURL(t)
	secret := auth.Generate()
	nk := store.NewKey{Name: "money", Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret), RPM: 1_000_000, TPM: 1_000_000_000}
	if amount > 0 {
		nk.Budget, nk.BudgetPeriod = amount, budget.Daily
	}
	if _, _, err := st.CreateKey(t.Context(), nk); err != nil {
		t.Fatal(err)
	}
	key, err := st.KeyByHash(t.Context(), auth.Hash(secret))
	if err != nil {
		t.Fatal(err)
	}
	s.keys.(*fakeKeys).keys[testSecret] = key

	clk := &mclock{now: fixedTime}
	f := &moneyFixture{s: s, store: st, url: url, clock: clk, key: key}
	f.budgets = budget.New(st, testHoldTTL, clk.Now, slog.New(slog.DiscardHandler))
	f.idem = idempotency.New(st, testLockFor, 24*time.Hour, 1<<20, clk.Now, slog.New(slog.DiscardHandler))
	s.budgets, s.idem = f.budgets, f.idem
	s.pricing = map[string]config.Price{
		config.PriceKey(providerName, "mock-1"): {InputMicros: 1 * money.PerUSD, OutputMicros: 2 * money.PerUSD},
	}
	return f
}

// holdsBy counts holds in each status.
func (f *moneyFixture) holds(t *testing.T, status string) int64 {
	t.Helper()
	return storetest.Int(t, f.url, "SELECT count(*) FROM budget_holds WHERE status = $1", status)
}

// worstCaseOfChatBody is what a chatBody request holds: its input bound,
// 16 + 16 + 2 = 34 tokens at $1 per million, plus the alias default of 5
// output tokens at $2 per million. 34 + 10 micro-USD.
const worstCaseOfChatBody = 44

func TestWorstCaseOfChatBody(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	s.pricing = map[string]config.Price{"mock/mock-1": {InputMicros: 1 * money.PerUSD, OutputMicros: 2 * money.PerUSD}}
	alias, _ := s.router.Alias("mock-fast")
	got, err := s.worstCase(alias, []provider.Message{{Role: provider.RoleUser, Parts: []string{"hi"}}}, 5)
	if err != nil || got != worstCaseOfChatBody {
		t.Fatalf("worstCase = %d, %v; want %d", got, err, worstCaseOfChatBody)
	}
}

// Acceptance (M5): 200 concurrent requests against a budget that fits
// exactly K holds, with the provider held open until all 200 are admitted
// or rejected: exactly K succeed, the rest get 402, and the invariants
// hold afterwards.
func TestBudgetAdmitsExactlyKOf200(t *testing.T) {
	const n, k = 200, 37
	g := newGate()
	s := testServer(t, g, func(c *config.Server) { c.MaxInflight = 1000 })
	f := withMoney(t, s, "gate", k*worstCaseOfChatBody)
	// Without a cost, nothing is held and nothing refused: fail now rather
	// than wait for refusals that never come.
	alias, _ := s.router.Alias("mock-fast")
	if wc, _ := s.worstCase(alias, []provider.Message{{Role: provider.RoleUser, Parts: []string{"hi"}}}, 5); wc != worstCaseOfChatBody {
		t.Fatalf("worst case = %d, want %d: is usage.Cost implemented?", wc, worstCaseOfChatBody)
	}

	finished := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			finished <- do(context.Background(), s, http.MethodPost, "/v1/chat/completions", chatBody).Code
		})
	}
	defer func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
		wg.Wait()
	}()

	// While the admitted requests are held open, the only ones that can
	// finish are the refused ones.
	for range n - k {
		if code := recv(t, finished); code != http.StatusPaymentRequired {
			t.Fatalf("a request finished with %d while the provider was held: want 402", code)
		}
	}
	for range k {
		recv(t, g.arrived)
	}
	select {
	case <-g.arrived:
		t.Fatal("more than K requests reached the provider")
	default:
	}
	if _, held := f.periodToday(t); held != k*worstCaseOfChatBody {
		t.Errorf("held %d with every admitted request running, want %d", held, k*worstCaseOfChatBody)
	}

	close(g.release)
	wg.Wait()
	for range k {
		if code := <-finished; code != http.StatusOK {
			t.Errorf("an admitted request finished with %d, want 200", code)
		}
	}
	// The gate reports 1 prompt and 1 completion token: 3 micro-USD each.
	spent, held := f.periodToday(t)
	if spent != k*3 || held != 0 {
		t.Errorf("spent %d, held %d; want %d, 0", spent, held, k*3)
	}
	if spent+held > f.key.Budget {
		t.Errorf("spent + held = %d, over the budget of %d", spent+held, f.key.Budget)
	}
	if got := f.holds(t, "settled"); got != k {
		t.Errorf("%d holds settled, want %d", got, k)
	}
	storetest.CheckBudgets(t, f.store, f.url)
}

// recv receives from ch, failing the test if nothing arrives within 30s:
// a guard against a hang, not part of what is tested.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting")
		panic("unreachable")
	}
}

func (f *moneyFixture) periodToday(t *testing.T) (spent, held money.Micros) {
	t.Helper()
	spent, held, err := f.store.BudgetPeriod(t.Context(), f.key.ID, budget.PeriodStart(budget.Daily, f.clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return spent, held
}

func TestBudgetExceededResponse(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	withMoney(t, s, "mock", worstCaseOfChatBody-1)
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %s", rec.Code, rec.Body)
	}
	e := decodeError(t, rec)
	for _, want := range []string{"daily budget of $0.000043", "up to $0.000044", "resets at 2026-10-08T00:00:00Z"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if e.Type != api.TypeInsufficientQuota || deref(e.Code) != api.CodeInsufficientQuota {
		t.Errorf("error = %+v, want insufficient_quota", e)
	}
}

// Acceptance (M5): every exit path leaves no hold in status held.
func TestEveryExitPathResolvesTheHold(t *testing.T) {
	const stream = `{"model": "mock-fast", "stream": true, "messages": [{"role": "user", "content": "hi"}]}`
	tests := []struct {
		name     string
		body     string
		provider func(leave context.CancelFunc) provider.Provider
		settled  bool // tokens were used, so the hold settles; otherwise it is released
	}{
		{"success", chatBody,
			func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{OutputTokens: 3}) }, true},
		{"stream success", stream,
			func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{OutputTokens: 3}) }, true},
		{"upstream failure", chatBody,
			func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{Status: 503}) }, false},
		{"stream failure before the commit point", stream,
			func(context.CancelFunc) provider.Provider { return mock.New("mock", mock.Config{Status: 503}) }, false},
		{"stream failure after the commit point", stream,
			func(context.CancelFunc) provider.Provider {
				return mock.New("mock", mock.Config{OutputTokens: 5, FailAtChunk: 3})
			}, true},
		{"failure after the hold, before the provider is called", chatBody,
			func(context.CancelFunc) provider.Provider { return &rejecting{} }, false},
		{"client gone during the call", chatBody,
			func(leave context.CancelFunc) provider.Provider {
				p := mock.New("mock", mock.Config{Hang: true})
				p.OnCall = leave
				return p
			}, false},
		{"client gone mid-stream", stream,
			func(leave context.CancelFunc) provider.Provider {
				return &scripted{stream: func(ctx context.Context) (provider.Stream, error) {
					return &leavesAfterFirstChunk{ctx: ctx, leave: leave}, nil
				}}
			}, true},
		{"panic", chatBody,
			func(context.CancelFunc) provider.Provider {
				return &scripted{chat: func(context.Context) (*provider.ChatResponse, error) { panic("provider bug") }}
			}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, leave := context.WithCancel(t.Context())
			defer leave()
			p := tt.provider(leave)
			s := testServer(t, p, nil)
			f := withMoney(t, s, p.Name(), 1_000_000)

			do(ctx, s, http.MethodPost, "/v1/chat/completions", tt.body)

			settled, released := f.holds(t, "settled"), f.holds(t, "released")
			if settled+released != 1 {
				t.Fatalf("%d settled and %d released holds, want exactly one resolved hold", settled, released)
			}
			if (settled == 1) != tt.settled {
				t.Errorf("settled = %v, want %v", settled == 1, tt.settled)
			}
			storetest.CheckBudgets(t, f.store, f.url)
		})
	}
}

// writeHook is a ResponseWriter that runs a check when the response
// starts, before the client could see any of it.
type writeHook struct {
	*httptest.ResponseRecorder
	once    sync.Once
	onStart func()
}

func (w *writeHook) WriteHeader(code int) {
	w.once.Do(w.onStart)
	w.ResponseRecorder.WriteHeader(code)
}

func (w *writeHook) Write(b []byte) (int, error) {
	w.once.Do(w.onStart)
	return w.ResponseRecorder.Write(b)
}

// The outcome is saved before the response is written (DECISIONS 2): when
// the first byte goes out, the hold is settled and the response stored.
func TestOutcomeIsSavedBeforeTheResponse(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 3}), nil)
	f := withMoney(t, s, "mock", 1_000_000)

	var settled, completed int64
	rec := &writeHook{ResponseRecorder: httptest.NewRecorder(), onStart: func() {
		settled = f.holds(t, "settled")
		completed = storetest.Int(t, f.url, "SELECT count(*) FROM idempotency_records WHERE status = 'completed'")
	}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Set("Idempotency-Key", `"persist-first"`)
	s.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if settled != 1 || completed != 1 {
		t.Errorf("when the response started: %d holds settled and %d records completed, want 1 and 1", settled, completed)
	}
}

// doIdem sends chatBody-style requests with an Idempotency-Key.
func doIdem(ctx context.Context, s *Server, idemKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Set("Idempotency-Key", idemKey)
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
}

// countingMock is the mock provider, counting calls.
func countingMock(cfg mock.Config) (*mock.Provider, *atomic.Int64) {
	var calls atomic.Int64
	p := mock.New("mock", cfg)
	p.OnCall = func() { calls.Add(1) }
	return p, &calls
}

// Acceptance (M5), idempotency: replay.
func TestIdempotentReplay(t *testing.T) {
	p, calls := countingMock(mock.Config{OutputTokens: 4})
	s := testServer(t, p, nil)
	f := withMoney(t, s, "mock", 1_000_000)
	rows := &collectingLog{}
	s.requests = rows

	first := doIdem(t.Context(), s, `"order-1"`, chatBody)
	second := doIdem(t.Context(), s, `"order-1"`, chatBody)
	if e := rows.last(t); !e.Replayed || e.Status != http.StatusOK || e.Provider != "" {
		t.Errorf("the replay's request-log row = %+v, want replayed with no provider", e)
	}
	if !strings.Contains(scrape(t, s), "tollgate_idempotent_replays_total 1") {
		t.Error("the replay wasn't counted")
	}
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses %d and %d, want 200 and 200", first.Code, second.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("the replay differs:\n first %s\nsecond %s", first.Body, second.Body)
	}
	if second.Header().Get(HeaderReplayed) != "true" || first.Header().Get(HeaderReplayed) != "" {
		t.Errorf("Idempotent-Replayed: first %q, second %q; want only the second marked",
			first.Header().Get(HeaderReplayed), second.Header().Get(HeaderReplayed))
	}
	if calls.Load() != 1 {
		t.Errorf("%d provider calls, want 1", calls.Load())
	}
	// No hold and no reservation for the replay.
	if n := f.holds(t, "settled") + f.holds(t, "released") + f.holds(t, "held"); n != 1 {
		t.Errorf("%d holds, want 1: the replay must not place one", n)
	}
	if reserved, _ := s.limiter.(*fakeLimiter).snapshot(); len(reserved) != 1 {
		t.Errorf("%d token reservations, want 1", len(reserved))
	}
}

// A streamed response is stored assembled, and replayed as a stream.
func TestIdempotentStreamReplay(t *testing.T) {
	const stream = `{"model": "mock-fast", "stream": true, "stream_options": {"include_usage": true}, "messages": [{"role": "user", "content": "hi"}]}`
	p, calls := countingMock(mock.Config{OutputTokens: 6})
	s := testServer(t, p, nil)
	withMoney(t, s, "mock", 0)

	text := func(rec *httptest.ResponseRecorder) (string, []string) {
		var content strings.Builder
		var events []string
		sc := bufio.NewScanner(rec.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			events = append(events, data)
			if c, ok := strings.CutPrefix(data, `{"id"`); ok && strings.Contains(c, `"content":"`) {
				_, after, _ := strings.Cut(c, `"content":"`)
				piece, _, _ := strings.Cut(after, `"`)
				content.WriteString(piece)
			}
		}
		return content.String(), events
	}
	first := doIdem(t.Context(), s, "s-1", stream)
	second := doIdem(t.Context(), s, "s-1", stream)
	firstText, firstEvents := text(first)
	secondText, secondEvents := text(second)

	if calls.Load() != 1 {
		t.Errorf("%d provider calls, want 1", calls.Load())
	}
	if second.Header().Get("Content-Type") != "text/event-stream" || second.Header().Get(HeaderReplayed) != "true" {
		t.Errorf("replay headers = %v", second.Header())
	}
	if firstText == "" || secondText != firstText {
		t.Errorf("replayed text %q, want the original %q", secondText, firstText)
	}
	// role, one content event, finish, usage, [DONE]
	if len(secondEvents) != 5 || secondEvents[4] != "[DONE]" || !strings.Contains(secondEvents[3], `"usage":{`) {
		t.Errorf("replayed events = %q", secondEvents)
	}
	if len(firstEvents) < 5 {
		t.Errorf("original events = %q", firstEvents)
	}
}

// Acceptance (M5), idempotency: 422 on mismatch, and a malformed key is
// a 400.
func TestIdempotencyMismatchAndBadKey(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	withMoney(t, s, "mock", 0)
	if rec := doIdem(t.Context(), s, "k", chatBody); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	other := strings.Replace(chatBody, "hi", "hello", 1)
	rec := doIdem(t.Context(), s, "k", other)
	if rec.Code != http.StatusUnprocessableEntity || deref(decodeError(t, rec).Code) != api.CodeIdempotencyKeyReused {
		t.Errorf("different body: status %d, body %s; want 422 idempotency_key_reused", rec.Code, rec.Body)
	}
	bad := doIdem(t.Context(), s, `"unterminated`, chatBody)
	if bad.Code != http.StatusBadRequest || deref(decodeError(t, bad).Code) != api.CodeInvalidIdempotency {
		t.Errorf("malformed key: status %d, body %s; want 400", bad.Code, bad.Body)
	}
}

// Acceptance (M5), idempotency: 409 while in progress, and N concurrent
// duplicates cause exactly one upstream call.
func TestConcurrentDuplicatesCallTheProviderOnce(t *testing.T) {
	const n = 20
	g := newGate()
	s := testServer(t, g, nil)
	withMoney(t, s, "gate", 1_000_000)

	finished := make(chan *httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { finished <- doIdem(context.Background(), s, "dup", chatBody) })
	}
	defer func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
		wg.Wait()
	}()

	recv(t, g.arrived)
	for range n - 1 {
		rec := recv(t, finished)
		if rec.Code != http.StatusConflict || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("a duplicate got %d (Retry-After %q) while the first ran; want 409 with Retry-After",
				rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	close(g.release)
	if rec := recv(t, finished); rec.Code != http.StatusOK {
		t.Errorf("the first request: %d, want 200", rec.Code)
	}
	wg.Wait()
	if len(g.arrived) != 0 {
		t.Errorf("%d more calls reached the provider; want exactly one in all", len(g.arrived))
	}
	// And the next retry is a replay.
	if rec := doIdem(t.Context(), s, "dup", chatBody); rec.Code != http.StatusOK || rec.Header().Get(HeaderReplayed) != "true" {
		t.Errorf("retry after completion: %d, replayed %q", rec.Code, rec.Header().Get(HeaderReplayed))
	}
}

// Acceptance (M5), idempotency: a retry is allowed after a failure.
func TestRetryAfterAFailure(t *testing.T) {
	var calls atomic.Int64
	p := &scripted{chat: func(context.Context) (*provider.ChatResponse, error) {
		if calls.Add(1) == 1 {
			return nil, &provider.Error{Provider: "scripted", Class: provider.Unavailable, Status: 500, Message: "boom"}
		}
		return &provider.ChatResponse{Model: "mock-1", Content: "ok", FinishReason: provider.FinishStop,
			Usage: &provider.Usage{PromptTokens: 1, CompletionTokens: 1}}, nil
	}}
	s := testServer(t, p, nil)
	f := withMoney(t, s, "scripted", 1_000_000)

	if rec := doIdem(t.Context(), s, "retry-me", chatBody); rec.Code != http.StatusBadGateway {
		t.Fatalf("first attempt: %d, want 502", rec.Code)
	}
	if rec := doIdem(t.Context(), s, "retry-me", chatBody); rec.Code != http.StatusOK || rec.Header().Get(HeaderReplayed) != "" {
		t.Errorf("retry: %d, replayed %q; want a fresh 200", rec.Code, rec.Header().Get(HeaderReplayed))
	}
	if calls.Load() != 2 {
		t.Errorf("%d calls, want 2", calls.Load())
	}
	storetest.CheckBudgets(t, f.store, f.url)
}

// Acceptance (M5), idempotency: takeover after locked_until.
func TestTakeoverOfAnAbandonedKey(t *testing.T) {
	p, calls := countingMock(mock.Config{OutputTokens: 2})
	s := testServer(t, p, nil)
	f := withMoney(t, s, "mock", 0)

	// A request claims the key and then the process "crashes".
	res, err := f.idem.Begin(t.Context(), f.key.ID, "crashed", []byte(chatBody), "req_crashed")
	if err != nil || res.Outcome != idempotency.Proceed {
		t.Fatalf("Begin = %+v, %v", res, err)
	}
	if rec := doIdem(t.Context(), s, "crashed", chatBody); rec.Code != http.StatusConflict {
		t.Fatalf("before the lock expires: %d, want 409", rec.Code)
	}
	f.clock.advance(testLockFor)
	if rec := doIdem(t.Context(), s, "crashed", chatBody); rec.Code != http.StatusOK {
		t.Fatalf("after the lock expires: %d, want 200 from a takeover; body %s", rec.Code, rec.Body)
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls, want 1", calls.Load())
	}
}

// A budget the database can't check is a 503, not a free pass.
func TestBudgetStoreFailureIs503(t *testing.T) {
	s := testServer(t, mock.New("mock", mock.Config{OutputTokens: 1}), nil)
	withMoney(t, s, "mock", 1_000_000)
	s.budgets = failingBudgets{}
	rec := do(t.Context(), s, http.MethodPost, "/v1/chat/completions", chatBody)
	if rec.Code != http.StatusServiceUnavailable || deref(decodeError(t, rec).Code) != api.CodeBudgetUnavailable {
		t.Errorf("status %d, body %s; want 503 budget_unavailable", rec.Code, rec.Body)
	}
}

type failingBudgets struct{}

func (failingBudgets) Hold(context.Context, auth.Key, money.Micros, string) (*budget.Hold, error) {
	return nil, errors.New("database is down")
}
func (failingBudgets) Settle(context.Context, *budget.Hold, money.Micros) {}
func (failingBudgets) Release(context.Context, *budget.Hold)              {}

// rejecting is a provider whose request rules turn every request away, so
// the router fails it after the hold is placed but before any call.
type rejecting struct{ scripted }

func (*rejecting) Name() string { return "rejecting" }

func (*rejecting) Validate(*provider.ChatRequest) error {
	return &provider.ParamError{Param: "messages", Message: "not for this model"}
}
