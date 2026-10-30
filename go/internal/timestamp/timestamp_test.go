package timestamp_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

func TestMarshalJSON(t *testing.T) {
	paris := time.FixedZone("CET", 3600)
	tests := []struct {
		in   time.Time
		want string
	}{
		// Microseconds, as Postgres stores them: truncated to milliseconds, not rounded.
		{time.Date(2026, 10, 30, 9, 21, 52, 130999000, time.UTC), `"2026-10-30T09:21:52.130Z"`},
		// Always three digits, even when they're zero.
		{time.Date(2026, 10, 30, 9, 21, 52, 0, time.UTC), `"2026-10-30T09:21:52.000Z"`},
		// Always UTC.
		{time.Date(2026, 10, 30, 10, 21, 52, 5000000, paris), `"2026-10-30T09:21:52.005Z"`},
	}
	for _, tt := range tests {
		got, err := json.Marshal(timestamp.Time(tt.in))
		if err != nil || string(got) != tt.want {
			t.Errorf("Marshal(%v) = %s, %v; want %s", tt.in, got, err, tt.want)
		}
	}
}
