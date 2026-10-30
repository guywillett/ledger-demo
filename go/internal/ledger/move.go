// Package ledger is the only code that changes balances. Every change is a movement between two
// buckets of one account, recorded in balance_movements (append-only) and applied to
// account_balances in the caller's transaction. Node: node/src/ledger/.
//
// The ledger layer (decision 57): it uses the entities (deposits; withdrawals from step 3b) and is
// used by orchestration (ingestion, compliance, requests) and the processes. Balances change only
// through its posting functions (postings.go), which pair each entity transition with its
// movement.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

// Bucket is one of an account's balances, or external (outside our books: money arriving or
// leaving).
type Bucket string

const (
	BucketExternal  Bucket = "external"
	BucketPendingIn Bucket = "pending_in"
	BucketAvailable Bucket = "available"
	BucketHeld      Bucket = "held"
)

// DepositStep is the step within a deposit that causes a movement.
type DepositStep string

const (
	StepBook   DepositStep = "book"
	StepCredit DepositStep = "credit"
	StepReturn DepositStep = "return"
)

// Reference is the business object causing a movement and the step within it: (type, ID, step),
// applied at most once. Its fields are unexported, so a Reference comes only from a constructor
// (DepositReference, WithdrawalReference), each taking its entity's own ID type and
// step type: a deposit's ID with a withdrawal's step, or a withdrawal's UUID as a deposit's, can't
// be written. Node gets the same pairing from a discriminated union (move.ts MovementReference),
// and the database from its paired CHECK on (reference_type, reference_step).
//
// The zero Reference (no constructor) is rejected by Move.
type Reference struct {
	entity string    // "deposit" or "withdrawal": reference_type
	id     uuid.UUID // reference_id
	typeID string    // the same ID as a TypeID, for messages
	step   string    // reference_step
}

// DepositReference is a deposit's step. (WithdrawalReference is in withdrawals.go.)
func DepositReference(id ids.DepositID, step DepositStep) Reference {
	return Reference{entity: "deposit", id: id.UUID(), typeID: id.TypeID(), step: string(step)}
}

// String is e.g. "deposit dep_01m5…/book".
func (r Reference) String() string {
	return r.entity + " " + r.typeID + "/" + r.step
}

// Movement is an amount moving between two buckets of one account, for a Reference.
type Movement struct {
	Account ids.AccountID
	From    Bucket
	To      Bucket
	Amount  money.Amount
	Ref     Reference
}

// MoveResult is what Move did: the "applied" | "duplicate" vocabulary of idempotent inserts
// (CLAUDE.md).
type MoveResult string

const (
	// MoveApplied: this call recorded the movement and changed the balances.
	MoveApplied MoveResult = "applied"
	// MoveDuplicate: a movement with this reference was already recorded (and is the same).
	MoveDuplicate MoveResult = "duplicate"
)

// The errors. Each is its own type (a struct with an Error method, which is all the error interface
// asks), so a caller tells them apart with errors.As or errors.AsType, the Go counterpart of
// `instanceof` on an Error subclass, and reads their fields. They're returned as pointers, the
// usual form for error types.

// LedgerInconsistencyError: a posting's movement already exists although the entity's state says
// the step is happening for the first time (its transition just applied, or it was just inserted).
// Two guards disagree. Returned so the transaction rolls back (the status change is undone) and the
// caller logs it; never silently accepted.
type LedgerInconsistencyError struct {
	Ref Reference
}

func (e *LedgerInconsistencyError) Error() string {
	return fmt.Sprintf(
		"%s %s: its %s movement already exists, but the %s's state says that step hasn't happened yet",
		e.Ref.entity, e.Ref.typeID, e.Ref.step, e.Ref.entity,
	)
}

// MovementConflictError: a movement with this reference already exists but differs (another
// account, amount or buckets): the reference is being reused for something else, which is a bug,
// not a retry.
type MovementConflictError struct {
	Movement Movement
}

func (e *MovementConflictError) Error() string {
	return fmt.Sprintf("movement %s already exists with different details", e.Movement.Ref)
}

// OverdraftError: the movement would take a bucket below zero, and the CHECK (… >= 0) on
// account_balances rejected it. Posting can only overdraw through a bug, which is exactly when the
// log should say what happened rather than show a raw database error.
type OverdraftError struct {
	Account ids.AccountID
	Bucket  Bucket
	Amount  money.Amount
	Ref     Reference
	err     error // the database's check_violation
}

func (e *OverdraftError) Error() string {
	return fmt.Sprintf("movement %s would overdraw %s of account %v", e.Ref, e.Bucket, e.Account)
}

// Unwrap returns the database's error, so errors.Is and errors.As see through an OverdraftError to
// the *pgconn.PgError inside: Node's `cause`, but followed automatically.
func (e *OverdraftError) Unwrap() error { return e.err }

// balanceChecks maps the balance columns' CHECK constraints to their buckets. Postgres names a
// column's CHECK constraint <table>_<column>_check.
var balanceChecks = map[string]Bucket{
	"account_balances_pending_in_check": BucketPendingIn,
	"account_balances_available_check":  BucketAvailable,
	"account_balances_held_check":       BucketHeld,
}

// checkViolation is Postgres's SQLSTATE for a CHECK constraint failing.
const checkViolation = "23514"

// asOverdraft turns a check_violation on one of the balance CHECKs into an OverdraftError, and
// returns any other error unchanged.
func asOverdraft(err error, m Movement) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pgErr.Code != checkViolation {
		return err
	}
	bucket, ok := balanceChecks[pgErr.ConstraintName]
	if !ok {
		return err
	}
	return &OverdraftError{Account: m.Account, Bucket: bucket, Amount: m.Amount, Ref: m.Ref, err: err}
}

const insertMovementSQL = `
INSERT INTO balance_movements (id, account_id, from_bucket, to_bucket, amount_minor,
                               reference_type, reference_id, reference_step)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (reference_type, reference_id, reference_step) DO NOTHING
RETURNING id
`

const existingMovementSQL = `
SELECT account_id, from_bucket, to_bucket, amount_minor
FROM balance_movements
WHERE reference_type = $1 AND reference_id = $2 AND reference_step = $3
`

// One fixed statement for every movement: the change to each bucket is computed in Go (minus for
// the source, plus for the destination, nothing for external), so the SQL never changes shape and
// no column names are built from strings. CHECK (… >= 0) on each column rejects a movement that
// would overdraw a bucket (reported as an OverdraftError).
const applyToBalancesSQL = `
UPDATE account_balances
SET pending_in = pending_in + $2,
    available  = available  + $3,
    held       = held       + $4,
    updated_at = now()
WHERE account_id = $1
`

// Move applies the movement once: MoveApplied if this call recorded it, MoveDuplicate if a movement
// with the same reference already exists (and is the same movement). A Tx, because the movement,
// the balance change and whatever caused them must commit or roll back together.
//
// Concurrency: if another transaction is inserting the same reference, ON CONFLICT waits for it;
// if it commits, this call is a duplicate, if it rolls back, this call applies.
func Move(ctx context.Context, tx db.Tx, m Movement) (MoveResult, error) {
	switch {
	case m.Ref.entity == "":
		return "", errors.New("movement without a reference")
	case m.From == m.To:
		return "", fmt.Errorf("movement %s from %s to itself", m.Ref, m.From)
	case m.Amount <= 0:
		return "", fmt.Errorf("movement %s: amount must be positive", m.Ref)
	}

	var movementID ids.MovementID
	err := tx.QueryRow(ctx, insertMovementSQL,
		ids.New[ids.Movement](), m.Account, m.From, m.To, m.Amount, m.Ref.entity, m.Ref.id, m.Ref.step,
	).Scan(&movementID)
	if errors.Is(err, pgx.ErrNoRows) { // ON CONFLICT DO NOTHING inserted nothing: a repeat
		if err := expectSameMovement(ctx, tx, m); err != nil {
			return "", err
		}
		return MoveDuplicate, nil
	}
	if err != nil {
		return "", fmt.Errorf("movement %s: %w", m.Ref, err)
	}

	tag, err := tx.Exec(ctx, applyToBalancesSQL, m.Account,
		change(BucketPendingIn, m), change(BucketAvailable, m), change(BucketHeld, m))
	if err != nil {
		return "", asOverdraft(fmt.Errorf("movement %s: %w", m.Ref, err), m)
	}
	if tag.RowsAffected() != 1 {
		return "", fmt.Errorf("movement %s: no balance row for account %v", m.Ref, m.Account)
	}
	return MoveApplied, nil
}

// MoveOnce is Move for a posting: the caller has just established, through the entity's own state
// (a conditional transition that applied, or a fresh insert), that this step is happening for the
// first time. So the movement must be new, and a duplicate means the two guards disagree.
func MoveOnce(ctx context.Context, tx db.Tx, m Movement) error {
	result, err := Move(ctx, tx, m)
	if err != nil {
		return err
	}
	if result == MoveDuplicate {
		return &LedgerInconsistencyError{Ref: m.Ref}
	}
	return nil
}

// change is the movement's effect on one bucket.
func change(bucket Bucket, m Movement) money.Amount {
	var delta money.Amount
	if m.From == bucket {
		delta -= m.Amount
	}
	if m.To == bucket {
		delta += m.Amount
	}
	return delta
}

// expectSameMovement reads the existing movement (the latest committed data, or this transaction's
// own earlier insert) and refuses a reference reused with other details.
func expectSameMovement(ctx context.Context, tx db.Tx, m Movement) error {
	var existing Movement
	err := tx.QueryRow(ctx, existingMovementSQL, m.Ref.entity, m.Ref.id, m.Ref.step).
		Scan(&existing.Account, &existing.From, &existing.To, &existing.Amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return &MovementConflictError{Movement: m}
	}
	if err != nil {
		return fmt.Errorf("movement %s: %w", m.Ref, err)
	}
	if existing.Account != m.Account || existing.From != m.From || existing.To != m.To ||
		existing.Amount != m.Amount {
		return &MovementConflictError{Movement: m}
	}
	return nil
}
