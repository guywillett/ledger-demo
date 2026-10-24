import type { Queryable } from '../shared/db.ts';
import { SUBJECTS } from '../shared/events.ts';
import type { Uuid } from '../shared/ids.ts';
import { parseStatus, type PaymentStatus } from '../payments/status.ts';
import type { CheckScope } from './scope.ts';
import { sameSteps } from './steps.ts';

// The payment checks: each payment's movements against its status (and movements for no
// payment), and payments stuck in a non-final status.

// A payment whose movements don't match its status (or a movement for no payment).
export type PaymentMismatch = {
  readonly paymentId: Uuid<'pay'>;
  readonly accountId: Uuid<'acct'>;
  // null: movements reference a payment that doesn't exist.
  readonly status: PaymentStatus | null;
  readonly steps: readonly string[];
  readonly expectedSteps: readonly string[];
  // Some of its movements were posted to another account than the payment's.
  readonly otherAccount: boolean;
  // Some of its movements are for another amount than the payment's. The account sums can miss
  // this: two payments off by +10 and -10 on the same account still add up.
  readonly wrongAmount: boolean;
};

// For a payment stuck in `created`, where its payment.created event is, which says which side
// stalled: 'unpublished', the relay (the event is still in the outbox); 'published', the worker
// (the event reached the stream but the payment wasn't booked); 'missing', a bug (the payment
// was stored without its event, which the shared transaction should make impossible).
export type CreatedEventState = 'unpublished' | 'published' | 'missing';

export type StuckPayment = {
  readonly paymentId: Uuid<'pay'>;
  readonly accountId: Uuid<'acct'>;
  readonly status: PaymentStatus;
  readonly since: Date;
  readonly ageSeconds: number;
  // Only for status `created`; null otherwise.
  readonly event: CreatedEventState | null;
};

const NON_FINAL: readonly PaymentStatus[] = [
  'created',
  'received',
  'under_review',
  'approved',
  'rejected',
];

// The movements a payment in each status must have, and no others. Each list is in alphabetical
// order, not lifecycle order: it is compared with array_agg(... ORDER BY reference_step).
// (For payments the two orders happen to coincide.)
function expectedSteps(status: PaymentStatus): readonly string[] {
  switch (status) {
    // Unmatched (suspense) books nothing: a movement for one is a mismatch.
    case 'unmatched':
    case 'created':
      return [];
    case 'received':
    case 'under_review':
    case 'approved':
    case 'rejected':
      return ['book'];
    case 'credited':
      return ['book', 'credit'];
    case 'returned':
      return ['book', 'return'];
  }
}

// Every payment with the steps of its movements, and whether any was posted elsewhere or for
// another amount.
//
// Scale: this is the query that grows with the data. It returns one row per payment (all of
// them, for a whole-database run), and node-postgres reads the entire result into memory before
// query() resolves; the loop in reconcile() then keeps only the few that mismatch. Millions of
// payments means millions of row objects (and their steps arrays) on the heap at once.
// Two fixes:
// - Compare in SQL: compute the expected steps with a CASE on status, return only the
//   mismatching payments, and count(*) separately for paymentsChecked. Memory then grows with
//   the problems, not the data. The cost: the status-to-steps rule (expectedSteps) would live in
//   SQL, away from the TypeScript switch that the compiler checks for exhaustiveness.
// - Stream: read the rows in batches through a cursor (pg-cursor or pg-query-stream) and keep
//   the comparison here. Memory is bounded by the batch size; the database work is unchanged.
const PAYMENT_STEPS_SQL = `
SELECT p.id, p.account_id, p.status,
       COALESCE(array_agg(m.reference_step ORDER BY m.reference_step)
                  FILTER (WHERE m.id IS NOT NULL), '{}') AS steps,
       COALESCE(bool_or(m.account_id <> p.account_id), false) AS other_account,
       COALESCE(bool_or(m.amount_minor <> p.amount_minor), false) AS wrong_amount
FROM incoming_payments p
LEFT JOIN balance_movements m ON m.reference_type = 'payment' AND m.reference_id = p.id
WHERE $1::uuid[] IS NULL OR p.account_id = ANY($1)
GROUP BY p.id, p.account_id, p.status
`;

// Movements that reference a payment that doesn't exist.
const ORPHAN_MOVEMENTS_SQL = `
SELECT m.reference_id, m.account_id, array_agg(m.reference_step ORDER BY m.reference_step) AS steps
FROM balance_movements m
LEFT JOIN incoming_payments p ON p.id = m.reference_id
WHERE m.reference_type = 'payment' AND p.id IS NULL
  AND ($1::uuid[] IS NULL OR m.account_id = ANY($1))
GROUP BY m.reference_id, m.account_id
`;

// Payments in a non-final status whose latest status change is older than the threshold. For
// those still `created`, the state of their payment.created event in the outbox (at most one:
// msg_id is unique per payment and status).
const STUCK_SQL = `
SELECT p.id, p.account_id, p.status, h.at,
       floor(extract(epoch FROM now() - h.at))::int AS age_seconds,
       CASE WHEN p.status = 'created' THEN
         COALESCE((SELECT CASE WHEN o.published_at IS NULL THEN 'unpublished' ELSE 'published' END
                   FROM outbox o
                   WHERE o.entity_type = 'payment' AND o.entity_id = p.id
                     AND o.subject = $4), 'missing')
       END AS event
FROM incoming_payments p
JOIN LATERAL (
  SELECT at FROM payment_status_history WHERE payment_id = p.id ORDER BY id DESC LIMIT 1
) h ON true
WHERE p.status = ANY($2::text[])
  AND h.at <= now() - $3 * interval '1 millisecond'
  AND ($1::uuid[] IS NULL OR p.account_id = ANY($1))
ORDER BY h.at
`;

export async function checkPayments(
  db: Queryable,
  scope: CheckScope,
): Promise<{
  readonly mismatches: PaymentMismatch[];
  readonly paymentsChecked: number;
  readonly stuck: StuckPayment[];
}> {
  // Scale: steps.rows holds every payment at once (see PAYMENT_STEPS_SQL); only mismatches are
  // kept from it.
  const steps = await db.query<{
    id: string;
    account_id: string;
    status: string;
    steps: string[];
    other_account: boolean;
    wrong_amount: boolean;
  }>(PAYMENT_STEPS_SQL, [scope.accounts]);
  const mismatches: PaymentMismatch[] = [];
  for (const row of steps.rows) {
    const status = parseStatus(row.status);
    const expected = expectedSteps(status);
    if (row.other_account || row.wrong_amount || !sameSteps(row.steps, expected)) {
      mismatches.push({
        paymentId: row.id as Uuid<'pay'>,
        accountId: row.account_id as Uuid<'acct'>,
        status,
        steps: row.steps,
        expectedSteps: expected,
        otherAccount: row.other_account,
        wrongAmount: row.wrong_amount,
      });
    }
  }
  const orphans = await db.query<{ reference_id: string; account_id: string; steps: string[] }>(
    ORPHAN_MOVEMENTS_SQL,
    [scope.accounts],
  );
  for (const row of orphans.rows) {
    mismatches.push({
      paymentId: row.reference_id as Uuid<'pay'>,
      accountId: row.account_id as Uuid<'acct'>,
      status: null,
      steps: row.steps,
      expectedSteps: [],
      otherAccount: false,
      wrongAmount: false,
    });
  }

  const stuckRows = await db.query<{
    id: string;
    account_id: string;
    status: string;
    at: Date;
    age_seconds: number;
    event: CreatedEventState | null;
  }>(STUCK_SQL, [scope.accounts, NON_FINAL, scope.stuckAfterMs, SUBJECTS.paymentCreated]);
  const stuck: StuckPayment[] = stuckRows.rows.map((row) => ({
    paymentId: row.id as Uuid<'pay'>,
    accountId: row.account_id as Uuid<'acct'>,
    status: parseStatus(row.status),
    since: row.at,
    ageSeconds: row.age_seconds,
    event: row.event,
  }));

  return { mismatches, paymentsChecked: steps.rows.length, stuck };
}
