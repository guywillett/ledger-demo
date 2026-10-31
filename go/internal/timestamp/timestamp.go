// Package timestamp is a time written in JSON exactly as the Node implementation writes one:
// JavaScript's Date.toISOString(), UTC to the millisecond, "2026-10-30T09:21:52.130Z" (slice 3
// decision 44). For every timestamp the API answers with or an event carries, so Node's and Go's
// bytes are the same.
//
// Go's own encoding (time.Time's MarshalJSON, RFC 3339 with nanoseconds and the zone's offset)
// would also satisfy the contract, but differ from Node's for the same row.
package timestamp

import (
	"regexp"
	"time"
)

// Time is a time.Time with Node's JSON form. A defined type over time.Time: convert with
// timestamp.Time(t) and time.Time(ts); time.Time's methods don't carry over, only this one.
type Time time.Time

// layout is toISOString's format. Go's time layouts are written as the reference time
// (Mon Jan 2 15:04:05 2006); ".000" is three fractional digits, cut off, not rounded.
const layout = "2006-01-02T15:04:05.000Z"

// MarshalJSON writes the time in UTC, to the millisecond. Truncated, as Node does: node-postgres
// turns Postgres's microseconds into a Date by dropping the sub-millisecond digits (measured over
// all 10^6 fractions), and toISOString prints what's left.
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format(layout) + `"`), nil
}

// isoPattern is Zod's z.iso.datetime() with its defaults, as Node's request and event schemas use
// it: a date, T, hours, minutes and seconds with any fraction, and Z. No offset (+01:00 and +00:00
// are refused), seconds required, upper-case T and Z only, nothing around it (measured against
// Node's server, the cases in timestamp_test.go).
var isoPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.(\d+))?Z$`)

// ParseISO parses a timestamp from outside as Node's schemas accept one (Zod's z.iso.datetime()),
// to the millisecond, as JavaScript's Date holds it (further digits are cut off, not rounded). ok
// is false for anything Zod refuses.
//
// The pattern fixes every piece's position, so the date and time to the second are parsed with a
// fixed layout, which checks the calendar as Zod's regular expression does (a 29 February only in
// a leap year, 1900 not one and 2000 one; no hour 24, minute or second 60). The fraction is read
// here: Zod takes any number of digits.
func ParseISO(s string) (t time.Time, ok bool) {
	m := isoPattern.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02T15:04:05", s[:19]) // no zone in the layout: UTC
	if err != nil {
		return time.Time{}, false
	}
	ms := 0
	for i := range 3 { // the first three digits of the fraction, missing ones as 0
		ms *= 10
		if i < len(m[1]) {
			ms += int(m[1][i] - '0')
		}
	}
	return t.Add(time.Duration(ms) * time.Millisecond), true
}
