package usage

import (
	"github.com/ktripathi2281/tollgate/internal/config"
	"github.com/ktripathi2281/tollgate/internal/money"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// MaxBilledTokens caps the token counts that Cost multiplies. No real
// request comes near it. With config.MaxPrice it keeps the arithmetic far
// from overflow: 1e8 tokens at 1e10 micro-USD per million tokens is 1e18,
// and two of those fit in an int64.
const MaxBilledTokens = 100_000_000

// Cost returns what a request costs at price, in micro-USD: its prompt
// tokens at the input rate plus its completion tokens at the output rate,
// using the tier that its prompt length selects. Prices are per million
// tokens, so the total is divided by a million and rounded up: a fraction
// of a micro-dollar is charged as a whole one. Counts below zero count as
// zero, and counts above MaxBilledTokens as MaxBilledTokens.
func Cost(price config.Price, promptTokens, completionTokens int) money.Micros {
	prompt := min(max(promptTokens, 0), MaxBilledTokens)
	completion := min(max(completionTokens, 0), MaxBilledTokens)
	in, out := price.Rates(prompt)
	total := money.Micros(prompt)*in + money.Micros(completion)*out
	// Rounding once, on the total: adding one less than the divisor before
	// dividing rounds up.
	return (total + perMillion - 1) / perMillion
}

// perMillion converts a price per million tokens to a price per token.
const perMillion = 1_000_000

// messageOverhead is a bound on what a chat format adds to each message,
// in tokens: role markers and separators. Real formats add 3 to 7.
const messageOverhead = 16

// InputBound returns an upper bound on the prompt tokens of messages, for
// budget holds: the UTF-8 byte count of their text, plus messageOverhead
// for each message and once more for the start of the reply. A byte-level
// BPE tokenizer never produces more tokens than bytes (DECISIONS 4).
func InputBound(messages []provider.Message) int {
	n := messageOverhead
	for _, m := range messages {
		n += messageOverhead
		for _, part := range m.Parts {
			n += len(part)
		}
	}
	return n
}
