// Package timestamp is a time written in JSON exactly as the Node implementation writes one:
// JavaScript's Date.toISOString(), UTC to the millisecond, "2026-10-30T09:21:52.130Z" (slice 3
// decision 44). For every timestamp the API answers with or an event carries, so Node's and Go's
// bytes are the same.
//
// Go's own encoding (time.Time's MarshalJSON, RFC 3339 with nanoseconds and the zone's offset)
// would also satisfy the contract, but differ from Node's for the same row.
package timestamp

import "time"

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
