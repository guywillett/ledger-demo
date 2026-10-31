// Package money is the amount type: integer minor units (cents), never floats (ADR 0001,
// docs/decisions/0001-money-representation.md).
//
// Named Amount, not Money: Go code names it with its package, and money.Amount reads better than
// money.Money (and matches the amount_minor fields).
package money

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
)

// Amount is an amount in minor units. Postgres stores it as bigint, so int64 holds every value
// the database can, and pgx reads and writes it directly (Amount's underlying type is int64).
// Arithmetic is plain int64 arithmetic; there is no float conversion anywhere.
//
// In JSON an Amount is a string of digits ("1250"), as in Node's API, events and logs: a JSON
// number above 2^53 is rounded by JavaScript (and many other) parsers.
type Amount int64

// The reasons ParseAmount gives. Problem details may word them differently from Node (decision
// 42); the shared vectors (step 2) pin which inputs are rejected, not the wording.
var (
	ErrNotPositiveWholeNumber = errors.New("must be a positive whole number of minor units, as a string")
	ErrTooLarge               = errors.New("is larger than the maximum amount")
)

// MaxAmountLength is the most characters an amount may have: the length of the largest bigint,
// 9223372036854775807 (Node's amountMinorSchema .max(19)).
const MaxAmountLength = 19

// ParseAmount parses an amount from outside (a request or a notification), with the rules of
// Node's amountMinorSchema (node/src/shared/schemas.ts):
//
//   - at most MaxAmountLength characters;
//   - digits only, ASCII 0-9: no sign, no decimal point, no spaces, no exponent;
//   - no leading zero, and so no zero: the amount must be positive;
//   - at most the largest bigint.
//
// Node checks the length and the pattern before converting to a BigInt; checking each character
// here gives the same accept/reject decision for every input. Rules to confirm with the shared
// vectors (step 2): that Node rejects every non-ASCII digit (its regex [0-9] does), and that a
// 19-digit value above the maximum fails as too large rather than as too long.
func ParseAmount(s string) (Amount, error) {
	if len(s) == 0 || len(s) > MaxAmountLength || s[0] == '0' {
		return 0, ErrNotPositiveWholeNumber
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrNotPositiveWholeNumber
		}
	}
	// Only digits remain, so ParseInt can fail only by being out of range.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrTooLarge
	}
	return Amount(n), nil
}

// String returns the digits, e.g. "1250".
func (a Amount) String() string {
	return strconv.FormatInt(int64(a), 10)
}

// MarshalJSON writes the amount as a JSON string of digits: "1250", not 1250. Balances can be
// zero, so this writes "0" too; only parsing insists on a positive amount.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON reads an amount field of a request or notification: a JSON string, parsed by
// ParseAmount. A JSON number (1250, unquoted) is rejected, as Node's z.string() rejects it.
func (a *Amount) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return ErrNotPositiveWholeNumber
	}
	parsed, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// LogValue (slog.LogValuer) logs an amount as a digit string, as Node's logger does with BigInts,
// whichever slog handler writes it.
func (a Amount) LogValue() slog.Value {
	return slog.StringValue(a.String())
}
