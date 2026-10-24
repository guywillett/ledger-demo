import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import type { CheckScope } from './scope.ts';

// The balance check: each account's balance row against what its payment and withdrawal statuses
// say, and against the net of its movements.

export type Buckets = {
  readonly pendingIn: bigint;
  readonly available: bigint;
  readonly held: bigint;
};

// An account whose balance disagrees with what its payment and withdrawal statuses, or its
// movements, say.
export type AccountDifference = {
  readonly accountId: Uuid<'acct'>;
  // null: the account has no balance row. A trigger creates it with the account, but nothing
  // stops it being deleted later (and move() can't post to the account without it).
  readonly balance: Buckets | null;
  readonly fromStatuses: Buckets;
  readonly fromMovements: Buckets;
};

// $1 is the account filter (NULL for all accounts) throughout.
//
// sum() of a bigint column is numeric, which pg returns as a string; ::text makes that explicit,
// and the string is parsed with BigInt (exact). COALESCE turns "no rows" into 0.
//
// Starts from accounts, not account_balances, so an account without a balance row shows up
// (with NULL balances) instead of being skipped. One row per account, all held in memory (see
// the scale note at the top).
//
// What the statuses say: pending_in is the payments being processed; available is the credited
// payments minus every withdrawal that has taken funds from it (held, submitted or executed;
// a failed one gave them back); held is the withdrawals not yet settled.
const BALANCES_SQL = `
WITH accounts_checked AS (
  SELECT a.id AS account_id, b.pending_in, b.available, b.held
  FROM accounts a
  LEFT JOIN account_balances b ON b.account_id = a.id
  WHERE $1::uuid[] IS NULL OR a.id = ANY($1)
),
by_payments AS (
  SELECT account_id,
         COALESCE(sum(amount_minor)
                    FILTER (WHERE status IN ('received', 'under_review', 'approved', 'rejected')),
                  0) AS pending_in,
         COALESCE(sum(amount_minor) FILTER (WHERE status = 'credited'), 0) AS available
  FROM incoming_payments
  WHERE account_id IN (SELECT account_id FROM accounts_checked)
  GROUP BY account_id
),
by_withdrawals AS (
  SELECT account_id,
         COALESCE(sum(amount_minor) FILTER (WHERE status IN ('held', 'submitted', 'executed')),
                  0) AS spent,
         COALESCE(sum(amount_minor) FILTER (WHERE status IN ('held', 'submitted')), 0) AS held
  FROM withdrawals
  WHERE account_id IN (SELECT account_id FROM accounts_checked)
  GROUP BY account_id
),
by_movements AS (
  SELECT account_id,
         COALESCE(sum(amount_minor) FILTER (WHERE to_bucket = 'pending_in'), 0)
           - COALESCE(sum(amount_minor) FILTER (WHERE from_bucket = 'pending_in'), 0) AS pending_in,
         COALESCE(sum(amount_minor) FILTER (WHERE to_bucket = 'available'), 0)
           - COALESCE(sum(amount_minor) FILTER (WHERE from_bucket = 'available'), 0) AS available,
         COALESCE(sum(amount_minor) FILTER (WHERE to_bucket = 'held'), 0)
           - COALESCE(sum(amount_minor) FILTER (WHERE from_bucket = 'held'), 0) AS held
  FROM balance_movements
  WHERE account_id IN (SELECT account_id FROM accounts_checked)
  GROUP BY account_id
)
SELECT a.account_id,
       a.pending_in::text AS balance_pending_in,
       a.available::text AS balance_available,
       a.held::text AS balance_held,
       COALESCE(p.pending_in, 0)::text AS statuses_pending_in,
       (COALESCE(p.available, 0) - COALESCE(w.spent, 0))::text AS statuses_available,
       COALESCE(w.held, 0)::text AS statuses_held,
       COALESCE(m.pending_in, 0)::text AS movements_pending_in,
       COALESCE(m.available, 0)::text AS movements_available,
       COALESCE(m.held, 0)::text AS movements_held
FROM accounts_checked a
LEFT JOIN by_payments p USING (account_id)
LEFT JOIN by_withdrawals w USING (account_id)
LEFT JOIN by_movements m USING (account_id)
`;

export async function checkBalances(
  db: Queryable,
  scope: CheckScope,
): Promise<{ readonly differences: AccountDifference[]; readonly accountsChecked: number }> {
  const balances = await db.query<{
    account_id: string;
    // null when the account has no balance row.
    balance_pending_in: string | null;
    balance_available: string | null;
    balance_held: string | null;
    statuses_pending_in: string;
    statuses_available: string;
    statuses_held: string;
    movements_pending_in: string;
    movements_available: string;
    movements_held: string;
  }>(BALANCES_SQL, [scope.accounts]);
  const differences: AccountDifference[] = [];
  for (const row of balances.rows) {
    const check: AccountDifference = {
      accountId: row.account_id as Uuid<'acct'>,
      balance:
        row.balance_pending_in === null ||
        row.balance_available === null ||
        row.balance_held === null
          ? null
          : buckets(row.balance_pending_in, row.balance_available, row.balance_held),
      fromStatuses: buckets(row.statuses_pending_in, row.statuses_available, row.statuses_held),
      fromMovements: buckets(row.movements_pending_in, row.movements_available, row.movements_held),
    };
    if (
      check.balance === null ||
      !same(check.balance, check.fromStatuses) ||
      !same(check.balance, check.fromMovements)
    ) {
      differences.push(check);
    }
  }
  return { differences, accountsChecked: balances.rows.length };
}

function buckets(pendingIn: string, available: string, held: string): Buckets {
  return { pendingIn: BigInt(pendingIn), available: BigInt(available), held: BigInt(held) };
}

function same(a: Buckets, b: Buckets): boolean {
  return a.pendingIn === b.pendingIn && a.available === b.available && a.held === b.held;
}
