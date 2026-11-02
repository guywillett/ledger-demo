// Package depositworker is the deposit-worker's handler: one delivery of a deposit.created event,
// through the deposit's lifecycle. Node: node/src/deposit-worker/handler.ts. cmd/deposit-worker
// runs it on the shared consumer process (consumer.Run).
//
// The process layer (decision 57).
package depositworker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/compliance"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
)

// Handling one delivery: the deposit-worker's steps in the slice spec. Posting and compliance take
// turns, each step its own transaction:
//
//	Tx 1 book (posting) -> Tx 2 start review (compliance) -> review delay -> decision ->
//	Tx 3 decide (compliance) -> Tx 4 post the decision (posting)
//
// Every step is a conditional transition, so a redelivery (after a crash, a timeout or a duplicate
// publish) repeats only the steps that haven't happened yet: completed ones skip.

// Deps is what the handler runs with (Node: HandlerDeps).
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// Decide is the compliance decision for a deposit under review (random in the demo). It can't
	// fail, as Node's decide(): it runs on every delivery, duplicates included, so a decider that
	// could fail would fail deliveries that need no decision. (A panic, as a throw in Node, is a
	// transient failure: the lifecycle recovers it.)
	Decide func() compliance.Decision
	// How long the pseudo review takes.
	ReviewDelay time.Duration
	// During the review, signal Working at least this often (well under the consumer's
	// ack_wait), so the message isn't redelivered while it's being processed.
	WorkingInterval time.Duration
	// After a transient failure, the delay before JetStream redelivers (consumer.DeliveryRetry in
	// the process).
	Retry backoff.Backoff

	// deliveryLog is where the delivery log is written: Pool, unless a test in this package makes
	// its writes fail (Node's tests pass a wrapped pool, failingDeliveryFinish).
	deliveryLog db.Querier
}

// Handle handles one delivery of a deposit.created event (consumer.ConsumeOptions.Handle).
// Recording the delivery, validating the event, settling the message and recording its outcome
// are the shared lifecycle's (consumer.Lifecycle).
func Handle(ctx context.Context, d Deps, m consumer.Message) consumer.Outcome {
	log := d.deliveryLog
	if log == nil {
		log = d.Pool
	}
	lifecycle := consumer.Lifecycle[events.DepositCreatedV1]{
		Consumer:  consumer.DepositWorker,
		EventType: events.SubjectDepositCreated,
		Decode:    events.DecodeDepositCreatedV1,
		Pool:      log,
		Logger:    d.Logger,
		Retry:     d.Retry,
		IsBug:     ledger.IsInvariantViolation,
	}
	return lifecycle.Deliver(ctx, m, func(
		ctx context.Context,
		event events.DepositCreatedV1,
		delivery *consumer.Delivery,
	) (consumer.Settled, error) {
		return process(ctx, d, event.DepositID, delivery)
	})
}

// process runs the steps for one deposit and settles the message.
func process(
	ctx context.Context,
	d Deps,
	id ids.DepositID,
	delivery *consumer.Delivery,
) (consumer.Settled, error) {
	delivery.Bind("deposit_id", id)

	// Each result is "applied" (this delivery made the transition) or "skipped"; each step is a
	// closure (a function literal using id from here) run in its own transaction.
	booked, err := db.InTxValue(ctx, d.Pool, func(tx db.Tx) (ledger.Result, error) {
		return ledger.Book(ctx, tx, id)
	})
	if err != nil {
		return consumer.Settled{}, err
	}
	reviewStarted, err := db.InTxValue(ctx, d.Pool,
		func(tx db.Tx) (compliance.Result, error) {
			return compliance.StartReview(ctx, tx, id)
		})
	if err != nil {
		return consumer.Settled{}, err
	}
	// Only the delivery that started the review waits it out. A duplicate (everything will skip),
	// an unknown deposit, or a redelivery after a crash between Tx 2 and Tx 3 goes straight on:
	// the review has already happened from the system's point of view, and Tx 3 still records at
	// most one decision.
	if reviewStarted == compliance.Applied {
		if err := review(ctx, d, delivery.Message(), delivery.Log()); err != nil {
			return consumer.Settled{}, err
		}
	}
	decision := d.Decide()
	decided, err := db.InTxValue(ctx, d.Pool,
		func(tx db.Tx) (compliance.Result, error) {
			return compliance.Decide(ctx, tx, id, decision)
		})
	if err != nil {
		return consumer.Settled{}, err
	}
	// Which way to post comes from the recorded state, not from decision: on a redelivery after an
	// earlier decision, the one just made was discarded by Tx 3.
	posted, err := db.InTxValue(ctx, d.Pool, func(tx db.Tx) (ledger.Posted, error) {
		return ledger.PostDecision(ctx, tx, id)
	})
	if err != nil {
		return consumer.Settled{}, err
	}

	changed := booked == ledger.Applied || reviewStarted == compliance.Applied ||
		decided == compliance.Applied || posted != ledger.PostedSkipped
	if changed {
		decidedField := "skipped"
		if decided == compliance.Applied {
			decidedField = decision.Outcome()
		}
		delivery.Log().Info("deposit processed", "booked", booked, "review_started", reviewStarted,
			"decided", decidedField, "posted", posted)
		return delivery.Done(ctx, consumer.Applied)
	}

	// Nothing changed: either everything had already happened (a duplicate), or there is no such
	// deposit (the event refers to nothing, e.g. after a database reset). This read only classifies
	// the outcome; every transition has already been attempted.
	exists, err := deposits.Exists(ctx, d.Pool, id)
	if err != nil {
		return consumer.Settled{}, err
	}
	if !exists {
		delivery.Log().Error("deposit.created event for an unknown deposit; terminating it")
		return delivery.Stop(ctx, consumer.Invalid, "unknown deposit")
	}
	delivery.Log().Info("duplicate delivery: deposit already processed")
	return delivery.Done(ctx, consumer.Duplicate)
}

// review is the pseudo review: wait ReviewDelay, telling JetStream we're still working (Working)
// before each stretch of at most WorkingInterval, so the message isn't redelivered meanwhile.
//
// Working is a best-effort signal, as Node's fire-and-forget working(): if it can't be sent (the
// connection has closed; while reconnecting nats.go buffers it), the worst outcome is an early
// redelivery, which the conditional steps handle, so it is logged and the review carries on.
//
// Cancellable, unlike Node's sleep: each stretch is a select on the context and a timer, so when
// shutdown's grace period runs out and the handler's context is cancelled (decision 59), the
// review stops at once and returns the context's error. The lifecycle then settles it as a
// transient failure: a nak with the retry delay, recorded failed. Nothing after the review has
// run, so the deposit stays under_review (Tx 2 committed), and the redelivery skips the review
// (its start skips) and goes on to decide and post, as after a crash between Tx 2 and Tx 3.
func review(ctx context.Context, d Deps, m consumer.Message, log *slog.Logger) error {
	for remaining := d.ReviewDelay; remaining > 0; {
		if err := m.Working(); err != nil {
			log.Warn("could not signal working; carrying on with the review", "err", err)
		}
		wait := remaining
		if d.WorkingInterval > 0 { // zero or less: no interval, one stretch (never a zero wait)
			wait = min(remaining, d.WorkingInterval)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("review interrupted: %w", ctx.Err())
		case <-timer.C:
		}
		remaining -= wait
	}
	return nil
}
