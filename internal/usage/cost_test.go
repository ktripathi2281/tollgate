package usage

import (
	"math"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// These tests are for the M5 exercise, Cost.

// price builds a config.Price from micro-USD per million tokens.
func price(in, out money.Micros) config.Price {
	return config.Price{InputMicros: in, OutputMicros: out}
}

func TestCost(t *testing.T) {
	// $1 and $2 per million tokens: 1 and 2 micro-USD per token.
	oneTwo := price(1*money.PerUSD, 2*money.PerUSD)
	// $0.10 and $0.50 per million tokens, and five times that for prompts
	// over 100,000 tokens: Claude Haiku 5.5.
	haiku := config.Price{
		InputMicros: 100_000, OutputMicros: 500_000,
		LongContext: &config.LongContextPrice{AbovePromptTokens: 100_000, InputMicros: 500_000, OutputMicros: 2_500_000},
	}
	tests := []struct {
		name               string
		price              config.Price
		prompt, completion int
		want               money.Micros
	}{
		{"nothing costs nothing", oneTwo, 0, 0, 0},
		{"whole micro-dollars", oneTwo, 10, 5, 20},               // 10×1 + 5×2
		{"a million tokens", oneTwo, 1_000_000, 0, 1_000_000},    // $1
		{"a fraction rounds up", haiku, 1, 0, 1},                 // 0.1 micro-USD charged as 1
		{"rounding happens once, on the total", haiku, 10, 0, 1}, // 10 × 0.1 = exactly 1
		{"just over a whole number rounds up", haiku, 11, 0, 2},  // 1.1
		{"input and output together", haiku, 1000, 200, 200},     // 100 + 100
		{"at the tier threshold, the base price", haiku, 100_000, 1000, 10_500},
		{"over the threshold, the long-context price for both", haiku, 100_001, 1000, 52_501}, // 50,000.5 + 2,500 → 52,501
		{"a free model", price(0, 0), 5000, 5000, 0},
		{"negative counts are zero", oneTwo, -5, -1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cost(tt.price, tt.prompt, tt.completion); got != tt.want {
				t.Errorf("Cost(%d, %d) = %d, want %d", tt.prompt, tt.completion, got, tt.want)
			}
		})
	}
}

// At the highest allowed price, even absurd token counts can't overflow.
func TestCostDoesNotOverflow(t *testing.T) {
	top := price(config.MaxPrice, config.MaxPrice)
	got := Cost(top, math.MaxInt, math.MaxInt)
	want := 2 * money.Micros(MaxBilledTokens) * config.MaxPrice / 1_000_000
	if got != want {
		t.Errorf("Cost at the caps = %d, want %d", got, want)
	}
	if got <= 0 {
		t.Error("the cost overflowed")
	}
}

func TestInputBound(t *testing.T) {
	tests := []struct {
		name     string
		messages []provider.Message
		want     int
	}{
		{"no messages: the reply's overhead", nil, 16},
		{"one ASCII message", []provider.Message{{Parts: []string{"hello"}}}, 16 + 16 + 5},
		{"bytes, not characters", []provider.Message{{Parts: []string{"日本語"}}}, 16 + 16 + 9},
		{"every part of every message", []provider.Message{
			{Parts: []string{"ab", "cd"}},
			{Parts: []string{"efg"}},
		}, 16 + 2*16 + 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InputBound(tt.messages); got != tt.want {
				t.Errorf("InputBound = %d, want %d", got, tt.want)
			}
		})
	}
	// It is never below the estimate the rate limiter uses.
	m := []provider.Message{{Parts: []string{"The quick brown fox jumps over the lazy dog."}}}
	if InputBound(m) < EstimatePrompt(m) {
		t.Error("InputBound is below EstimatePrompt")
	}
}
