package backoff_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
)

// Node's test/shared/backoff.test.ts, plus the limits Go must get right without floating point.
func TestDelayDoublesUpToTheMaximum(t *testing.T) {
	b := backoff.Backoff{Base: time.Second, Max: 60 * time.Second}
	var got []time.Duration
	for _, attempt := range []int{1, 2, 3, 4, 6, 7, 50} {
		got = append(got, b.Delay(attempt))
	}
	want := []time.Duration{1, 2, 4, 8, 32, 60, 60}
	for i := range want {
		want[i] *= time.Second
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Node clamps attempts below 1 to the first delay; a huge attempt doesn't overflow.
	long := backoff.Backoff{Base: 30 * time.Second, Max: time.Hour}
	if d := long.Delay(0); d != 30*time.Second {
		t.Errorf("attempt 0: %v", d)
	}
	if d := long.Delay(1_000_000); d != time.Hour {
		t.Errorf("attempt 1e6: %v", d)
	}
}

func TestPauseReturnsEarlyWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	start := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	backoff.Pause(ctx, time.Minute)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Pause waited %v after cancellation", elapsed)
	}
}

func TestPauseWaitsTheDelay(t *testing.T) {
	start := time.Now()
	backoff.Pause(t.Context(), 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("Pause returned after %v", elapsed)
	}
}
