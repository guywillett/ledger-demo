import type { Queryable, Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';

// The payment lifecycle, shared by the modules that own its transitions:
//   created -> received -> under_review -> approved -> credited
//                                       \-> rejected -> returned
// ingestion sets `created`; posting (ledger/) owns created -> received and the moves out of
// approved / rejected; compliance (compliance/) owns received -> under_review -> decision.
// Status names are part of the event contract (payment.<status>).

export const PAYMENT_STATUSES = [
  'created',
  'received',
  'under_review',
  'approved',
  'rejected',
  'credited',
  'returned',
  // Suspense: the payment matches none of our accounts. Outside the lifecycle above: ingestion
  // creates it directly, no transition leads into it, and none leads out of it in this slice. A
  // terminal state until a resolution flow (match to an account, or return) exists.
  'unmatched',
] as const;
export type PaymentStatus = (typeof PAYMENT_STATUSES)[number];
// The statuses of a payment matched to an account: everything that books or reviews payments
// works with these, and never has to handle suspense.
export type BookedStatus = Exclude<PaymentStatus, 'unmatched'>;

export function parseStatus(value: string): PaymentStatus {
  const status = PAYMENT_STATUSES.find((s) => s === value);
  if (status === undefined) {
    throw new Error(`unknown payment status in database: ${value}`);
  }
  return status;
}

// 'applied' if this call made the transition; 'skipped' if it had already happened or the
// payment is in another state.
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

// What a transition needs to know about the payment it moved, e.g. to post its movement.
export type TransitionedPayment = {
  readonly accountId: Uuid<'acct'>;
  readonly amountMinor: bigint;
};

const TRANSITION_SQL = `
UPDATE incoming_payments
SET status = $3, updated_at = now()
WHERE id = $1 AND status = $2
RETURNING account_id, amount_minor
`;

const HISTORY_SQL = `
INSERT INTO payment_status_history (payment_id, from_status, to_status, reason)
VALUES ($1, $2, $3, $4)
`;

// Moves a payment from `from` to `to` if, and only if, it is currently in `from`, and records
// the history row in the same transaction. Returns undefined when no row changed: the
// transition already happened, or the payment is in another state (or doesn't exist). That
// ambiguity is deliberate: it is what makes repeating a transition safe (no check-then-act).
//
// Postgres re-evaluates the WHERE after waiting for a concurrent transaction's row lock, so of
// two concurrent callers exactly one sees its row change.
export async function transitionStatus(
  tx: Tx,
  t: { readonly paymentId: Uuid<'pay'>; readonly reason?: string } & Transition,
): Promise<TransitionedPayment | undefined> {
  const result = await tx.query<{ account_id: string; amount_minor: bigint }>(TRANSITION_SQL, [
    t.paymentId,
    t.from,
    t.to,
  ]);
  const row = result.rows[0];
  if (row === undefined) return undefined;
  await tx.query(HISTORY_SQL, [t.paymentId, t.from, t.to, t.reason ?? null]);
  return { accountId: row.account_id as Uuid<'acct'>, amountMinor: row.amount_minor };
}

// One row of a payment's status history.
export type StatusChange = {
  readonly from: PaymentStatus | null;
  readonly to: PaymentStatus;
  readonly reason: string | null;
  readonly at: Date;
};

const HISTORY_OF_SQL = `
SELECT payment_id, from_status, to_status, reason, at
FROM payment_status_history
WHERE payment_id = ANY($1)
ORDER BY payment_id, id
`;

// The status history of several payments in one query (not one query per payment), oldest
// change first. Payments without history rows are absent from the map.
export async function statusHistories(
  db: Queryable,
  paymentIds: readonly Uuid<'pay'>[],
): Promise<Map<Uuid<'pay'>, StatusChange[]>> {
  const histories = new Map<Uuid<'pay'>, StatusChange[]>();
  if (paymentIds.length === 0) return histories;
  const result = await db.query<{
    payment_id: string;
    from_status: string | null;
    to_status: string;
    reason: string | null;
    at: Date;
  }>(HISTORY_OF_SQL, [paymentIds]);
  for (const row of result.rows) {
    const id = row.payment_id as Uuid<'pay'>;
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
