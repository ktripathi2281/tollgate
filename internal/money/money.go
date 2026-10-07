// Package money holds the gateway's money type. Amounts are integer
// micro-USD: there are no floats anywhere on the money path, because binary
// floating point can't represent most decimal amounts exactly.
package money

import "errors"

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
	// EXERCISE: implement ParseUSD; money_test.go has every case it must pass.
	// Hint: split on '.', parse each side with strconv.ParseUint (base 10), right-pad
	// the fraction to six digits, and check for overflow before multiplying by PerUSD.
	_ = s
	return 0, errors.New("money: ParseUSD is not implemented yet")
}
