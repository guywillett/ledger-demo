import type { Queryable, Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';

// The deposit lifecycle, shared by the modules that own its transitions:
//   created -> received -> under_review -> approved -> credited
//                                       \-> rejected -> returned
// ingestion sets `created`; posting (ledger/) owns created -> received and the moves out of
// approved / rejected; compliance (compliance/) owns received -> under_review -> decision.
// Status names are part of the event contract (deposit.<status>).

export const DEPOSIT_STATUSES = [
  'created',
  'received',
  'under_review',
  'approved',
  'rejected',
  'credited',
  'returned',
  // Suspense: the deposit matches none of our accounts. Outside the lifecycle above: ingestion
  // creates it directly, no transition leads into it, and none leads out of it in this slice. A
  // terminal state until a resolution flow (match to an account, or return) exists.
  'unmatched',
] as const;
export type DepositStatus = (typeof DEPOSIT_STATUSES)[number];
// The statuses of a deposit matched to an account: everything that books or reviews deposits
// works with these, and never has to handle suspense.
export type BookedStatus = Exclude<DepositStatus, 'unmatched'>;

export function parseStatus(value: string): DepositStatus {
  const status = DEPOSIT_STATUSES.find((s) => s === value);
  if (status === undefined) {
    throw new Error(`unknown deposit status in database: ${value}`);
  }
  return status;
}

// 'applied' if this call made the transition; 'skipped' if it had already happened or the
// deposit is in another state.
export type TransitionResult = 'applied' | 'skipped';

// The legal transitions, grouped by the module that owns them. transitionStatus accepts only
// these, so an illegal pair (e.g. credited -> created) is a compile error: the lifecycle above
// is enforced by tsc, not just documented. Each module's own functions take its own group.
export type PostingTransition =
  | { readonly from: 'created'; readonly to: 'received' }
  | { readonly from: 'approved'; readonly to: 'credited' }
  | { readonly from: 'rejected'; readonly to: 'returned' };

export type ComplianceTransition =
  | { readonly from: 'received'; readonly to: 'under_review' }
  | { readonly from: 'under_review'; readonly to: 'approved' | 'rejected' };

export type Transition = PostingTransition | ComplianceTransition;

// What a transition needs to know about the deposit it moved, e.g. to post its movement.
export type TransitionedDeposit = {
  readonly accountId: Uuid<'acct'>;
  readonly amountMinor: bigint;
};

const TRANSITION_SQL = `
UPDATE deposits
SET status = $3, updated_at = now()
WHERE id = $1 AND status = $2
RETURNING account_id, amount_minor
`;

const HISTORY_SQL = `
INSERT INTO deposit_status_history (deposit_id, from_status, to_status, reason)
VALUES ($1, $2, $3, $4)
`;

// Moves a deposit from `from` to `to` if, and only if, it is currently in `from`, and records
// the history row in the same transaction. Returns undefined when no row changed: the
// transition already happened, or the deposit is in another state (or doesn't exist). That
// ambiguity is deliberate: it is what makes repeating a transition safe (no check-then-act).
//
// Postgres re-evaluates the WHERE after waiting for a concurrent transaction's row lock, so of
// two concurrent callers exactly one sees its row change.
export async function transitionStatus(
  tx: Tx,
  t: { readonly depositId: Uuid<'dep'>; readonly reason?: string } & Transition,
): Promise<TransitionedDeposit | undefined> {
  const result = await tx.query<{ account_id: string; amount_minor: bigint }>(TRANSITION_SQL, [
    t.depositId,
    t.from,
    t.to,
  ]);
  const row = result.rows[0];
  if (row === undefined) return undefined;
  await tx.query(HISTORY_SQL, [t.depositId, t.from, t.to, t.reason ?? null]);
  return { accountId: row.account_id as Uuid<'acct'>, amountMinor: row.amount_minor };
}

// One row of a deposit's status history.
export type StatusChange = {
  readonly from: DepositStatus | null;
  readonly to: DepositStatus;
  readonly reason: string | null;
  readonly at: Date;
};

const HISTORY_OF_SQL = `
SELECT deposit_id, from_status, to_status, reason, at
FROM deposit_status_history
WHERE deposit_id = ANY($1)
ORDER BY deposit_id, id
`;

// The status history of several deposits in one query (not one query per deposit), oldest
// change first. Deposits without history rows are absent from the map.
export async function statusHistories(
  db: Queryable,
  depositIds: readonly Uuid<'dep'>[],
): Promise<Map<Uuid<'dep'>, StatusChange[]>> {
  const histories = new Map<Uuid<'dep'>, StatusChange[]>();
  if (depositIds.length === 0) return histories;
  const result = await db.query<{
    deposit_id: string;
    from_status: string | null;
    to_status: string;
    reason: string | null;
    at: Date;
  }>(HISTORY_OF_SQL, [depositIds]);
  for (const row of result.rows) {
    const id = row.deposit_id as Uuid<'dep'>;
    const change: StatusChange = {
      from: row.from_status === null ? null : parseStatus(row.from_status),
      to: parseStatus(row.to_status),
      reason: row.reason,
      at: row.at,
    };
    const list = histories.get(id);
    if (list === undefined) histories.set(id, [change]);
    else list.push(change);
  }
  return histories;
}
