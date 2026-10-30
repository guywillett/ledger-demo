package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

// unsettled are the statuses a settlement can start from: funds reserved, outcome not known yet.
// A settlement applies whether or not the submitter has recorded its submission yet (slice 2
// decision 27).
var unsettled = []Status{StatusHeld, StatusSubmitted}

// Transition is one of the legal status changes, with the values it carries. Its fields are
// unexported, so a Transition comes only from Submit, Execute or Release, as deposits' come only
// from their constants: an illegal change (executed -> held, submitted -> submitted) can't be
// written. Node's types also tie values to transitions; here:
//
//   - a reason only for failed: impossible. Only Release takes one.
//   - a transfer ID required for a submission, and a reason for a release: rejected at run time,
//     when the value is "". A function parameter can't require a non-empty string, as Node's types
//     can't either (both accept an empty string); Go rejects it, Node would store it.
//   - the zero Transition (none of the three): rejected at run time.
//
// Optional values are "" for none, as deposits' reason: neither a provider transfer ID nor a
// reason is ever validly empty, so "" is free to mean "not given", and callers needn't take the
// address of a string to pass one.
type Transition struct {
	from       []Status
	to         Status
	transferID TransferID
	reason     string
}

// Submit is held -> submitted, recording the provider's transfer ID (required).
func Submit(transferID TransferID) Transition {
	return Transition{from: []Status{StatusHeld}, to: StatusSubmitted, transferID: transferID}
}

// Execute is held | submitted -> executed. transferID is the provider's, if the caller has it
// ("" if not): its update may arrive before the submitter has recorded the submission, and this
// is then the only chance to store it.
func Execute(transferID TransferID) Transition {
	return Transition{from: unsettled, to: StatusExecuted, transferID: transferID}
}

// Release is held | submitted -> failed, with the reason (required) and, as for Execute, the
// provider's transfer ID if the caller has it.
func Release(reason string, transferID TransferID) Transition {
	return Transition{from: unsettled, to: StatusFailed, transferID: transferID, reason: reason}
}

func (t Transition) check() error {
	switch {
	case t.to == "":
		return errors.New("invalid withdrawal transition (none of Submit, Execute, Release)")
	case t.to == StatusSubmitted && t.transferID == "":
		return errors.New("a submission needs the provider's transfer ID")
	case t.to == StatusFailed && t.reason == "":
		return errors.New("a release needs a reason")
	}
	return nil
}

// Transitioned is what a transition reports about the withdrawal it moved: enough to post its
// movement, and the status it actually came from (one of the transition's from statuses).
type Transitioned struct {
	Account ids.AccountID
	Amount  money.Amount
	From    Status
}

// TransferIDMismatchError: a transition reported a provider transfer ID other than the one stored
// for this withdrawal. The provider has one transfer per withdrawal (our idempotency key), so this
// should never happen: the provider contradicts itself, or an update was matched to the wrong
// withdrawal. Never a retry and never resolved by picking one: nothing is changed, and the error is
// returned for the caller to expose (log as an error, answer with a conflict) and someone to
// investigate.
type TransferIDMismatchError struct {
	Withdrawal ids.WithdrawalID
	Stored     TransferID
	Reported   TransferID
}

func (e *TransferIDMismatchError) Error() string {
	return fmt.Sprintf("withdrawal %v has provider transfer %s, but a transition reported transfer %s",
		e.Withdrawal, e.Stored, e.Reported)
}

// One fixed statement for every transition. $4 and $5 are NULL unless the transition sets them:
// only failed has a failure reason (and the table's CHECK ties the two together); the provider
// transfer ID comes with a submission, or with a settlement that reports it. A reported ID must
// match the stored one, if any: the last condition makes a mismatching transition change nothing
// (TransitionStatus then finds out why). Postgres 18's RETURNING old.status gives the status
// before this update, which is not knowable in advance when there are two from statuses.
const transitionSQL = `
UPDATE withdrawals
SET status = $3,
    failure_reason = $4,
    provider_transfer_id = COALESCE(provider_transfer_id, $5),
    updated_at = now()
WHERE id = $1 AND status = ANY($2)
  AND ($5::text IS NULL OR provider_transfer_id IS NULL OR provider_transfer_id = $5)
RETURNING old.status AS from_status, account_id, amount_minor
`

const storedTransferIDSQL = `SELECT provider_transfer_id FROM withdrawals WHERE id = $1`

const historySQL = `
INSERT INTO withdrawal_status_history (withdrawal_id, from_status, to_status, reason)
VALUES ($1, $2, $3, $4)
`

// TransitionStatus moves a withdrawal to t's to status if, and only if, it is currently in one of
// t's from statuses, and records the history row in the same transaction. false when no row
// changed: the transition already happened, the withdrawal is in another state (e.g. already
// final), or it doesn't exist. As for deposits, that ambiguity is what makes repeating a transition
// safe, and of two concurrent callers exactly one sees its row change (Postgres re-checks the
// WHERE after the row lock).
//
// Returns a *TransferIDMismatchError, with nothing changed, if t reports a provider transfer ID
// other than the stored one, whatever the withdrawal's status (a repeated update for a final
// withdrawal with another ID is the same contradiction). The read that tells this apart from an
// ordinary skip only classifies: the UPDATE has already declined to act.
func TransitionStatus(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	t Transition,
) (Transitioned, bool, error) {
	if err := t.check(); err != nil {
		return Transitioned{}, false, err
	}
	// pgx encodes a []string as a text[] for ANY($2), but not a slice of the named type Status, so
	// convert. Optional values go in as nil pointers, written as NULL.
	from := make([]string, len(t.from))
	for i, s := range t.from {
		from[i] = string(s)
	}
	reason, transferID := nullable(t.reason), nullable(string(t.transferID))

	var moved Transitioned
	var fromStatus string
	err := tx.QueryRow(ctx, transitionSQL, id, from, t.to, reason, transferID).
		Scan(&fromStatus, &moved.Account, &moved.Amount)
	if errors.Is(err, pgx.ErrNoRows) {
		if t.transferID != "" {
			if err := expectSameTransferID(ctx, tx, id, t.transferID); err != nil {
				return Transitioned{}, false, err
			}
		}
		return Transitioned{}, false, nil
	}
	if err != nil {
		return Transitioned{}, false, fmt.Errorf("withdrawal %v -> %s: %w", id, t.to, err)
	}
	if moved.From, err = ParseStatus(fromStatus); err != nil {
		return Transitioned{}, false, err
	}
	if _, err := tx.Exec(ctx, historySQL, id, moved.From, t.to, reason); err != nil {
		return Transitioned{}, false, fmt.Errorf("withdrawal %v -> %s history: %w", id, t.to, err)
	}
	return moved, true, nil
}

// nullable is s, or nil (NULL) for "".
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func expectSameTransferID(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	reported TransferID,
) error {
	var stored *TransferID
	err := tx.QueryRow(ctx, storedTransferIDSQL, id).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no such withdrawal: an ordinary skip
	}
	if err != nil {
		return fmt.Errorf("withdrawal %v transfer ID: %w", id, err)
	}
	if stored != nil && *stored != reported {
		return &TransferIDMismatchError{Withdrawal: id, Stored: *stored, Reported: reported}
	}
	return nil
}

// StatusChange is one row of a withdrawal's status history. From is nil for the first row, Reason
// nil unless the change recorded one (as in Withdrawal, nullable columns are pointers).
type StatusChange struct {
	From   *Status
	To     Status
	Reason *string
	At     time.Time
}

const historyOfSQL = `
SELECT from_status, to_status, reason, at
FROM withdrawal_status_history
WHERE withdrawal_id = $1
ORDER BY id
`

// StatusHistory reads a withdrawal's status history, oldest change first.
func StatusHistory(ctx context.Context, q db.Querier, id ids.WithdrawalID) ([]StatusChange, error) {
	rows, err := q.Query(ctx, historyOfSQL, id)
	if err != nil {
		return nil, fmt.Errorf("withdrawal %v history: %w", id, err)
	}
	// defer rows.Close() releases the connection however the function returns; rows.Err() after
	// the loop reports an error that ended it early (Next returns false for both "done" and
	// "failed").
	defer rows.Close()
	var history []StatusChange
	for rows.Next() {
		var change StatusChange
		var from *string
		var to string
		if err := rows.Scan(&from, &to, &change.Reason, &change.At); err != nil {
			return nil, err
		}
		if from != nil {
			status, err := ParseStatus(*from)
			if err != nil {
				return nil, err
			}
			change.From = &status
		}
		if change.To, err = ParseStatus(to); err != nil {
			return nil, err
		}
		history = append(history, change)
	}
	return history, rows.Err()
}
