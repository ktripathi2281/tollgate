// Package usage counts tokens. From M5 it also computes cost, and from M6
// it records requests.
package usage

import (
	"unicode/utf8"

	"github.com/ktripathi2281/tollgate/internal/provider"
)

// charsPerToken is the estimate's ratio. It is a rough average for English
// text with common tokenizers; provider counts are always preferred.
const charsPerToken = 4

// EstimateTokens estimates the tokens in text at about four characters per
// token, rounded up. The gateway only estimates when a provider reports no
// counts, for example after an interrupted stream.
func EstimateTokens(text string) int {
	return tokensForChars(utf8.RuneCountInString(text))
}

// EstimatePrompt estimates the prompt tokens of messages: at least 1.
func EstimatePrompt(messages []provider.Message) int {
	chars := 0
	for _, m := range messages {
		for _, part := range m.Parts {
			chars += utf8.RuneCountInString(part)
		}
	}
	return max(1, tokensForChars(chars))
}

func tokensForChars(chars int) int {
	return (chars + charsPerToken - 1) / charsPerToken
}
