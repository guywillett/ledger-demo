import type { Queryable } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';

// Reading balances. Only move() changes them (ledger/move.ts).

export type Balance = {
  readonly pendingIn: bigint;
  readonly available: bigint;
  // Single currency in slice 1 (amounts are EUR minor units throughout).
  readonly currency: 'EUR';
};

const BALANCE_SQL = `
SELECT pending_in, available
FROM account_balances
WHERE account_id = $1
`;

// undefined if there is no such account: every account gets its balance row when it is
// created, so a missing row means a missing account.
export async function getBalance(
  db: Queryable,
  accountId: Uuid<'acct'>,
): Promise<Balance | undefined> {
  const result = await db.query<{ pending_in: bigint; available: bigint }>(BALANCE_SQL, [
    accountId,
  ]);
  const row = result.rows[0];
  if (row === undefined) return undefined;
  return { pendingIn: row.pending_in, available: row.available, currency: 'EUR' };
}
