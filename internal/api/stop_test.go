package api

import (
	"encoding/json"
	"slices"
	"testing"
)

// These tests are for the M1 exercise, parseStop.

func TestParseStop(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{`"END"`, []string{"END"}},
		{`"\n\n"`, []string{"\n\n"}},
		{`"¿qué?"`, []string{"¿qué?"}},
		{`["a"]`, []string{"a"}},
		{`["a", "b", "c", "d"]`, []string{"a", "b", "c", "d"}},
		{`["a", "a"]`, []string{"a", "a"}}, // duplicates are harmless
		{`[]`, nil},                        // an empty array means no stop sequences
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := parseStop(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatalf("parseStop(%s): unexpected error: %v", tt.raw, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseStop(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseStopRejects(t *testing.T) {
	inputs := []string{
		`""`,                        // empty sequence
		`["a", ""]`,                 // empty sequence in an array
		`["a", "b", "c", "d", "e"]`, // more than four
		`["a", null]`,               // null is not a sequence
		`[1]`,                       // wrong element type
		`[["a"]]`,                   // nested array
		`1`,                         // wrong type
		`true`,                      // wrong type
		`{}`,                        // wrong type
		`{"0": "a"}`,                // an object is not an array
	}
	tooMany, err := json.Marshal(slices.Repeat([]string{"x"}, maxStopSequences+1))
	if err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, string(tooMany))

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got, err := parseStop(json.RawMessage(in))
			if err == nil {
				t.Errorf("parseStop(%s) = %q, want an error", in, got)
			}
		})
	}
}
