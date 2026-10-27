import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import { checkBalances, type AccountDifference } from './balances.ts';
import { checkDeliveries, type DeliveryStats } from './deliveries.ts';
import { checkOutbox, type OutboxStats } from './outbox.ts';
import { checkDeposits, type DepositMismatch, type StuckDeposit } from './deposits.ts';
import type { CheckScope } from './scope.ts';
import { checkSuspense, type DepositConflictRecord, type SuspenseReport } from './suspense.ts';
import {
  checkWithdrawals,
  type StuckWithdrawal,
  type TransferUpdateProblem,
  type WithdrawalMismatch,
} from './withdrawals.ts';

// The report's types, importable from here as before: each is defined with its check.
export type { AccountDifference, Buckets } from './balances.ts';
export type { DeliveryStats } from './deliveries.ts';
export type { OutboxStats } from './outbox.ts';
export type { CreatedEventState, DepositMismatch, StuckDeposit } from './deposits.ts';
export type { DepositConflictRecord, SuspenseReport } from './suspense.ts';
export type { StuckWithdrawal, TransferUpdateProblem, WithdrawalMismatch } from './withdrawals.ts';

// Reconciliation: checks that the ledger's three views of the money agree.
//   balances  (account_balances)  the current figures, a cache only move() changes
//   movements (balance_movements) the append-only journal; replaying it must give the balances
//   statuses  (deposits, withdrawals) the business state: what each deposit's and each
//             withdrawal's status says should be booked
// Plus what all three can agree on and still be wrong: deposits and withdrawals that stopped
// part-way, and events stuck in the outbox.
//
// Read-only. Run it in one REPEATABLE READ transaction (scripts/reconcile.ts does) so every
// query sees the same snapshot while the worker carries on: each worker step commits its status
// change, movement and balance update together, so any snapshot is consistent.
//
// Scale: fine for the demo, not for millions of deposits as written.
// - Memory: DEPOSIT_STEPS_SQL returns a row per deposit, WITHDRAWAL_STEPS_SQL a row per
//   withdrawal, and BALANCES_SQL a row per account; pg buffers each whole result in memory
//   before query() resolves (see DEPOSIT_STEPS_SQL in deposits.ts).
// - The database: every query scans whole tables, and one long REPEATABLE READ snapshot stops
//   VACUUM removing dead rows (e.g. the balance rows every movement rewrites) for as long as it
//   runs. At scale: run against a read replica, batch by account range (one snapshot per batch is
//   still consistent per account), or reconcile incrementally from a checkpoint (only movements
//   and deposits changed since the last run).
//
// One check per concern, each in its own file (balances, deposits, withdrawals, suspense,
// outbox, deliveries), each owning its SQL, row types and mapping. reconcile() runs them in turn
// on the one `db` it is given, sequentially: they share the caller's snapshot, a pg client runs
// one query at a time anyway, and a failure names its query.

export type ReconcileOptions = {
  // Only these accounts (deposits, withdrawals, movements, balances, outbox events); all accounts
  // if omitted. The delivery statistics are always for the whole database (deliveries aren't
  // linked to accounts).
  readonly accountIds?: readonly Uuid<'acct'>[];
  // A deposit or withdrawal in a non-final status, or an event unpublished, for longer than this
  // is reported as stuck.
  readonly stuckAfterMs: number;
};

export type ReconcileReport = {
  readonly accountsChecked: number;
  readonly depositsChecked: number;
  readonly withdrawalsChecked: number;
  readonly differences: readonly AccountDifference[];
  readonly depositMismatches: readonly DepositMismatch[];
  readonly withdrawalMismatches: readonly WithdrawalMismatch[];
  readonly stuckDeposits: readonly StuckDeposit[];
  readonly stuckWithdrawals: readonly StuckWithdrawal[];
  // Conflicting and unmatched transfer updates, oldest first. With an account filter, only that
  // account's conflicts: an unmatched update belongs to no account.
  readonly transferUpdateProblems: readonly TransferUpdateProblem[];
  // null in an account-scoped run (unmatched deposits belong to no account).
  readonly suspense: SuspenseReport | null;
  // Oldest first; with an account filter, those for that account's deposits.
  readonly depositConflicts: readonly DepositConflictRecord[];
  readonly outbox: OutboxStats;
  readonly deliveries: DeliveryStats;
  // The books agree: no account differences and no deposit or withdrawal mismatches. Stuck
  // deposits and withdrawals, and outbox problems, don't make the books wrong; they are
  // reported separately.
  readonly consistent: boolean;
};

export async function reconcile(
  db: Queryable,
  options: ReconcileOptions,
): Promise<ReconcileReport> {
  const scope: CheckScope = {
    accounts: options.accountIds ?? null,
    stuckAfterMs: options.stuckAfterMs,
  };

  const balances = await checkBalances(db, scope);
  const deposits = await checkDeposits(db, scope);
  const withdrawals = await checkWithdrawals(db, scope);
  const suspense = await checkSuspense(db, scope);
  const outbox = await checkOutbox(db, scope);
  const deliveries = await checkDeliveries(db, scope);

  return {
    accountsChecked: balances.accountsChecked,
    depositsChecked: deposits.depositsChecked,
    withdrawalsChecked: withdrawals.withdrawalsChecked,
    differences: balances.differences,
    depositMismatches: deposits.depositMismatches,
    withdrawalMismatches: withdrawals.withdrawalMismatches,
    stuckDeposits: deposits.stuckDeposits,
    stuckWithdrawals: withdrawals.stuckWithdrawals,
    transferUpdateProblems: withdrawals.transferUpdateProblems,
    suspense: suspense.suspense,
    depositConflicts: suspense.depositConflicts,
    outbox,
    deliveries,
    consistent:
      balances.differences.length === 0 &&
      deposits.depositMismatches.length === 0 &&
      withdrawals.withdrawalMismatches.length === 0,
  };
}
