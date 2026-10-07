// Package money holds the gateway's money type. Amounts are integer
// micro-USD: there are no floats anywhere on the money path, because binary
// floating point can't represent most decimal amounts exactly.
package money

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Micros is an amount in micro-USD. One US dollar is 1_000_000 Micros.
type Micros int64

// PerUSD is the number of Micros in one US dollar.
const PerUSD Micros = 1_000_000

// ParseUSD parses a non-negative decimal USD amount such as "0.075" or "15"
// into Micros. It accepts plain digits with at most one decimal point and at
// most six digits after it, with at least one digit on each side of the
// point. It rejects signs, exponents, spaces, separators, and any value that
// would overflow int64.
//
// Prices in the config are decimal strings parsed with this function: "0.075"
// USD per million tokens is 75_000 Micros per million tokens.
func ParseUSD(s string) (Micros, error) {
	whole, frac, hasPoint := strings.Cut(s, ".")
	if whole == "" || (hasPoint && frac == "") {
		return 0, fmt.Errorf("money: %q is not a decimal amount", s)
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("money: %q has more than 6 decimal places", s)
	}

	// ParseUint in base 10 accepts digits only: no sign, spaces, exponent,
	// underscores or hex prefix.
	dollars, err := strconv.ParseUint(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: parse %q: %w", s, err)
	}
	var micros uint64
	if frac != "" {
		// Right-pad to six digits: ".075" is 75_000 millionths, not 75.
		micros, err = strconv.ParseUint(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("money: parse %q: %w", s, err)
		}
	}

	// Go integer arithmetic wraps silently, so check before multiplying:
	// dollars*1e6 + micros <= MaxInt64 rearranges to the test below.
	if dollars > (math.MaxInt64-micros)/uint64(PerUSD) {
		return 0, fmt.Errorf("money: %q is too large", s)
	}
	return Micros(dollars*uint64(PerUSD) + micros), nil
}
