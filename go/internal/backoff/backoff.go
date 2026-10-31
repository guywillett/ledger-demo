// Package backoff is the retry timing shared by the long-running processes (the relay; later the
// deposit-worker and the submitter). Node: node/src/shared/backoff.ts.
//
// A foundation package (decision 57): it depends on nothing of ours.
package backoff

import (
	"context"
	"time"
)

// Backoff is a delay that starts at Base and doubles with each further attempt, up to Max. No
// jitter, as in Node.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay is the wait before attempt number attempt (1 for the first retry): Base on the first,
// doubling with each further attempt, at most Max. Attempts below 1 count as 1.
//
// Node computes base × 2^(attempt-1) with the exponent capped at 30, in floating point. A
// time.Duration is an int64 of nanoseconds, where 30 s × 2^30 would overflow, so this doubles
// step by step and stops at Max: the same values for every attempt.
func (b Backoff) Delay(attempt int) time.Duration {
	d := b.Base
	for i := 1; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	return min(d, b.Max)
}

// Pause waits for d, but returns early when ctx is cancelled (on shutdown), as Node's pause with
// its AbortSignal.
//
// select waits on whichever channel is ready first: ctx.Done() is closed when the context is
// cancelled, and the timer's channel receives when d has passed. The timer is stopped on the way
// out (defer), so an early return doesn't leave it running until it fires. Since Go 1.23 an
// unreferenced timer is garbage-collected even if never stopped, so this is tidiness, not a leak
// fix; it also says the timer's life ends here.
func Pause(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
