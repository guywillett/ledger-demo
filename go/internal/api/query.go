package api

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// The parsing of a request's parameters, the query string's and the path's IDs, by Node's rules
// (Fastify's parser, Zod's schemas and messages): the bytes of a 400 must match Node's, so how a
// value is read, and what each problem says, is reproduced here rather than left to Go's defaults.
// Each list's parser (parseDepositsQuery, parseWithdrawalsQuery) is built from these.

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

// includeHistory reads ?include, as Node's include: z.literal('history').optional(): absent is
// false, exactly "history" true, anything else (another value, or the parameter given twice) a
// problem. Shared by the deposits' and the withdrawals' GETs.
func includeHistory(query map[string][]string) (bool, []ValidationError) {
	values, ok := query["include"]
	if !ok {
		return false, nil
	}
	if len(values) == 1 && values[0] == "history" {
		return true, nil
	}
	return false, []ValidationError{{Path: "include", Message: `Invalid input: expected "history"`}}
}

// parseTypeID parses value as a TypeID of kind K, as Node's typeIdSchema does: it must be well
// formed, have K's prefix and hold a UUIDv7. Anything else is typeIDProblem.
func parseTypeID[K ids.Kind](value, path string) (ids.ID[K], []ValidationError) {
	id, err := ids.FromTypeID[K](value)
	if err != nil {
		return ids.ID[K]{}, typeIDProblem[K](path)
	}
	return id, nil
}

// typeIDProblem is the one problem for a value at path that isn't a TypeID of kind K, naming the
// kind it must be, as Node's message does: "must be a dep_ ID".
func typeIDProblem[K ids.Kind](path string) []ValidationError {
	return []ValidationError{{Path: path, Message: "must be a " + ids.PrefixOf[K]() + "_ ID"}}
}

// idParam parses the path value name as a TypeID of kind K (see parseTypeID), the problem's path
// the parameter's name: idParam[ids.Account](r, "account_id").
func idParam[K ids.Kind](r *http.Request, name string) (ids.ID[K], []ValidationError) {
	return parseTypeID[K](r.PathValue(name), name)
}

// oneOfMessage is Zod's message for a value outside an enum (z.enum), measured against Node:
// `Invalid option: expected one of "held"|"submitted"`. The values are listed in the order given,
// so the order is part of the API's bytes.
func oneOfMessage[S ~string](values []S) string {
	quoted := make([]string, len(values))
	for i, s := range values {
		quoted[i] = `"` + string(s) + `"`
	}
	return "Invalid option: expected one of " + strings.Join(quoted, "|")
}

// parseStatusFilter reads ?status, a list's status filter (slice 4 decision 70): absent is nil (no
// filter); one of valid, its status; anything else (another value, an empty one, or the parameter
// given twice) is one problem with Zod's enum message, which lists valid in the order given.
func parseStatusFilter[S ~string](query map[string][]string, valid []S) (*S, []ValidationError) {
	values, ok := query["status"]
	if !ok {
		return nil, nil
	}
	s := S(values[0])
	if len(values) != 1 || !slices.Contains(valid, s) {
		return nil, []ValidationError{{Path: "status", Message: oneOfMessage(valid)}}
	}
	return &s, nil
}

// parseBefore reads ?before, a list's cursor (slice 4 decision 66): absent is nil (the first page);
// otherwise exactly one TypeID of kind K (see parseTypeID), the parameter given twice being the
// same problem. A position in time, never looked up: another account's ID, or one that names
// nothing, is accepted.
func parseBefore[K ids.Kind](query map[string][]string) (*ids.ID[K], []ValidationError) {
	values, ok := query["before"]
	if !ok {
		return nil, nil
	}
	if len(values) != 1 {
		return nil, typeIDProblem[K]("before")
	}
	before, invalid := parseTypeID[K](values[0], "before")
	if invalid != nil {
		return nil, invalid
	}
	return &before, nil
}

// defaultLimit is a list's page size when ?limit is absent (slice 4 decision 66: every list keeps
// limit's rules, 1 to 200 and 50 by default).
const defaultLimit = 50

// parseLimit reads ?limit, a list's page size: absent is defaultLimit; otherwise a whole number
// from 1 to 200, read as JavaScript's Number() reads it (see jsNumber), with Zod's messages
// (limitErrors). The parameter given twice reads as NaN, as Node's array does.
func parseLimit(query map[string][]string) (int, []ValidationError) {
	values, ok := query["limit"]
	if !ok {
		return defaultLimit, nil
	}
	n := math.NaN()
	if len(values) == 1 {
		n = jsNumber(values[0])
	}
	invalid := limitErrors(n)
	if len(invalid) != 0 {
		return 0, invalid
	}
	return int(n), nil
}

// limitErrors are Zod's errors for a limit read as n, in Zod's order, measured against Node. A
// value that isn't a finite number, or isn't whole, gets that one error. A whole number outside
// JavaScript's safe range (int() refuses it) gets that error and is still checked against the
// bounds, so 1e300 has two errors, as in Node; within the range, only the bounds are checked.
func limitErrors(n float64) []ValidationError {
	one := func(message string) []ValidationError {
		return []ValidationError{{Path: "limit", Message: message}}
	}
	var messages []string
	switch {
	case math.IsNaN(n):
		return one("Invalid input: expected number, received NaN")
	case math.IsInf(n, 1):
		return one("Invalid input: expected number, received Infinity")
	case math.IsInf(n, -1):
		return one("Invalid input: expected number, received -Infinity")
	case n != math.Trunc(n):
		return one("Invalid input: expected int, received number")
	case n > maxSafeInteger:
		messages = append(messages, "Too big: expected int to be <=9007199254740991")
	case n < -maxSafeInteger:
		messages = append(messages, "Too small: expected int to be >=-9007199254740991")
	}
	if n < 1 {
		messages = append(messages, "Too small: expected number to be >=1")
	}
	if n > 200 {
		messages = append(messages, "Too big: expected number to be <=200")
	}
	errs := make([]ValidationError, len(messages))
	for i, m := range messages {
		errs[i] = ValidationError{Path: "limit", Message: m}
	}
	return errs
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, 2^53 - 1: Zod's int() accepts no larger.
const maxSafeInteger = 1<<53 - 1
