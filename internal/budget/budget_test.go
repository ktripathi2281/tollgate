package budget_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ktripathi2281/tollgate/internal/auth"
	"github.com/ktripathi2281/tollgate/internal/budget"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/store"
	"github.com/ktripathi2281/tollgate/internal/store/storetest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestPeriods(t *testing.T) {
	at := time.Date(2026, 10, 12, 23, 59, 59, 0, time.FixedZone("IST", 5*3600+1800))
	// 23:59:59 IST is 18:29:59 UTC: periods are UTC.
	if got, want := budget.PeriodStart(budget.Daily, at), time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("daily start = %v, want %v", got, want)
	}
	if got, want := budget.PeriodStart(budget.Monthly, at), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("monthly start = %v, want %v", got, want)
	}
	dec31 := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if got := budget.PeriodEnd(budget.Daily, dec31); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("daily end = %v", got)
	}
	if got := budget.PeriodEnd(budget.Monthly, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monthly end = %v", got)
	}
}

// clock is a fake clock that moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

const holdTTL = 10 * time.Minute

type fixture struct {
	store *store.Store
	url   string
	clock *clock
	logs  *bytes.Buffer
	b     *budget.Budgets
	key   auth.Key
}

// newFixture sets up Budgets on a fresh schema, with a key whose daily
// budget is the given amount.
func newFixture(t *testing.T, amount money.Micros) *fixture {
	t.Helper()
	s, url := storetest.OpenWithURL(t)
	secret := auth.Generate()
	if _, _, err := s.CreateKey(t.Context(), store.NewKey{
		Name: "budgeted", Hash: auth.Hash(secret), Prefix: auth.DisplayPrefix(secret), RPM: 1, TPM: 1,
		Budget: amount, BudgetPeriod: budget.Daily,
	}); err != nil {
		t.Fatal(err)
	}
	key, err := s.KeyByHash(t.Context(), auth.Hash(secret))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	var logs bytes.Buffer
	b := budget.New(s, holdTTL, clk.Now, slog.New(slog.NewJSONHandler(&logs, nil)))
	return &fixture{store: s, url: url, clock: clk, logs: &logs, b: b, key: key}
}

func (f *fixture) period(t *testing.T, start time.Time) (spent, held money.Micros) {
	t.Helper()
	spent, held, err := f.store.BudgetPeriod(t.Context(), f.key.ID, start)
	if err != nil {
		t.Fatal(err)
	}
	return spent, held
}

func (f *fixture) hold(t *testing.T, amount money.Micros) *budget.Hold {
	t.Helper()
	h, err := f.b.Hold(t.Context(), f.key, amount, "req_test")
	if err != nil {
		t.Fatalf("Hold(%d): %v", amount, err)
	}
	return h
}

var today = time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)

func TestHoldSettleRelease(t *testing.T) {
	f := newFixture(t, 1000)

	h := f.hold(t, 300)
	if spent, held := f.period(t, today); spent != 0 || held != 300 {
		t.Fatalf("after the hold: spent %d, held %d; want 0, 300", spent, held)
	}
	f.b.Settle(t.Context(), h, 120)
	if spent, held := f.period(t, today); spent != 120 || held != 0 {
		t.Fatalf("after the settle: spent %d, held %d; want 120, 0", spent, held)
	}

	h2 := f.hold(t, 500)
	f.b.Release(t.Context(), h2)
	if spent, held := f.period(t, today); spent != 120 || held != 0 {
		t.Fatalf("after the release: spent %d, held %d; want 120, 0", spent, held)
	}
	storetest.CheckBudgets(t, f.store, f.url)
}

func TestNothingToHold(t *testing.T) {
	f := newFixture(t, 1000)
	if h, err := f.b.Hold(t.Context(), f.key, 0, "req"); h != nil || err != nil {
		t.Errorf("Hold of nothing = %v, %v; want no hold", h, err)
	}
	unbudgeted := f.key
	unbudgeted.Budget = 0
	if h, err := f.b.Hold(t.Context(), unbudgeted, 500, "req"); h != nil || err != nil {
		t.Errorf("Hold for a key without a budget = %v, %v; want no hold", h, err)
	}
}

func TestHoldsStopAtTheBudget(t *testing.T) {
	f := newFixture(t, 1000)
	f.hold(t, 400)
	f.hold(t, 600) // exactly fills the budget
	if _, err := f.b.Hold(t.Context(), f.key, 1, "req"); !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("a hold past the budget: got %v, want ErrExceeded", err)
	}
	if spent, held := f.period(t, today); spent != 0 || held != 1000 {
		t.Errorf("a refused hold changed the period: spent %d, held %d", spent, held)
	}
}

// Settle records the actual cost even past the budget: the tokens were
// used, so they're owed. Later holds are then refused.
func TestSettleCanPassTheBudget(t *testing.T) {
	f := newFixture(t, 1000)
	h := f.hold(t, 900)
	f.b.Settle(t.Context(), h, 1200)
	if spent, held := f.period(t, today); spent != 1200 || held != 0 {
		t.Fatalf("spent %d, held %d; want 1200, 0", spent, held)
	}
	if _, err := f.b.Hold(t.Context(), f.key, 1, "req"); !errors.Is(err, budget.ErrExceeded) {
		t.Errorf("a hold after overspending: got %v, want ErrExceeded", err)
	}
	storetest.CheckBudgets(t, f.store, f.url)
}

// Acceptance (M5): double settle and settle-after-release are no-ops.
func TestResolvingTwiceChangesNothing(t *testing.T) {
	f := newFixture(t, 1000)
	h := f.hold(t, 300)
	f.b.Settle(t.Context(), h, 100)
	f.b.Settle(t.Context(), h, 250) // a second settle, with a different cost
	f.b.Release(t.Context(), h)     // and a release after the settle
	if spent, held := f.period(t, today); spent != 100 || held != 0 {
		t.Fatalf("spent %d, held %d; want the first settle's 100, 0", spent, held)
	}

	r := f.hold(t, 300)
	f.b.Release(t.Context(), r)
	f.b.Release(t.Context(), r)
	if spent, held := f.period(t, today); spent != 100 || held != 0 {
		t.Fatalf("after a double release: spent %d, held %d; want 100, 0", spent, held)
	}
	if n := f.b.Stats.LateSettles.Load() + f.b.Stats.SettleFailures.Load(); n != 0 {
		t.Errorf("a double settle was counted as a failure or late settle: %d", n)
	}
	storetest.CheckBudgets(t, f.store, f.url)
}

// Acceptance (M5): a hold orphaned by a simulated crash is released by the
// sweeper. Acceptance (M5): a late settle after a sweeper release is
// logged and counted.
func TestSweeperAndLateSettle(t *testing.T) {
	f := newFixture(t, 1000)
	orphan := f.hold(t, 400) // its request "crashes": nothing settles it
	f.clock.set(f.clock.Now().Add(holdTTL - time.Second))
	young := f.hold(t, 100)

	if n, err := f.b.Sweep(t.Context()); err != nil || n != 0 {
		t.Fatalf("sweep before the expiry = %d, %v; want nothing released", n, err)
	}
	f.clock.set(f.clock.Now().Add(time.Second))
	if n, err := f.b.Sweep(t.Context()); err != nil || n != 1 {
		t.Fatalf("sweep at the expiry = %d, %v; want the orphan released", n, err)
	}
	if spent, held := f.period(t, today); spent != 0 || held != 100 {
		t.Fatalf("after the sweep: spent %d, held %d; want 0 and the young hold's 100", spent, held)
	}
	if f.b.Stats.Swept.Load() != 1 {
		t.Errorf("Swept = %d, want 1", f.b.Stats.Swept.Load())
	}

	// The orphan's request turns out to be alive after all, and settles.
	f.b.Settle(t.Context(), orphan, 350)
	if spent, held := f.period(t, today); spent != 0 || held != 100 {
		t.Fatalf("a late settle changed the period: spent %d, held %d", spent, held)
	}
	if f.b.Stats.LateSettles.Load() != 1 {
		t.Errorf("LateSettles = %d, want 1", f.b.Stats.LateSettles.Load())
	}
	if !strings.Contains(f.logs.String(), `"level":"ERROR","msg":"late settle`) || !strings.Contains(f.logs.String(), `"cost_micros":350`) {
		t.Errorf("the late settle isn't logged at error level with its cost:\n%s", f.logs)
	}

	f.b.Settle(t.Context(), young, 80)
	storetest.CheckBudgets(t, f.store, f.url)
}

// Acceptance (M5): a hold placed before a period boundary settles into its
// own period, and the new period starts at zero.
func TestPeriodBoundary(t *testing.T) {
	f := newFixture(t, 1000)
	f.clock.set(time.Date(2026, 10, 12, 23, 59, 59, 0, time.UTC))
	late := f.hold(t, 700)

	f.clock.set(time.Date(2026, 10, 13, 0, 0, 1, 0, time.UTC))
	tomorrow := today.AddDate(0, 0, 1)
	if spent, held := f.period(t, tomorrow); spent != 0 || held != 0 {
		t.Fatalf("the new period: spent %d, held %d; want it to start at zero", spent, held)
	}
	// The whole new budget is available, though yesterday's hold is open.
	early := f.hold(t, 1000)
	if early.PeriodStart != tomorrow {
		t.Fatalf("the new hold is in period %v, want %v", early.PeriodStart, tomorrow)
	}

	f.b.Settle(t.Context(), late, 600)
	if spent, held := f.period(t, today); spent != 600 || held != 0 {
		t.Errorf("yesterday: spent %d, held %d; want the late hold settled there: 600, 0", spent, held)
	}
	if spent, held := f.period(t, tomorrow); spent != 0 || held != 1000 {
		t.Errorf("today: spent %d, held %d; want only today's hold: 0, 1000", spent, held)
	}
	f.b.Release(t.Context(), early)
	storetest.CheckBudgets(t, f.store, f.url)
}

// Many holds at once never take more than the budget.
func TestConcurrentHolds(t *testing.T) {
	f := newFixture(t, 1000)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			h, err := f.b.Hold(context.Background(), f.key, 70, "req")
			if errors.Is(err, budget.ErrExceeded) {
				return
			}
			if err != nil {
				t.Errorf("Hold: %v", err)
				return
			}
			admitted.Add(1)
			f.b.Release(context.Background(), h)
		})
	}
	wg.Wait()
	// Releases make room again, so the count can be anything from 14 up.
	// What matters is that the totals agree with the holds at the end, and
	// nothing is left held. The server's 200-request test checks the exact
	// count, with every request held open.
	if admitted.Load() < 1000/70 {
		t.Errorf("admitted %d holds, want at least %d", admitted.Load(), 1000/70)
	}
	storetest.CheckBudgets(t, f.store, f.url)
}

// failingStore fails every settle, to test retries and the failure count.
type failingStore struct {
	budget.Store
	settles atomic.Int64
}

func (f *failingStore) SettleHold(context.Context, int64, money.Micros, time.Time) (store.Resolution, error) {
	f.settles.Add(1)
	return 0, errors.New("connection reset")
}

func TestSettleRetriesThenCountsTheFailure(t *testing.T) {
	fs := &failingStore{}
	var logs bytes.Buffer
	b := budget.New(fs, holdTTL, time.Now, slog.New(slog.NewJSONHandler(&logs, nil)))
	budget.SetRetryWait(b, 0)

	b.Settle(t.Context(), &budget.Hold{ID: 7, KeyID: 1, Amount: 500, RequestID: "req_x"}, 321)
	if n := fs.settles.Load(); n != 3 {
		t.Errorf("tried %d times, want 3", n)
	}
	if n := b.Stats.SettleFailures.Load(); n != 1 {
		t.Errorf("SettleFailures = %d, want 1", n)
	}
	for _, want := range []string{`"level":"ERROR"`, `"hold_micros":500`, `"cost_micros":321`, `"request_id":"req_x"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the failure log lacks %s:\n%s", want, logs.String())
		}
	}
}
