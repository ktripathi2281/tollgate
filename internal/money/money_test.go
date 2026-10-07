package money

import (
	"fmt"
	"math"
	"testing"
)

func TestParseUSD(t *testing.T) {
	tests := []struct {
		in   string
		want Micros
	}{
		{"0", 0},
		{"1", 1_000_000},
		{"15", 15_000_000},
		{"1.00", 1_000_000},
		{"2.5", 2_500_000},
		{"0.075", 75_000},
		{"0.0375", 37_500},
		{"0.000001", 1},
		{"0.000000", 0},
		{"007.5", 7_500_000},
		{"123456.789012", 123_456_789_012},
		// The largest value that fits in int64.
		{"9223372036854.775807", math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.in), func(t *testing.T) {
			got, err := ParseUSD(tt.in)
			if err != nil {
				t.Fatalf("ParseUSD(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseUSD(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseUSDRejects(t *testing.T) {
	inputs := []string{
		"",
		".",
		"-1",
		"+1",
		"1.0000001", // seven fractional digits
		".5",        // no whole part
		"5.",        // no fraction after the point
		"1.2.3",
		"1,5",
		"1_000",
		"1e3",
		" 1",
		"1 ",
		"$1",
		"abc",
		"NaN",
		"0x10",
		"9223372036854.775808", // one Micro above math.MaxInt64
		"9223372036855",        // whole part overflows once scaled
		"99999999999999999999", // overflows uint64 before scaling
	}
	for _, in := range inputs {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			got, err := ParseUSD(in)
			if err == nil {
				t.Errorf("ParseUSD(%q) = %d, want an error", in, got)
			}
		})
	}
}
