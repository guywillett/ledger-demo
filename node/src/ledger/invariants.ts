import { LedgerInconsistencyError, MovementConflictError, OverdraftError } from './move.ts';

// Errors from posting that mean our data or code is wrong, as opposed to a dependency failing: the
// two guards disagree (LedgerInconsistencyError), a movement reference is reused for something else
// (MovementConflictError), a movement would overdraw a bucket (OverdraftError, which wraps the
// database's check_violation as its cause), or any other CHECK constraint rejected a change
// (Postgres error code 23514 on the error itself). Retrying can't fix them, so the consumers that
// post (the deposit-worker, the submitter) terminate the message with an alert instead of
// redelivering it up to max_deliver.
export function isInvariantViolation(err: unknown): boolean {
  if (
    err instanceof LedgerInconsistencyError ||
    err instanceof MovementConflictError ||
    err instanceof OverdraftError
  ) {
    return true;
  }
  return typeof err === 'object' && err !== null && 'code' in err && err.code === '23514';
}
