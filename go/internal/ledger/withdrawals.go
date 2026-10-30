package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// Withdrawal postings: creating a withdrawal with its funds held, then executing or releasing them
// when it settles. Each changes the withdrawal and posts its movement in the caller's transaction,
// so both happen or neither does. Node: node/src/ledger/withdrawals.ts.

// WithdrawalStep is the step within a withdrawal that causes a movement.
type WithdrawalStep string

const (
	StepHold    WithdrawalStep = "hold"
	StepExecute WithdrawalStep = "execute"
	StepRelease WithdrawalStep = "release"
)

// WithdrawalReference is a withdrawal's step.
func WithdrawalReference(id ids.WithdrawalID, step WithdrawalStep) Reference {
	return Reference{entity: "withdrawal", id: id.UUID(), typeID: id.TypeID(), step: string(step)}
}

// InsufficientFundsError: not enough available funds for the withdrawal. A business outcome (the
// client asked for more than the account holds), not a bug: deliberately not an OverdraftError,
// which the consumers treat as an invariant violation. Only CreateWithdrawal turns one into the
// other, for the available bucket, where running short is expected.
//
// It wraps that *OverdraftError (Unwrap), so the database's refusal stays in the chain, as Node's
// `cause`. Unlike `cause`, errors.As and errors.AsType follow it: errors.AsType[*OverdraftError]
// finds the OverdraftError inside an InsufficientFundsError. So test for InsufficientFundsError
// first; IsInvariantViolation does.
type InsufficientFundsError struct {
	Withdrawal ids.WithdrawalID
	Account    ids.AccountID
	Amount     money.Amount
	overdraft  *OverdraftError
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient available funds on account %v for withdrawal %v",
		e.Account, e.Withdrawal)
}

// Unwrap returns the OverdraftError on available.
func (e *InsufficientFundsError) Unwrap() error { return e.overdraft }

// The insert lives here, not in package withdrawals, because creating a withdrawal is its first
// posting (decision 57): withdrawals exports no insert, so CreateWithdrawal, which always holds the
// funds, is the only way to store one. One statement inserts the withdrawal in held and its first
// history row, or nothing when the account already has a withdrawal with this idempotency key.
// Data-modifying CTEs always run to completion, so `history` runs although nothing reads it.
const createWithdrawalSQL = `
WITH inserted AS (
  INSERT INTO withdrawals (id, account_id, idempotency_key, amount_minor, currency,
                           creditor_iban, creditor_name, remittance_info, status)
  VALUES ($1, $2, $3, $4, 'EUR', $5, $6, $7, 'held')
  ON CONFLICT (account_id, idempotency_key) DO NOTHING
  RETURNING *
),
history AS (
  INSERT INTO withdrawal_status_history (withdrawal_id, from_status, to_status)
  SELECT id, NULL, 'held' FROM inserted
)
SELECT ` + withdrawals.Columns + ` FROM inserted
`

// CreateWithdrawal inserts a withdrawal in held and reserves its funds, available -> held, in the
// caller's transaction: the only way to create one, so a withdrawal is never stored without its
// hold (held is one bucket per account, and settling a withdrawal that reserved nothing would
// execute or release other withdrawals' reserved funds).
//
// false when the account already has a withdrawal with this idempotency key: nothing is inserted
// or held; classifying the repeat is the caller's job. Concurrency: if another transaction is
// inserting the same key, ON CONFLICT waits for it; if it commits, nothing is inserted (false); if
// it rolls back, this insert goes ahead.
//
// The balance CHECK (available >= 0) is the insufficient-funds check, atomic with the reservation,
// so two concurrent withdrawals can never both spend the same funds (no check-then-act). Short
// funds return an *InsufficientFundsError, and an unknown account a
// *withdrawals.AccountNotFoundError (the insert's foreign key decides; nothing reads first).
//
// Both are returned rather than handled here because the transaction can't continue: when a
// statement fails, Postgres marks the whole transaction as aborted, and every later statement in
// it fails ("current transaction is aborted, commands ignored until end of transaction block")
// until ROLLBACK. So the caller can only return the error and let db.InTx roll back (the
// withdrawal row goes too, so the idempotency key stays free), then handle it outside (the API
// answers 422 or 404).
//
// Django is different on the surface: inside atomic(), a nested atomic() block is a SAVEPOINT, and
// an exception leaving it rolls back to the savepoint, so the outer transaction can carry on after
// catching it. Postgres has savepoints too, but nothing here uses them: a failed hold means the
// whole request failed, so rolling everything back is the behaviour we want.
func CreateWithdrawal(
	ctx context.Context,
	tx db.Tx,
	w withdrawals.NewWithdrawal,
) (withdrawals.Withdrawal, bool, error) {
	created, err := withdrawals.Scan(tx.QueryRow(ctx, createWithdrawalSQL,
		ids.New[ids.Withdrawal](), w.Account, w.IdempotencyKey, w.Amount, w.CreditorIBAN,
		w.CreditorName, w.RemittanceInfo,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return withdrawals.Withdrawal{}, false, nil
	}
	if err != nil {
		return withdrawals.Withdrawal{}, false, withdrawals.AsAccountNotFound(
			fmt.Errorf("create withdrawal on account %v: %w", w.Account, err), w.Account)
	}

	// The withdrawal was inserted in this transaction, so its hold must be new (MoveOnce).
	err = MoveOnce(ctx, tx, Movement{
		Account: created.Account,
		From:    BucketAvailable,
		To:      BucketHeld,
		Amount:  created.Amount,
		Ref:     WithdrawalReference(created.ID, StepHold),
	})
	overdraft, isOverdraft := errors.AsType[*OverdraftError](err)
	if isOverdraft && overdraft.Bucket == BucketAvailable {
		return withdrawals.Withdrawal{}, false, &InsufficientFundsError{
			Withdrawal: created.ID, Account: created.Account, Amount: created.Amount, overdraft: overdraft,
		}
	}
	if err != nil {
		return withdrawals.Withdrawal{}, false, err
	}
	return created, true, nil
}

// MarkSubmitted: held -> submitted, recording the provider's transfer ID; no movement (the funds
// stay held until the transfer settles). Skipped if the withdrawal is no longer held: the
// provider's update may already have settled it (slice 2 decision 27), or this is a repeat.
//
// Not a posting, but it lives with them, as in Node, so the withdrawal's transitions are in one
// place for the submitter.
func MarkSubmitted(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	transferID withdrawals.TransferID,
) (Result, error) {
	_, applied, err := withdrawals.TransitionStatus(ctx, tx, id, withdrawals.Submit(transferID))
	if err != nil {
		return "", err
	}
	if !applied {
		return Skipped, nil
	}
	return Applied, nil
}

// The settlements optionally record the provider's transfer ID ("" if the caller doesn't have it),
// which the provider's update carries: when it settles a withdrawal before the submission was
// recorded, MarkSubmitted then skips, and this is the only chance to store it. An ID that differs
// from the one already stored returns a *withdrawals.TransferIDMismatchError and changes nothing;
// so does MarkSubmitted with another ID than the one an earlier update stored.

// Execute: held | submitted -> executed, the money has left, held -> external. Skipped if already
// settled (executed by a repeat, or failed).
func Execute(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	transferID withdrawals.TransferID,
) (Result, error) {
	return settle(ctx, tx, id, withdrawals.Execute(transferID), BucketExternal, StepExecute)
}

// Release: held | submitted -> failed, with the reason: the funds go back, held -> available.
// Skipped if already settled.
func Release(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	reason string,
	transferID withdrawals.TransferID,
) (Result, error) {
	return settle(ctx, tx, id, withdrawals.Release(reason, transferID), BucketAvailable, StepRelease)
}

func settle(
	ctx context.Context,
	tx db.Tx,
	id ids.WithdrawalID,
	t withdrawals.Transition,
	to Bucket,
	step WithdrawalStep,
) (Result, error) {
	moved, applied, err := withdrawals.TransitionStatus(ctx, tx, id, t)
	if err != nil {
		return "", err
	}
	if !applied {
		return Skipped, nil
	}
	err = MoveOnce(ctx, tx, Movement{
		Account: moved.Account,
		From:    BucketHeld,
		To:      to,
		Amount:  moved.Amount,
		Ref:     WithdrawalReference(id, step),
	})
	if err != nil {
		return "", err
	}
	return Applied, nil
}
