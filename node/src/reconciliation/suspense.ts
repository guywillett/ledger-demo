import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import type { CheckScope } from './scope.ts';

// What incoming-payment ingestion recorded instead of refusing: deposits in suspense, and
// notifications that conflicted with a stored deposit.

// Suspense: deposits that arrived at the bank but match none of our accounts (status unmatched).
// The money is in nobody's books: not inconsistent, but a finance person's job (in production, a
// daily report). Whole-database runs only: an unmatched deposit belongs to no account.
export type SuspenseReport = {
  readonly count: number;
  readonly totalMinor: bigint;
  readonly oldestAgeSeconds: number | null;
  // Oldest first.
  readonly deposits: readonly {
    readonly depositId: Uuid<'dep'>;
    readonly networkTransactionId: string;
    readonly amountMinor: bigint;
    readonly ageSeconds: number;
  }[];
};

// A notification that conflicted with the stored deposit (the same network transaction ID with
// other details): recorded, nothing changed.
export type DepositConflictRecord = {
  readonly depositId: Uuid<'dep'>;
  readonly differingFields: readonly string[];
  readonly receivedAt: Date;
};

const SUSPENSE_SQL = `
SELECT id, network_transaction_id, amount_minor,
       floor(extract(epoch FROM now() - created_at))::int AS age_seconds
FROM deposits
WHERE status = 'unmatched'
ORDER BY created_at, id
`;

const DEPOSIT_CONFLICTS_SQL = `
SELECT c.deposit_id, c.differing_fields, c.received_at
FROM deposit_notification_conflicts c
JOIN deposits p ON p.id = c.deposit_id
WHERE $1::uuid[] IS NULL OR p.account_id = ANY($1)
ORDER BY c.received_at, c.id
`;

export async function checkSuspense(
  db: Queryable,
  scope: CheckScope,
): Promise<{
  readonly suspense: SuspenseReport | null;
  readonly depositConflicts: DepositConflictRecord[];
}> {
  // null in an account-scoped run: an unmatched deposit belongs to no account.
  let suspense: SuspenseReport | null = null;
  if (scope.accounts === null) {
    const unmatched = await db.query<{
      id: string;
      network_transaction_id: string;
      amount_minor: bigint;
      age_seconds: number;
    }>(SUSPENSE_SQL);
    const deposits = unmatched.rows.map((row) => ({
      depositId: row.id as Uuid<'dep'>,
      networkTransactionId: row.network_transaction_id,
      amountMinor: row.amount_minor,
      ageSeconds: row.age_seconds,
    }));
    suspense = {
      count: deposits.length,
      totalMinor: deposits.reduce((sum, p) => sum + p.amountMinor, 0n),
      oldestAgeSeconds: deposits[0]?.ageSeconds ?? null,
      deposits,
    };
  }
  const conflicts = await db.query<{
    deposit_id: string;
    differing_fields: string[];
    received_at: Date;
  }>(DEPOSIT_CONFLICTS_SQL, [scope.accounts]);
  const depositConflicts: DepositConflictRecord[] = conflicts.rows.map((row) => ({
    depositId: row.deposit_id as Uuid<'dep'>,
    differingFields: row.differing_fields,
    receivedAt: row.received_at,
  }));
  return { suspense, depositConflicts };
}
