package ledger

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsInvariantViolation reports whether err means our data or code is wrong, as opposed to a
// dependency failing: the two guards disagree (LedgerInconsistencyError), a movement reference is
// reused for something else (MovementConflictError), a movement would overdraw a bucket
// (OverdraftError), or any other CHECK constraint rejected a change (a check_violation anywhere in
// the chain). Retrying can't fix them, so the consumers that post (the deposit-worker, the
// submitter) terminate the message with an alert instead of redelivering it up to max_deliver.
// Node: node/src/ledger/invariants.ts.
//
// errors.AsType looks through the whole chain of wrapped errors (fmt.Errorf's %w, Unwrap), so a
// violation wrapped with context by a caller is still found.
func IsInvariantViolation(err error) bool {
	if _, ok := errors.AsType[*LedgerInconsistencyError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*MovementConflictError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*OverdraftError](err); ok {
		return true
	}
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == checkViolation
}
