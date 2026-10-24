import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import type { CheckScope } from './scope.ts';

// What incoming-payment ingestion recorded instead of refusing: payments in suspense, and
// notifications that conflicted with a stored payment.

// Suspense: payments that arrived at the bank but match none of our accounts (status unmatched).
// The money is in nobody's books: not inconsistent, but a finance person's job (in production, a
// daily report). Whole-database runs only: an unmatched payment belongs to no account.
export type SuspenseReport = {
  readonly count: number;
  readonly totalMinor: bigint;
  readonly oldestAgeSeconds: number | null;
  // Oldest first.
  readonly payments: readonly {
    readonly paymentId: Uuid<'pay'>;
    readonly networkTransactionId: string;
    readonly amountMinor: bigint;
    readonly ageSeconds: number;
  }[];
};

// A notification that conflicted with the stored payment (the same network transaction ID with
// other details): recorded, nothing changed.
export type PaymentConflictRecord = {
  readonly paymentId: Uuid<'pay'>;
  readonly differingFields: readonly string[];
  readonly receivedAt: Date;
};

const SUSPENSE_SQL = `
SELECT id, network_transaction_id, amount_minor,
       floor(extract(epoch FROM now() - created_at))::int AS age_seconds
FROM incoming_payments
WHERE status = 'unmatched'
ORDER BY created_at, id
`;

const PAYMENT_CONFLICTS_SQL = `
SELECT c.payment_id, c.differing_fields, c.received_at
FROM payment_notification_conflicts c
JOIN incoming_payments p ON p.id = c.payment_id
WHERE $1::uuid[] IS NULL OR p.account_id = ANY($1)
ORDER BY c.received_at, c.id
`;

export async function checkSuspense(
  db: Queryable,
  scope: CheckScope,
): Promise<{
  readonly suspense: SuspenseReport | null;
  readonly paymentConflicts: PaymentConflictRecord[];
}> {
  // null in an account-scoped run: an unmatched payment belongs to no account.
  let suspense: SuspenseReport | null = null;
  if (scope.accounts === null) {
    const unmatched = await db.query<{
      id: string;
      network_transaction_id: string;
      amount_minor: bigint;
      age_seconds: number;
    }>(SUSPENSE_SQL);
    const payments = unmatched.rows.map((row) => ({
      paymentId: row.id as Uuid<'pay'>,
      networkTransactionId: row.network_transaction_id,
      amountMinor: row.amount_minor,
      ageSeconds: row.age_seconds,
    }));
    suspense = {
      count: payments.length,
      totalMinor: payments.reduce((sum, p) => sum + p.amountMinor, 0n),
      oldestAgeSeconds: payments[0]?.ageSeconds ?? null,
      payments,
    };
  }
  const conflicts = await db.query<{
    payment_id: string;
    differing_fields: string[];
    received_at: Date;
  }>(PAYMENT_CONFLICTS_SQL, [scope.accounts]);
  const paymentConflicts: PaymentConflictRecord[] = conflicts.rows.map((row) => ({
    paymentId: row.payment_id as Uuid<'pay'>,
    differingFields: row.differing_fields,
    receivedAt: row.received_at,
  }));
  return { suspense, paymentConflicts };
}
