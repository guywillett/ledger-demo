import type { Uuid } from '../shared/ids.ts';

// What every check needs to know about the run, built once by reconcile() from its options.
// In every check's SQL, $1 is the account filter: scope.accounts, NULL for all accounts.
export type CheckScope = {
  readonly accounts: readonly Uuid<'acct'>[] | null;
  // A deposit or withdrawal in a non-final status, or an event unpublished, for longer than this
  // is reported as stuck.
  readonly stuckAfterMs: number;
};
