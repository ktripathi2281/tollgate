package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/money"
)

// longContext prices the minimal config's target with a long-context tier.
var longContext = strings.Replace(minimal, `{ input: "1.00", output: "2.00" }`,
	`{ input: "0.10", output: "0.50", long_context: { above_prompt_tokens: 100000, input: "0.50", output: "2.50" } }`, 1)

func TestParseLongContextPrice(t *testing.T) {
	cfg, err := config.Parse([]byte(longContext))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := config.Price{
		Input: "0.10", Output: "0.50", InputMicros: 100_000, OutputMicros: 500_000,
		LongContext: &config.LongContextPrice{
			AbovePromptTokens: 100_000,
			Input:             "0.50", Output: "2.50", InputMicros: 500_000, OutputMicros: 2_500_000,
		},
	}
	if got := cfg.Pricing["mock/mock-1"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Pricing = %+v (long context %+v)\nwant %+v (long context %+v)", got, got.LongContext, want, want.LongContext)
	}
}

func TestPriceRates(t *testing.T) {
	flat := config.Price{InputMicros: 1 * money.PerUSD, OutputMicros: 2 * money.PerUSD}
	tiered := config.Price{
		InputMicros: 100_000, OutputMicros: 500_000,
		LongContext: &config.LongContextPrice{AbovePromptTokens: 100_000, InputMicros: 500_000, OutputMicros: 2_500_000},
	}
	tests := []struct {
		name            string
		price           config.Price
		promptTokens    int
		wantIn, wantOut money.Micros
	}{
		{"no tier, short prompt", flat, 10, 1 * money.PerUSD, 2 * money.PerUSD},
		{"no tier, long prompt", flat, 1_000_000, 1 * money.PerUSD, 2 * money.PerUSD},
		{"empty prompt", tiered, 0, 100_000, 500_000},
		{"exactly at the threshold is not over it", tiered, 100_000, 100_000, 500_000},
		{"one token over", tiered, 100_001, 500_000, 2_500_000},
		{"far over", tiered, 900_000, 500_000, 2_500_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, out := tt.price.Rates(tt.promptTokens)
			if in != tt.wantIn || out != tt.wantOut {
				t.Errorf("Rates(%d) = %d, %d; want %d, %d", tt.promptTokens, in, out, tt.wantIn, tt.wantOut)
			}
		})
	}
}

func TestParseRejectsLongContextMistakes(t *testing.T) {
	const field = `pricing["mock/mock-1"].long_context`
	tests := []struct {
		name        string
		yaml        string
		wantInError []string
		// notInError lists substrings the error message must not contain.
		notInError []string
	}{
		{
			name:        "missing threshold",
			yaml:        strings.Replace(longContext, "above_prompt_tokens: 100000, ", "", 1),
			wantInError: []string{field + ".above_prompt_tokens: must be at least 1, got 0"},
		},
		{
			name:        "negative threshold",
			yaml:        strings.Replace(longContext, "above_prompt_tokens: 100000", "above_prompt_tokens: -5", 1),
			wantInError: []string{field + ".above_prompt_tokens: must be at least 1, got -5"},
		},
		{
			name:        "unparseable prices are reported, without a misleading comparison",
			yaml:        strings.Replace(longContext, `input: "0.50", output: "2.50"`, `input: "x", output: "-1"`, 1),
			wantInError: []string{field + ".input", field + ".output"},
			notInError:  []string{"lower than the base"},
		},
		{
			name:        "cheaper input than the base price",
			yaml:        strings.Replace(longContext, `input: "0.50", output: "2.50"`, `input: "0.05", output: "2.50"`, 1),
			wantInError: []string{field + ": prices must not be lower than the base prices"},
		},
		{
			name:        "cheaper output than the base price",
			yaml:        strings.Replace(longContext, `input: "0.50", output: "2.50"`, `input: "0.50", output: "0.25"`, 1),
			wantInError: []string{field + ": prices must not be lower than the base prices"},
		},
		{
			name:        "unknown field",
			yaml:        strings.Replace(longContext, "above_prompt_tokens:", "above_tokens:", 1),
			wantInError: []string{"above_tokens"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse: expected an error, got nil")
			}
			for _, want := range tt.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			for _, not := range tt.notInError {
				if strings.Contains(err.Error(), not) {
					t.Errorf("error %q mentions %q", err, not)
				}
			}
		})
	}
}
