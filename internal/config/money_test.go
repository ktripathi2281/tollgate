package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ktripathi2281/tollgate/internal/config"
)

func TestParseBudgetAndIdempotency(t *testing.T) {
	cfg, err := config.Parse([]byte(minimal + `
budget: { hold_ttl: 20m }
idempotency: { lock_for: 7m, retention: 48h, max_response_bytes: 4096 }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Budget.HoldTTL != 20*time.Minute {
		t.Errorf("Budget = %+v", cfg.Budget)
	}
	if want := (config.Idempotency{LockFor: 7 * time.Minute, Retention: 48 * time.Hour, MaxResponseBytes: 4096}); cfg.Idempotency != want {
		t.Errorf("Idempotency = %+v, want %+v", cfg.Idempotency, want)
	}

	// The defaults pass the startup checks with the default timeouts.
	d := config.Default()
	if d.Budget.HoldTTL != 10*time.Minute || d.Idempotency.LockFor != 6*time.Minute ||
		d.Idempotency.Retention != 24*time.Hour || d.Idempotency.MaxResponseBytes != 1<<20 {
		t.Errorf("defaults = %+v, %+v", d.Budget, d.Idempotency)
	}
}

// Acceptance (M5): startup rejects a hold_ttl or idempotency lock that is
// too short for timeouts.total.
func TestParseRejectsTimingsTooShortForARequest(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantInError []string
	}{
		{
			name: "hold TTL no longer than total plus grace",
			// 300s total + 30s grace = 5m30s.
			yaml:        "budget: { hold_ttl: 5m30s }\n",
			wantInError: []string{"budget.hold_ttl: must be longer than timeouts.total plus server.shutdown_grace (5m30s)", "got 5m30s"},
		},
		{
			name:        "hold TTL checked against a longer total",
			yaml:        "timeouts: { total: 15m }\n",
			wantInError: []string{"budget.hold_ttl", "(15m30s)", "got 10m0s"},
		},
		{
			name:        "lock no longer than total",
			yaml:        "idempotency: { lock_for: 5m }\n",
			wantInError: []string{"idempotency.lock_for: must be longer than timeouts.total (5m0s)", "got 5m0s"},
		},
		{
			name:        "retention shorter than the lock",
			yaml:        "idempotency: { retention: 1m }\n",
			wantInError: []string{"idempotency.retention: must be at least idempotency.lock_for"},
		},
		{
			name:        "zero response cap",
			yaml:        "idempotency: { max_response_bytes: 0 }\n",
			wantInError: []string{"idempotency.max_response_bytes: must be at least 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(minimal + tt.yaml))
			if err == nil {
				t.Fatal("Parse: expected an error, got nil")
			}
			for _, want := range tt.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestParseRejectsPricesAboveTheCap(t *testing.T) {
	yaml := strings.Replace(minimal, `{ input: "1.00", output: "2.00" }`, `{ input: "10000.000001", output: "10000" }`, 1)
	_, err := config.Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), `pricing["mock/mock-1"].input: must be at most "10000"`) {
		t.Fatalf("Parse = %v, want the input price rejected", err)
	}
	if strings.Contains(err.Error(), ".output") {
		t.Errorf("exactly the cap must be allowed: %v", err)
	}
}
