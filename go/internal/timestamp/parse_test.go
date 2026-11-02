package timestamp_test

import (
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// What Node's z.iso.datetime() accepts and refuses, measured against Node's server
// (POST /provider/transfer-updates' occurred_at), case by case.
func TestParseISOAcceptsWhatZodAccepts(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" for refused; otherwise the time to the millisecond
	}{
		{"2026-10-24T09:00:00Z", "2026-10-24T09:00:00.000Z"},
		{"2026-10-24T09:00:00.000Z", "2026-10-24T09:00:00.000Z"},
		{"2026-10-24T09:00:00.1Z", "2026-10-24T09:00:00.100Z"},
		{"2026-10-24T09:00:00.123456789Z", "2026-10-24T09:00:00.123Z"}, // cut off, not rounded
		{"2026-10-24T09:00:00.9999999999999Z", "2026-10-24T09:00:00.999Z"},
		{"2028-02-29T00:00:00Z", "2028-02-29T00:00:00.000Z"},
		{"2000-02-29T00:00:00Z", "2000-02-29T00:00:00.000Z"},
		{"0000-02-29T00:00:00Z", "0000-02-29T00:00:00.000Z"},
		{"2026-10-24T09:00Z", ""},         // no seconds
		{"2026-10-24T09:00:00+01:00", ""}, // an offset
		{"2026-10-24T09:00:00+00:00", ""}, // even a zero one
		{"2026-10-24t09:00:00z", ""},      // lower case
		{"2026-10-24T09:00:00z", ""},      // lower-case z
		{"2026-10-24 09:00:00Z", ""},      // a space for T
		{"2026-02-29T00:00:00Z", ""},      // not a leap year
		{"1900-02-29T00:00:00Z", ""},      // a century, not a leap year
		{"2026-10-24T24:00:00Z", ""},      // hour 24
		{"2026-10-24T09:00:60Z", ""},      // second 60
		{"2026-10-24T09:60:00Z", ""},      // minute 60
		{"2026-10-24T09:00:00.Z", ""},     // an empty fraction
		{"2026-10-24T09:00:00", ""},       // no Z
		{"20261024T090000Z", ""},          // the basic format
		{"2026-13-01T00:00:00Z", ""},      // month 13
		{"2026-04-31T00:00:00Z", ""},      // 31 April
		{"2026-10-24T09:00:00,5Z", ""},    // a decimal comma
		{"+2026-10-24T09:00:00Z", ""},     // a signed year
		{"2026-1-24T09:00:00Z", ""},       // a one-digit month
		{"2026-10-24T9:00:00Z", ""},       // a one-digit hour
		{" 2026-10-24T09:00:00Z", ""},     // a leading space
		{"2026-10-24T09:00:00Z ", ""},     // a trailing space
		{"", ""},
	}
	for _, tt := range tests {
		got, ok := timestamp.ParseISO(tt.in)
		switch {
		case tt.want == "" && ok:
			t.Errorf("%q: accepted as %v, want refused", tt.in, got)
		case tt.want != "" && (!ok || got.Location() != time.UTC ||
			got.Format("2006-01-02T15:04:05.000Z") != tt.want):
			t.Errorf("%q: %v %v, want %s", tt.in, got, ok, tt.want)
		}
	}
}
