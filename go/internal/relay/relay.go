// Package relay publishes the outbox to JetStream. Node: node/src/relay/relay.ts (the batch) and
// the poll loop of node/src/relay/main.ts. Each batch is
//
//  1. claimed (one statement: leased to this relay, attempts counted; see outbox.Claim),
//  2. published event by event in id order, outside any transaction, each with its msg_id as the
//     Nats-Msg-Id, waiting for the stream's acknowledgement,
//  3. marked: published, or failed with a retry delay.
//
// At least once: a relay that crashes between 2 and 3 leaves the lease to expire, and the event is
// published again; within the stream's duplicate window the msg_id makes the stream drop the
// repeat, and beyond it consumers are idempotent on state. No ordering guarantee: retries and other
// relay instances publish out of order, and consumers don't depend on order.
//
// The process layer (decision 57), with cmd/relay wiring it to its configuration.
package relay

import (
	"context"
	"log/slog"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/outbox"
)

// Publisher is how an event gets onto NATS: broker.Publisher in the process, a recording or
// failing one in the tests. Node: the EventPublisher type in shared/nats.ts.
//
// Defined here, where it is used, rather than next to broker.Publisher: in Go a type satisfies an
// interface just by having its methods (implicitly, like a TypeScript structural type), so the
// consumer states the one method it needs and broker needn't know this interface exists.
type Publisher interface {
	Publish(
		ctx context.Context,
		subject string,
		payload []byte,
		msgID string,
	) (broker.PublishResult, error)
}

// Deps is what the relay runs with.
type Deps struct {
	Pool      db.Querier // the pool: claims and marks are single statements
	Publisher Publisher
	Logger    *slog.Logger
	BatchSize int
	Lease     time.Duration
	// RetryDelay is how long to wait before retrying an event whose publish failed, given its
	// attempts so far.
	RetryDelay func(attempts int) time.Duration
}

// BatchResult is how many events a batch claimed, and how many of those the stream acknowledged.
// The poll loop needs both: a batch with nothing published means publishing is failing (e.g. NATS
// is down), and claiming the next batch at once would only fail that one too.
type BatchResult struct {
	Claimed   int
	Published int
}

// PublishBatch claims and publishes one batch. A failed publish is recorded and the rest of the
// batch carries on (there is no order to preserve). A database error (claiming or marking) ends
// the batch and is returned (with no result: as usual in Go, a result that comes with an error
// isn't to be read): the events still leased are retried once their lease expires.
func PublishBatch(ctx context.Context, d Deps) (BatchResult, error) {
	events, err := outbox.Claim(ctx, d.Pool, d.BatchSize, d.Lease)
	if err != nil {
		return BatchResult{}, err
	}
	result := BatchResult{Claimed: len(events)}
	for _, event := range events {
		published, err := publishEvent(ctx, d, event)
		if err != nil {
			return BatchResult{}, err
		}
		if published {
			result.Published++
		}
	}
	return result, nil
}

// publishEvent publishes one claimed event and marks it; published is true if the stream
// acknowledged it.
func publishEvent(ctx context.Context, d Deps, event outbox.Event) (published bool, err error) {
	log := d.Logger.With("event_id", event.ID, event.Entity.LogAttr())

	result, err := d.Publisher.Publish(ctx, event.Subject, event.Payload, event.MsgID)
	if err != nil {
		retryIn := d.RetryDelay(event.Attempts)
		// Logged before marking: if marking fails too (the database is down), the publish error
		// and the event it was for are still on record.
		log.Warn("publish failed; will retry", "err", err, "subject", event.Subject,
			"attempts", event.Attempts, "retry_in_ms", retryIn.Milliseconds())
		_, markErr := outbox.MarkFailed(ctx, d.Pool, event.ID, err.Error(), retryIn)
		return false, markErr
	}

	// Skipped: another relay published and marked it after this one's lease ran out.
	marked, err := outbox.MarkPublished(ctx, d.Pool, event.ID)
	if err != nil {
		return true, err
	}
	fields := []any{"subject", event.Subject, "stream_seq", result.Seq}
	if result.Duplicate {
		// The stream already held this msg_id (a repeat publish within the duplicate window), so
		// it stored nothing new.
		fields = append(fields, "stream_duplicate", true)
	}
	if marked == outbox.Skipped {
		fields = append(fields, "already_marked", true)
	}
	log.Info("event published", fields...)
	return true, nil
}

// The poll loop's timing, as Node's main.ts.
var (
	// Between polls when caught up: 200 ms, doubling while idle, at most 1 s.
	idleDelay = backoff.Backoff{Base: 200 * time.Millisecond, Max: time.Second}
	// After a failed batch (e.g. the database is down), or one in which every publish failed: 1 s,
	// doubling, at most 30 s.
	failureRetry = backoff.Backoff{Base: time.Second, Max: 30 * time.Second}
	// PublishRetry is the wait before retrying an event whose publish failed: 1 s after the first
	// attempt, doubling per attempt, at most 60 s (Deps.RetryDelay in the process).
	PublishRetry = backoff.Backoff{Base: time.Second, Max: 60 * time.Second}
)

// Poll publishes batch after batch until ctx is cancelled (on shutdown). It never fails: a failed
// batch is logged and retried with backoff.
//
// A full batch means there may be more, so the next one runs at once. Otherwise the relay has
// caught up and waits: 200 ms after a batch that found something, then doubling while it keeps
// finding nothing, up to 1 s. A batch in which nothing could be published (e.g. NATS down, every
// publish failing fast) backs off like a failed batch, rather than racing through the backlog.
//
// Cancelling ctx stops the waiting and the loop, but not the batch in progress: it runs on
// context.WithoutCancel(ctx), which keeps ctx's values but never reports it cancelled, so its
// publishes and marks complete and every event it published is marked (nothing is published
// twice). A separate context.Background() would do the same; WithoutCancel says why. Each publish
// still has its own timeout (broker.PublishTimeout), so the batch can't hang on NATS.
//
// In the process, not in cmd/relay as Node keeps it in main.ts: here a test can cancel mid-batch
// and check the batch still finishes and marks.
func Poll(ctx context.Context, d Deps) {
	batchCtx := context.WithoutCancel(ctx)
	idle, failures := 0, 0
	for ctx.Err() == nil {
		batch, err := PublishBatch(batchCtx, d)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			d.Logger.Error("relay batch failed; retrying", "err", err, "attempt", failures)
			backoff.Pause(ctx, failureRetry.Delay(failures))
			continue
		}
		if batch.Claimed > 0 && batch.Published == 0 {
			// Each event is already backed off on its own (MarkFailed), but the rest of the backlog
			// would fail the same way: wait before claiming more.
			failures++
			d.Logger.Warn("nothing in the batch could be published; backing off",
				"claimed", batch.Claimed, "attempt", failures)
			backoff.Pause(ctx, failureRetry.Delay(failures))
			continue
		}
		failures = 0
		if batch.Claimed == d.BatchSize {
			idle = 0
			continue
		}
		if batch.Claimed == 0 {
			idle++
		} else {
			idle = 1
		}
		backoff.Pause(ctx, idleDelay.Delay(idle))
	}
}
