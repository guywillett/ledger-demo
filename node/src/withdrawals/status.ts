import type { Queryable, Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';

// The withdrawal lifecycle, shared by the modules that own its transitions:
//   held -> submitted -> executed
//     |         \-----> failed
//     |---------------> executed   (the provider's update arrived before our submit response)
//     \---------------> failed     (refused at submission, or the provider's update came first)
// Creating a withdrawal sets `held` (withdrawals.ts); the submitter owns held -> submitted; the
// settlement postings (ledger/withdrawals.ts) own the moves to executed and failed, which are
// final. Status names are part of the event contract (withdrawal.<status>).
//
// This mirrors payments/status.ts rather than sharing a generic transition function with it:
// one parameterised by table and column names would have to build its SQL from strings. Each
// entity keeps its own fixed statements instead.

export const WITHDRAWAL_STATUSES = ['held', 'submitted', 'executed', 'failed'] as const;
export type WithdrawalStatus = (typeof WITHDRAWAL_STATUSES)[number];

export function parseWithdrawalStatus(value: string): WithdrawalStatus {
  const status = WITHDRAWAL_STATUSES.find((s) => s === value);
  if (status === undefined) {
    throw new Error(`unknown withdrawal status in database: ${value}`);
  }
  return status;
}

// Funds reserved, outcome not known yet: the statuses a settlement can start from. The one
// definition (reconciliation's held and stuck checks use it too).
export const UNSETTLED_WITHDRAWAL_STATUSES = ['held', 'submitted'] as const;
type Unsettled = (typeof UNSETTLED_WITHDRAWAL_STATUSES)[number];

// The legal transitions, grouped by the module that owns them, so an illegal one (e.g.
// executed -> held) is a compile error. `from` lists every status the transition may start from:
// a settlement applies whether or not the submitter has recorded its submission yet (slice 2
// decision 27).
export type SubmissionTransition = {
  readonly from: readonly 'held'[];
  readonly to: 'submitted';
  readonly providerTransferId: string;
};

// A settlement may carry the provider's transfer ID: its update may arrive before the submitter
// has recorded the submission (decision 27), and the submission then skips.
export type SettlementTransition =
  | {
      readonly from: readonly Unsettled[];
      readonly to: 'executed';
      readonly providerTransferId?: string;
    }
  | {
      readonly from: readonly Unsettled[];
      readonly to: 'failed';
      readonly reason: string;
      readonly providerTransferId?: string;
    };

export type WithdrawalTransition = SubmissionTransition | SettlementTransition;

// What a transition needs to know about the withdrawal it moved: enough to post its movement,
// and the status it actually came from (one of `from`).
export type TransitionedWithdrawal = {
  readonly accountId: Uuid<'acct'>;
  readonly amountMinor: bigint;
  readonly from: WithdrawalStatus;
};

// A transition reported a provider transfer ID other than the one stored for this withdrawal.
// The provider has one transfer per withdrawal (our idempotency key), so this should never
// happen: the provider contradicts itself, or an update was matched to the wrong withdrawal.
// Never a retry and never resolved by picking one: nothing is changed, and the error is thrown
// for the caller to expose (log as an error, answer with a conflict) and someone to investigate.
export class TransferIdMismatchError extends Error {
  readonly withdrawalId: Uuid<'wd'>;
  readonly storedTransferId: string;
  readonly reportedTransferId: string;

  constructor(withdrawalId: Uuid<'wd'>, stored: string, reported: string) {
    super(
      `withdrawal ${withdrawalId} has provider transfer ${stored}, but a transition reported ` +
        `transfer ${reported}`,
    );
    this.name = 'TransferIdMismatchError';
    this.withdrawalId = withdrawalId;
    this.storedTransferId = stored;
    this.reportedTransferId = reported;
  }
}

// One fixed statement for every transition. $4 and $5 are NULL unless the transition sets them:
// only `failed` has a failure reason (and the table's CHECK ties the two together); the provider
// transfer ID comes with a submission, or with a settlement that reports it. A reported ID must
// match the stored one, if any: the last condition makes a mismatching transition change
// nothing (the caller then finds out why, below). Postgres 18's RETURNING old.status gives the
// status before this update, which is not knowable in advance when `from` has two members.
const TRANSITION_SQL = `
UPDATE withdrawals
SET status = $3,
    failure_reason = $4,
    provider_transfer_id = COALESCE(provider_transfer_id, $5),
    updated_at = now()
WHERE id = $1 AND status = ANY($2)
  AND ($5::text IS NULL OR provider_transfer_id IS NULL OR provider_transfer_id = $5)
RETURNING old.status AS from_status, account_id, amount_minor
`;

const STORED_TRANSFER_ID_SQL = 'SELECT provider_transfer_id FROM withdrawals WHERE id = $1';

const HISTORY_SQL = `
INSERT INTO withdrawal_status_history (withdrawal_id, from_status, to_status, reason)
VALUES ($1, $2, $3, $4)
`;

// Moves a withdrawal to `to` if, and only if, it is currently in one of `from`, and records the
// history row in the same transaction. undefined when no row changed: the transition already
// happened, the withdrawal is in another state (e.g. already final), or it doesn't exist. As for
// payments, that ambiguity is what makes repeating a transition safe, and of two concurrent
// callers exactly one sees its row change (Postgres re-checks the WHERE after the row lock).
//
// Throws TransferIdMismatchError, with nothing changed, if the transition reports a provider
// transfer ID other than the stored one, whatever the withdrawal's status (a repeated update for
// a final withdrawal with another ID is the same contradiction). The read that tells this apart
// from an ordinary skip only classifies: the UPDATE has already declined to act.
export async function transitionWithdrawalStatus(
  tx: Tx,
  t: { readonly withdrawalId: Uuid<'wd'> } & WithdrawalTransition,
): Promise<TransitionedWithdrawal | undefined> {
  const reason = t.to === 'failed' ? t.reason : null;
  const providerTransferId = t.providerTransferId ?? null;
  const result = await tx.query<{ from_status: string; account_id: string; amount_minor: bigint }>(
    TRANSITION_SQL,
    [t.withdrawalId, t.from, t.to, reason, providerTransferId],
  );
  const row = result.rows[0];
  if (row === undefined) {
    if (providerTransferId !== null) {
      await expectSameTransferId(tx, t.withdrawalId, providerTransferId);
    }
    return undefined;
  }
  const from = parseWithdrawalStatus(row.from_status);
  await tx.query(HISTORY_SQL, [t.withdrawalId, from, t.to, reason]);
  return { accountId: row.account_id as Uuid<'acct'>, amountMinor: row.amount_minor, from };
}

async function expectSameTransferId(
  tx: Tx,
  withdrawalId: Uuid<'wd'>,
  reported: string,
): Promise<void> {
  const result = await tx.query<{ provider_transfer_id: string | null }>(STORED_TRANSFER_ID_SQL, [
    withdrawalId,
  ]);
  const stored = result.rows[0]?.provider_transfer_id ?? null;
  if (stored !== null && stored !== reported) {
    throw new TransferIdMismatchError(withdrawalId, stored, reported);
  }
}

// One row of a withdrawal's status history.
export type WithdrawalStatusChange = {
  readonly from: WithdrawalStatus | null;
  readonly to: WithdrawalStatus;
  readonly reason: string | null;
  readonly at: Date;
};

const HISTORY_OF_SQL = `
SELECT from_status, to_status, reason, at
FROM withdrawal_status_history
WHERE withdrawal_id = $1
ORDER BY id
`;

// A withdrawal's status history, oldest change first.
export async function withdrawalStatusHistory(
  db: Queryable,
  withdrawalId: Uuid<'wd'>,
): Promise<WithdrawalStatusChange[]> {
  const result = await db.query<{
    from_status: string | null;
    to_status: string;
    reason: string | null;
    at: Date;
  }>(HISTORY_OF_SQL, [withdrawalId]);
  return result.rows.map((row) => ({
    from: row.from_status === null ? null : parseWithdrawalStatus(row.from_status),
    to: parseWithdrawalStatus(row.to_status),
    reason: row.reason,
    at: row.at,
  }));
}
