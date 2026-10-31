package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// Ingestion's second kind of provider notification (the first is RecordNotification): the
// provider's result for a transfer we sent. Recorded, then settled, in the caller's transaction
// (slice 2 decision 29): executed -> held to external, failed -> funds released. Node:
// node/src/ingestion/transfer-updates.ts.
//
// The money is decided by the withdrawal's conditional transition, never by the record: the
// transfer_updates row only recognises a resent update and keeps contradictions and unmatched
// updates on file. Those are committed, not rolled back, so they stay visible; they move no money
// (decision 35). Only invariant violations (ledger bugs) are returned, and roll back.

// TransferUpdate is a provider's transfer update, validated.
type TransferUpdate struct {
	ProviderEventID string
	TransferID      withdrawals.TransferID
	// As the provider sent it.
	Reference string
	// The withdrawal the reference names, when it is a wd_ TypeID holding a UUIDv7; nil otherwise.
	WithdrawalID *ids.WithdrawalID
	Status       withdrawals.Status // StatusExecuted or StatusFailed
	// Set exactly for failed.
	Reason     *string
	OccurredAt time.Time
}

// UpdateKind is what recording a transfer update did. (Prefixed: the package's Kind is the deposit
// notification's.)
type UpdateKind int

const (
	// UpdateApplied: a new update, and it settled the withdrawal.
	UpdateApplied UpdateKind = iota + 1
	// UpdateDuplicate: nothing settled again: the same event resent (NotificationCount > 1), or a
	// second event for the result already recorded.
	UpdateDuplicate
	// UpdateConflict: a contradiction: the withdrawal already has the other final status, or
	// another transfer ID. Recorded, moving no money.
	UpdateConflict
	// UpdateEventConflict: the provider reused one of its event IDs for different details (its
	// bug). Nothing changed.
	UpdateEventConflict
	// UpdateUnmatched: the reference names no withdrawal: recorded, nothing to settle. Not a timing
	// race (a withdrawal exists before it is submitted), so a misroute or the provider's bug.
	UpdateUnmatched
)

// WithdrawalState is a withdrawal's status and transfer ID, as stored or as reported.
type WithdrawalState struct {
	Status     withdrawals.Status
	TransferID *withdrawals.TransferID // nil when none is stored
}

// UpdateResult is what RecordTransferUpdate did (Node: UpdateResult, a union), one struct with a
// Kind: the fields a Kind doesn't use are zero.
type UpdateResult struct {
	Kind              UpdateKind
	Withdrawal        ids.WithdrawalID // all but EventConflict and Unmatched
	NotificationCount int              // Duplicate
	Stored, Reported  WithdrawalState  // Conflict
}

// One statement records the update, with the withdrawal resolved from the reference, or recognises
// a resend (same event ID, same details: count it). The outcome stored here is provisional
// ('applied', or 'unmatched' when unresolved); settling sets the final one. Node's text exactly.
//
// IS NOT DISTINCT FROM, not =: reason is NULL for an executed update, and NULL = NULL is not true
// in SQL (it is NULL), which would make every resent executed update look like an event-ID
// conflict. Returns no row when the event ID is stored with other details. RETURNING
// (old.id IS NULL): Postgres 18's old row is NULL for a row this statement inserted, so inserted
// tells a new update from a counted resend.
const recordTransferUpdateSQL = `
INSERT INTO transfer_updates (provider_event_id, withdrawal_id, reference, transfer_id, status,
                              reason, occurred_at, outcome)
SELECT $1, w.id, $3, $4, $5, $6, $7, CASE WHEN w.id IS NULL THEN 'unmatched' ELSE 'applied' END
FROM (SELECT $2::uuid AS id) ref
LEFT JOIN withdrawals w ON w.id = ref.id
ON CONFLICT (provider_event_id) DO UPDATE
  SET notification_count = transfer_updates.notification_count + 1
  WHERE (transfer_updates.reference, transfer_updates.transfer_id, transfer_updates.status,
         transfer_updates.reason)
        IS NOT DISTINCT FROM (EXCLUDED.reference, EXCLUDED.transfer_id, EXCLUDED.status,
                              EXCLUDED.reason)
RETURNING id, withdrawal_id, notification_count, (old.id IS NULL) AS inserted
`

const setUpdateOutcomeSQL = `UPDATE transfer_updates SET outcome = $2 WHERE id = $1`

// RecordTransferUpdate records a transfer update and, if it is new and matched, settles its
// withdrawal, in the caller's transaction (a Tx: the record, the settlement and the final outcome
// commit together).
func RecordTransferUpdate(ctx context.Context, tx db.Tx, u TransferUpdate) (UpdateResult, error) {
	// 1. Record.
	var (
		rowID      int64
		withdrawal *ids.WithdrawalID // NULL: unmatched
		count      int
		inserted   bool
	)
	err := tx.QueryRow(ctx, recordTransferUpdateSQL, u.ProviderEventID, u.WithdrawalID,
		u.Reference, u.TransferID, u.Status, u.Reason, u.OccurredAt).
		Scan(&rowID, &withdrawal, &count, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return UpdateResult{Kind: UpdateEventConflict}, nil
	}
	if err != nil {
		return UpdateResult{}, fmt.Errorf("record transfer update %s: %w", u.ProviderEventID, err)
	}
	if withdrawal == nil {
		// Unmatched, whether new or resent: nothing to settle.
		return UpdateResult{Kind: UpdateUnmatched}, nil
	}
	if !inserted {
		// The same event resent: its first receipt already settled it, atomically.
		return UpdateResult{
			Kind: UpdateDuplicate, Withdrawal: *withdrawal, NotificationCount: count,
		}, nil
	}

	// 2. Settle a new, matched update.
	result, err := settleWithdrawal(ctx, tx, *withdrawal, u)
	if err != nil {
		return UpdateResult{}, err
	}

	// 3. The final outcome (the provisional one was 'applied').
	if result.Kind != UpdateApplied {
		_, err := tx.Exec(ctx, setUpdateOutcomeSQL, rowID, outcomeName(result.Kind))
		if err != nil {
			return UpdateResult{},
				fmt.Errorf("transfer update %s outcome: %w", u.ProviderEventID, err)
		}
	}
	return result, nil
}

// outcomeName is the outcome column's value for an update's kind. Every kind is named (a closed
// set, which the exhaustive linter checks): only a settled update's (applied, duplicate, conflict)
// is ever written by setUpdateOutcomeSQL; unmatched is written by the record itself, and an event
// conflict records nothing.
func outcomeName(k UpdateKind) string {
	switch k {
	case UpdateApplied:
		return "applied"
	case UpdateDuplicate:
		return "duplicate"
	case UpdateConflict:
		return "conflict"
	case UpdateUnmatched:
		return "unmatched"
	case UpdateEventConflict:
		return "event_conflict" // never stored
	}
	return "" // not a Kind: the CHECK refuses it
}

func settleWithdrawal(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	u TransferUpdate,
) (UpdateResult, error) {
	reported := WithdrawalState{Status: u.Status, TransferID: &u.TransferID}
	var settled ledger.Result
	var err error
	if u.Status == withdrawals.StatusExecuted {
		settled, err = ledger.Execute(ctx, tx, id, u.TransferID)
	} else {
		reason := "failed"
		if u.Reason != nil {
			reason = *u.Reason
		}
		settled, err = ledger.Release(ctx, tx, id, reason, u.TransferID)
	}
	if _, mismatch := errors.AsType[*withdrawals.TransferIDMismatchError](err); mismatch {
		// Another transfer than the one stored: recorded as a conflict, and the transaction
		// carries on. That is possible because the error comes after statements that succeeded
		// (the transition's UPDATE matched no row, then a read found the other transfer ID), not
		// from a failed one, so Postgres hasn't aborted the transaction. An insufficient-funds
		// hold is the opposite case: there the balance UPDATE itself fails, Postgres aborts the
		// transaction, and the caller can only roll it back (ledger.CreateWithdrawal).
		stored, err := storedState(ctx, tx, id)
		if err != nil {
			return UpdateResult{}, err
		}
		return conflict(id, stored, reported), nil
	}
	if err != nil {
		return UpdateResult{}, err // an invariant violation: rolled back by the caller
	}
	if settled == ledger.Applied {
		return UpdateResult{Kind: UpdateApplied, Withdrawal: id}, nil
	}

	// Skipped: already final. This read only classifies; the transition has decided.
	stored, err := storedState(ctx, tx, id)
	if err != nil {
		return UpdateResult{}, err
	}
	if stored.Status == u.Status {
		// A second event for the same result.
		return UpdateResult{Kind: UpdateDuplicate, Withdrawal: id, NotificationCount: 1}, nil
	}
	// The other final status: executed then failed, or the reverse, which SEPA doesn't allow (a
	// return after execution is a separate flow, out of scope).
	return conflict(id, stored, reported), nil
}

func conflict(id ids.WithdrawalID, stored, reported WithdrawalState) UpdateResult {
	return UpdateResult{Kind: UpdateConflict, Withdrawal: id, Stored: stored, Reported: reported}
}

func storedState(ctx context.Context, tx db.Tx, id ids.WithdrawalID) (WithdrawalState, error) {
	w, found, err := withdrawals.Get(ctx, tx, id)
	if err != nil {
		return WithdrawalState{}, err
	}
	if !found {
		// The update's row references it (a foreign key), and withdrawals are never deleted.
		return WithdrawalState{}, fmt.Errorf("withdrawal %v disappeared", id)
	}
	return WithdrawalState{Status: w.Status, TransferID: w.ProviderTransferID}, nil
}
