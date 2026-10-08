package usage

import (
	"testing"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"Hello there!", 3},
		{"日本語のテキスト", 2}, // 8 characters; counted in characters, not bytes
	}
	for _, tt := range tests {
		if got := EstimateTokens(tt.text); got != tt.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
		}
	}
}

func TestEstimatePrompt(t *testing.T) {
	tests := []struct {
		name     string
		messages []provider.Message
		want     int
	}{
		{"no messages", nil, 1},
		{"empty content", []provider.Message{{Role: provider.RoleUser, Parts: []string{""}}}, 1},
		{
			"parts and messages are added up before rounding",
			[]provider.Message{
				{Role: provider.RoleSystem, Parts: []string{"ab", "cd"}},
				{Role: provider.RoleUser, Parts: []string{"efg"}},
			},
			2, // 7 characters
		},
	}
	for _, tt := range tests {
		if got := EstimatePrompt(tt.messages); got != tt.want {
			t.Errorf("%s: EstimatePrompt = %d, want %d", tt.name, got, tt.want)
		}
	}
}
