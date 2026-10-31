// Package submitter is the submitter's handler: one delivery of a withdrawal.held event, sent to
// the provider and its outcome recorded. Node: node/src/submitter/handler.ts. cmd/submitter runs it
// on the shared consumer process (consumer.Run).
//
// The process layer (decision 57).
package submitter

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// Handling one delivery: sends the withdrawal to the provider, with its TypeID as the
// Idempotency-Key, and records the outcome.
//
//	accepted     -> held -> submitted, with the provider's transfer ID
//	refused      -> held -> failed, funds released (only a definitive 422 releases funds)
//	unknown      -> nak: tried again later with the same key, never a new one (decision 26)
//	invalid, key conflict, transfer-ID mismatch -> our bug: terminated with an alert, the
//	                withdrawal left held for a person (decision 35)
//
// The provider call is never inside a database transaction (no transaction waits on the network);
// each recording step is a short transaction of its own afterwards.

// Provider is how the handler reaches the provider: provider.Client in the process, a fake in the
// tests. Defined here, where it is used, so the handler states the one method it needs (Node:
// ProviderClient).
type Provider interface {
	SubmitTransfer(ctx context.Context, w withdrawals.Withdrawal) Outcome
}

// Deps is what the handler runs with (Node: SubmitterDeps).
type Deps struct {
	Pool     *pgxpool.Pool
	Logger   *slog.Logger
	Provider Provider
	// After an unknown outcome or a transient failure: the redelivery delay for the attempt
	// (consumer.DeliveryRetry in the process).
	Retry backoff.Backoff

	// deliveryLog is where the delivery log is written: Pool, unless a test in this package makes
	// its writes fail (Node's tests pass a wrapped pool).
	deliveryLog db.Querier
}

// isBug classifies an error the steps return: a ledger invariant violation from MarkSubmitted or
// Release, as in the deposit-worker, or the provider's update having stored another transfer ID
// (decision 35). Terminated with an alert, the withdrawal left held; the error's message names both
// transfer IDs.
func isBug(err error) bool {
	if ledger.IsInvariantViolation(err) {
		return true
	}
	_, mismatch := errors.AsType[*withdrawals.TransferIDMismatchError](err)
	return mismatch
}

// Handle handles one delivery of a withdrawal.held event (consumer.ConsumeOptions.Handle).
// Recording the delivery, validating the event, settling the message and recording its outcome are
// the shared lifecycle's (consumer.Lifecycle).
func Handle(ctx context.Context, d Deps, m consumer.Message) consumer.Outcome {
	log := d.deliveryLog
	if log == nil {
		log = d.Pool
	}
	lifecycle := consumer.Lifecycle[events.WithdrawalHeldV1]{
		Consumer:  consumer.Submitter,
		EventType: events.SubjectWithdrawalHeld,
		Decode:    events.DecodeWithdrawalHeldV1,
		Pool:      log,
		Logger:    d.Logger,
		Retry:     d.Retry,
		IsBug:     isBug,
		// E.g. the database went away after an accepted call. The redelivery calls again with the
		// same key, gets the same transfer, and records it.
		TransientFailureMessage: "submission failed; will retry",
	}
	return lifecycle.Deliver(ctx, m, func(
		ctx context.Context,
		event events.WithdrawalHeldV1,
		delivery *consumer.Delivery,
	) (consumer.Settled, error) {
		return submit(ctx, d, event.WithdrawalID, delivery)
	})
}

func submit(
	ctx context.Context,
	d Deps,
	id ids.WithdrawalID,
	delivery *consumer.Delivery,
) (consumer.Settled, error) {
	delivery.Bind("withdrawal_id", id)

	// Read the withdrawal: the request is built from what is stored, not from the event.
	w, found, err := withdrawals.Get(ctx, d.Pool, id)
	if err != nil {
		return consumer.Settled{}, err
	}
	if !found {
		delivery.Log().Error("withdrawal.held event for an unknown withdrawal; terminating it")
		return delivery.Stop(ctx, consumer.Invalid, "unknown withdrawal")
	}
	// Already submitted or settled (e.g. the provider's update overtook us, decision 27): no call
	// needed.
	//
	// A deliberate exception to "no check-then-act" (CLAUDE.md): this read decides whether to make
	// the provider call. That rule prevents races where acting on a stale read changes state
	// wrongly. Here a stale read can only cause one extra call, which changes nothing: the provider
	// answers our repeated key with the same transfer, and MarkSubmitted's conditional transition
	// skips. Money and status are still decided only by the conditional transitions; the read
	// exists to avoid a needless provider call.
	if w.Status != withdrawals.StatusHeld {
		delivery.Log().Info("withdrawal no longer held; not submitting", "status", w.Status)
		return delivery.Done(ctx, consumer.Duplicate)
	}

	// The call, outside any transaction. Working first resets the message's ack_wait, so the call
	// (at most PROVIDER_TIMEOUT_MS, well under ack_wait) starts with the full lease, however long
	// the database steps before it took. Best-effort, as in the deposit-worker's review.
	if err := delivery.Message().Working(); err != nil {
		delivery.Log().Warn("could not signal working; submitting anyway", "err", err)
	}
	result := d.Provider.SubmitTransfer(ctx, w)

	// Record the outcome.
	switch result.Kind {
	case Accepted:
		recorded, err := db.InTxValue(ctx, d.Pool,
			func(tx db.Tx) (ledger.Result, error) {
				return ledger.MarkSubmitted(ctx, tx, id, result.TransferID)
			})
		if err != nil {
			return consumer.Settled{}, err
		}
		msg, outcome := "withdrawal submitted", consumer.Applied
		if recorded != ledger.Applied {
			msg, outcome = "withdrawal already moved on", consumer.Duplicate
		}
		delivery.Log().Info(msg, "transfer_id", result.TransferID, "submitted", recorded)
		return delivery.Done(ctx, outcome)
	case Refused:
		released, err := db.InTxValue(ctx, d.Pool,
			func(tx db.Tx) (ledger.Result, error) {
				return ledger.Release(ctx, tx, id, result.Reason, "")
			})
		if err != nil {
			return consumer.Settled{}, err
		}
		msg, outcome := "withdrawal refused by the provider; funds released", consumer.Applied
		if released != ledger.Applied {
			msg = "withdrawal refused by the provider, but already settled"
			outcome = consumer.Duplicate
		}
		delivery.Log().Info(msg, "reason", result.Reason, "released", released)
		return delivery.Done(ctx, outcome)
	case Unknown:
		// The transfer may or may not exist at the provider. The withdrawal stays held, funds
		// reserved, which is correct either way; the redelivery asks again with the same key.
		delivery.Log().Warn("provider outcome unknown; will retry",
			"cause", result.Cause, "retry_in_ms", delivery.RetryDelay().Milliseconds())
		return delivery.RetryLater(ctx)
	case Invalid:
		// The status and the summary only; never the provider's body (see Invalid).
		return bug(ctx, delivery, "provider rejected our request as invalid",
			"provider_status", 400, "provider_error_fields", result.Fields,
			"provider_body_bytes", result.BodyBytes)
	case KeyConflict:
		return bug(ctx, delivery,
			"provider reports our idempotency key reused with another request",
			"provider_status", 409)
	}
	return consumer.Settled{}, errors.New("unexpected provider outcome")
}

// bug is our bug, not a transient failure: retrying can't fix it, and nothing may move money on it.
// Terminated with an alert, whether or not the outcome can be recorded; the withdrawal stays held
// (funds reserved) for a person to resolve, and reconciliation lists it as stuck.
func bug(
	ctx context.Context,
	delivery *consumer.Delivery,
	why string,
	fields ...any,
) (consumer.Settled, error) {
	logging.Fatal(ctx, delivery.Log(), why+"; terminating the message, withdrawal left held",
		append(fields, "alert", true)...)
	return delivery.Stop(ctx, consumer.Failed, why)
}
