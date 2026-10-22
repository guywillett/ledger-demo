import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import { parseStatus, type PaymentStatus } from '../shared/payment-status.ts';

// Reconciliation: checks that the ledger's three views of the money agree.
//   balances  (account_balances)  the current figures, a cache only move() changes
//   movements (balance_movements) the append-only journal; replaying it must give the balances
//   payments  (incoming_payments) the business state: what each status says should be booked
// Plus what all three can agree on and still be wrong: payments that stopped part-way.
//
// Read-only. Run it in one REPEATABLE READ transaction (scripts/reconcile.ts does) so every
// query sees the same snapshot while the worker carries on: each worker step commits its status
// change, movement and balance update together, so any snapshot is consistent.
//
// Scale: fine for the demo, not for millions of payments as written.
// - Memory: PAYMENT_STEPS_SQL returns a row per payment, and BALANCES_SQL a row per account;
//   pg buffers each whole result in memory before query() resolves (see PAYMENT_STEPS_SQL).
// - The database: every query scans whole tables, and one long REPEATABLE READ snapshot stops
//   VACUUM removing dead rows (e.g. the balance rows every movement rewrites) for as long as it
//   runs. At scale: run against a read replica, batch by account range (one snapshot per batch is
//   still consistent per account), or reconcile incrementally from a checkpoint (only movements
//   and payments changed since the last run).

export type ReconcileOptions = {
  // Only these accounts (payments, movements, balances); all accounts if omitted. The delivery
  // statistics are always for the whole database (deliveries aren't linked to accounts).
  readonly accountIds?: readonly Uuid<'acct'>[];
  // A payment in a non-final status for longer than this is reported as stuck.
  readonly stuckAfterMs: number;
};

export type Buckets = { readonly pendingIn: bigint; readonly available: bigint };

// An account whose balance disagrees with what its payments or its movements say.
export type AccountDifference = {
  readonly accountId: Uuid<'acct'>;
  // null: the account has no balance row. A trigger creates it with the account, but nothing
  // stops it being deleted later (and move() can't post to the account without it).
  readonly balance: Buckets | null;
  readonly fromPayments: Buckets;
  readonly fromMovements: Buckets;
};

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

export type StuckPayment = {
  readonly paymentId: Uuid<'pay'>;
  readonly accountId: Uuid<'acct'>;
  readonly status: PaymentStatus;
  readonly since: Date;
  readonly ageSeconds: number;
};

export type DeliveryStats = {
  readonly byOutcome: Readonly<Record<string, number>>;
  // Deliveries still in_progress after the stuck threshold: the trace a crash leaves (the
  // redelivery gets its own row), or a worker still working on it.
  readonly staleInProgress: number;
  // Repeat notifications from the provider with the same details (notification_count - 1).
  readonly duplicateNotifications: bigint;
};

export type ReconcileReport = {
  readonly accountsChecked: number;
  readonly paymentsChecked: number;
  readonly differences: readonly AccountDifference[];
  readonly mismatches: readonly PaymentMismatch[];
  readonly stuck: readonly StuckPayment[];
  readonly deliveries: DeliveryStats;
  // The books agree: no account differences and no payment mismatches. Stuck payments don't
  // make the books wrong; they are reported separately.
  readonly consistent: boolean;
};

const NON_FINAL: readonly PaymentStatus[] = [
  'created',
  'received',
  'under_review',
  'approved',
  'rejected',
];

// The movements a payment in each status must have, and no others.
function expectedSteps(status: PaymentStatus): readonly string[] {
  switch (status) {
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

// $1 is the account filter (NULL for all accounts) throughout.
//
// sum() of a bigint column is numeric, which pg returns as a string; ::text makes that explicit,
// and the string is parsed with BigInt (exact). COALESCE turns "no rows" into 0.
//
// Starts from accounts, not account_balances, so an account without a balance row shows up
// (with NULL balances) instead of being skipped. One row per account, all held in memory (see
// the scale note at the top).
const BALANCES_SQL = `
WITH accounts_checked AS (
  SELECT a.id AS account_id, b.pending_in, b.available
  FROM accounts a
  LEFT JOIN account_balances b ON b.account_id = a.id
  WHERE $1::uuid[] IS NULL OR a.id = ANY($1)
),
by_payments AS (
  SELECT account_id,
         COALESCE(sum(amount_minor) FILTER (WHERE status IN ('received', 'under_review', 'approved', 'rejected')), 0) AS pending_in,
         COALESCE(sum(amount_minor) FILTER (WHERE status = 'credited'), 0) AS available
  FROM incoming_payments
  WHERE account_id IN (SELECT account_id FROM accounts_checked)
  GROUP BY account_id
),
by_movements AS (
  SELECT account_id,
         COALESCE(sum(amount_minor) FILTER (WHERE to_bucket = 'pending_in'), 0)
           - COALESCE(sum(amount_minor) FILTER (WHERE from_bucket = 'pending_in'), 0) AS pending_in,
         COALESCE(sum(amount_minor) FILTER (WHERE to_bucket = 'available'), 0)
           - COALESCE(sum(amount_minor) FILTER (WHERE from_bucket = 'available'), 0) AS available
  FROM balance_movements
  WHERE account_id IN (SELECT account_id FROM accounts_checked)
  GROUP BY account_id
)
SELECT a.account_id,
       a.pending_in::text AS balance_pending_in,
       a.available::text AS balance_available,
       COALESCE(p.pending_in, 0)::text AS payments_pending_in,
       COALESCE(p.available, 0)::text AS payments_available,
       COALESCE(m.pending_in, 0)::text AS movements_pending_in,
       COALESCE(m.available, 0)::text AS movements_available
FROM accounts_checked a
LEFT JOIN by_payments p USING (account_id)
LEFT JOIN by_movements m USING (account_id)
`;

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
       COALESCE(array_agg(m.reference_step ORDER BY m.reference_step) FILTER (WHERE m.id IS NOT NULL), '{}') AS steps,
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

// Payments in a non-final status whose latest status change is older than the threshold.
const STUCK_SQL = `
SELECT p.id, p.account_id, p.status, h.at,
       floor(extract(epoch FROM now() - h.at))::int AS age_seconds
FROM incoming_payments p
JOIN LATERAL (
  SELECT at FROM payment_status_history WHERE payment_id = p.id ORDER BY id DESC LIMIT 1
) h ON true
WHERE p.status = ANY($2::text[])
  AND h.at <= now() - $3 * interval '1 millisecond'
  AND ($1::uuid[] IS NULL OR p.account_id = ANY($1))
ORDER BY h.at
`;

const DELIVERIES_BY_OUTCOME_SQL = `
SELECT outcome, count(*)::int AS n FROM event_deliveries GROUP BY outcome
`;

const STALE_IN_PROGRESS_SQL = `
SELECT count(*)::int AS n
FROM event_deliveries
WHERE outcome = 'in_progress' AND received_at <= now() - $1 * interval '1 millisecond'
`;

const DUPLICATE_NOTIFICATIONS_SQL = `
SELECT COALESCE(sum(notification_count - 1), 0)::text AS n
FROM incoming_payments
WHERE $1::uuid[] IS NULL OR account_id = ANY($1)
`;

export async function reconcile(
  db: Queryable,
  options: ReconcileOptions,
): Promise<ReconcileReport> {
  const accounts = options.accountIds ?? null;

  const balances = await db.query<{
    account_id: string;
    // null when the account has no balance row.
    balance_pending_in: string | null;
    balance_available: string | null;
    payments_pending_in: string;
    payments_available: string;
    movements_pending_in: string;
    movements_available: string;
  }>(BALANCES_SQL, [accounts]);
  const differences: AccountDifference[] = [];
  for (const row of balances.rows) {
    const check: AccountDifference = {
      accountId: row.account_id as Uuid<'acct'>,
      balance:
        row.balance_pending_in === null || row.balance_available === null
          ? null
          : buckets(row.balance_pending_in, row.balance_available),
      fromPayments: buckets(row.payments_pending_in, row.payments_available),
      fromMovements: buckets(row.movements_pending_in, row.movements_available),
    };
    if (
      check.balance === null ||
      !same(check.balance, check.fromPayments) ||
      !same(check.balance, check.fromMovements)
    ) {
      differences.push(check);
    }
  }

  // Scale: steps.rows holds every payment at once (see PAYMENT_STEPS_SQL); only mismatches are
  // kept from it.
  const steps = await db.query<{
    id: string;
    account_id: string;
    status: string;
    steps: string[];
    other_account: boolean;
    wrong_amount: boolean;
  }>(PAYMENT_STEPS_SQL, [accounts]);
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
    [accounts],
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
  }>(STUCK_SQL, [accounts, NON_FINAL, options.stuckAfterMs]);
  const stuck: StuckPayment[] = stuckRows.rows.map((row) => ({
    paymentId: row.id as Uuid<'pay'>,
    accountId: row.account_id as Uuid<'acct'>,
    status: parseStatus(row.status),
    since: row.at,
    ageSeconds: row.age_seconds,
  }));

  const byOutcome = await db.query<{ outcome: string; n: number }>(DELIVERIES_BY_OUTCOME_SQL);
  const stale = await db.query<{ n: number }>(STALE_IN_PROGRESS_SQL, [options.stuckAfterMs]);
  const duplicates = await db.query<{ n: string }>(DUPLICATE_NOTIFICATIONS_SQL, [accounts]);

  return {
    accountsChecked: balances.rows.length,
    paymentsChecked: steps.rows.length,
    differences,
    mismatches,
    stuck,
    deliveries: {
      byOutcome: Object.fromEntries(byOutcome.rows.map((r) => [r.outcome, r.n])),
      staleInProgress: stale.rows[0]?.n ?? 0,
      duplicateNotifications: BigInt(duplicates.rows[0]?.n ?? '0'),
    },
    consistent: differences.length === 0 && mismatches.length === 0,
  };
}

function buckets(pendingIn: string, available: string): Buckets {
  return { pendingIn: BigInt(pendingIn), available: BigInt(available) };
}

function same(a: Buckets, b: Buckets): boolean {
  return a.pendingIn === b.pendingIn && a.available === b.available;
}

function sameSteps(actual: readonly string[], expected: readonly string[]): boolean {
  return actual.length === expected.length && actual.every((step, i) => step === expected[i]);
}
