package api

import (
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// parseQuery splits a raw query string the way Node's API reads it (Fastify's querystring
// parser), which Go's url.ParseQuery doesn't quite:
//
//   - pairs are separated by & only. ParseQuery also rejects ;, dropping the whole pair
//     (?limit=2;x would be ignored, so the default would apply); Node keeps "2;x" as the value,
//     and it fails validation.
//   - a key or value whose %-escapes don't decode is kept as it was written (ParseQuery drops the
//     pair), so it fails validation too.
//   - a key without = has the value "".
//
// Repeated keys keep every value, in order; the caller decides what more than one means.
func parseQuery(raw string) map[string][]string {
	query := make(map[string][]string)
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		key = unescape(key)
		query[key] = append(query[key], unescape(value))
	}
	return query
}

// unescape decodes %-escapes and + (a space, in a query string), or returns s unchanged if it
// doesn't decode.
func unescape(s string) string {
	decoded, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

// jsNumber is JavaScript's Number(s): how Node reads ?limit (z.coerce.number()).
//
// Not the usual Go way (decision 54): reproduced so both implementations accept the same limits.
// Number() accepts more than decimal digits, and a mixed stack must answer a request the same
// whichever implementation gets it: surrounding whitespace (JavaScript's set) is ignored, "" is 0,
// and hexadecimal (0x10), octal (0o20), binary (0b10000), exponents (1e2, .5e1) and Infinity are
// numbers. Anything else is NaN. Only whether the result is a whole number from 1 to 200 matters.
func jsNumber(s string) float64 {
	s = strings.TrimFunc(s, isJSWhitespace)
	switch s {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			// With an explicit base, ParseUint accepts digits only: no sign, no underscores (those
			// only with base 0), as Number() after its prefix.
			n, err := strconv.ParseUint(s[2:], base, 64)
			if errors.Is(err, strconv.ErrRange) { // *strconv.NumError unwraps to its cause
				// Too large for 64 bits. JavaScript still gives a finite number (0x + 20 digits is
				// about 1.2e24), which fails as too big, not as infinite: any finite number above
				// the limit decides, and reads, the same.
				return math.MaxFloat64
			}
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		}
	}
	if !isDecimalLiteral(s) {
		return math.NaN()
	}
	// The grammar is checked above, so ParseFloat only converts (it accepts more, such as "inf",
	// hex floats and underscores, which Number() doesn't). Out of range, it returns +/-Inf.
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// isDecimalLiteral reports whether s is JavaScript's StrDecimalLiteral: an optional sign, digits
// with an optional fraction (or a fraction alone, ".5"), and an optional exponent.
func isDecimalLiteral(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	mantissa := 0
	for i < len(s) && isDigit(s[i]) {
		i, mantissa = i+1, mantissa+1
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i, mantissa = i+1, mantissa+1
		}
	}
	if mantissa == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exponent := 0
		for i < len(s) && isDigit(s[i]) {
			i, exponent = i+1, exponent+1
		}
		if exponent == 0 {
			return false
		}
	}
	return i == len(s)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isJSWhitespace is JavaScript's whitespace (its trim() and \s): what Number() ignores around a
// number. Not decision 56's separators, which are for IBANs and SEPA text.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}
