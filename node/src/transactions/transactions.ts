import { getDeposits, type Deposit } from '../deposits/deposits.ts';
import type { Queryable } from '../shared/db.ts';
import { toTypeId, type Uuid } from '../shared/ids.ts';
import { getWithdrawals, type Withdrawal } from '../withdrawals/withdrawals.ts';

// The account's transactions: its deposits and withdrawals together, newest first (slice 4
// decision 67). A read model over the two entities, so it sits above both modules.
//
// How one ordered page comes from two tables:
//   1. one UNION ALL of (type, id), each branch its own account-scoped, index-ordered query with
//      the cursor and limit + 1, decides the page: which items, in what order, and whether there
//      is a next one, on IDs alone, in one statement;
//   2. each kind's full rows are then read by ID within the account, one statement per kind on
//      the page (getDeposits, getWithdrawals), with the same row mappings as their own endpoints,
//      and put in the order step 1 decided.
// At most four statements (with the account check), whatever the page size: no N+1.
//
// No transaction around them (READ COMMITTED): the page is decided on IDs alone, deposits and
// withdrawals are never deleted, and their account never changes, so step 2 finds every ID step
// 1 chose; only an item's state can be newer than step 1's snapshot, which is what its own GET
// would show at that moment. That stops holding if an account can change: e.g. resolving a
// deposit from suspense into an account (out of scope), which would let a deposit join an account
// between the statements. A row step 2 can't find is therefore an invariant violation, thrown
// (a 500), never a shorter page (decision 67).

export type Transaction =
  | { readonly type: 'deposit'; readonly deposit: Deposit }
  | { readonly type: 'withdrawal'; readonly withdrawal: Withdrawal };

// A position in the list: an item's type and ID. As a cursor (before), only the ID counts: UUIDv7
// IDs sort by creation time across both tables, so either kind's ID is a position in time
// (decision 66).
export type TransactionRef =
  | { readonly type: 'deposit'; readonly id: Uuid<'dep'> }
  | { readonly type: 'withdrawal'; readonly id: Uuid<'wd'> };

export type TransactionPage = {
  readonly transactions: readonly Transaction[];
  // Pass as `before` to get the next (older) page; null on the last page.
  readonly nextBefore: TransactionRef | null;
};

const ACCOUNT_EXISTS_SQL = 'SELECT 1 FROM accounts WHERE id = $1';

// Each branch orders and limits itself, so it stops after limit + 1 rows of its own
// (account_id, id DESC) index whatever the account's history: no more than that many of the
// merged page can come from one table. The cursor, if any, is the same position in both. type
// breaks a tie between equal IDs, so the order is total. As the other lists, assembled from fixed
// fragments, the values always parameters.
function pageSql(withCursor: boolean): string {
  const where = withCursor ? 'account_id = $1 AND id < $3' : 'account_id = $1';
  return `
SELECT type, id FROM (
  (SELECT 'deposit' AS type, id FROM deposits
   WHERE ${where}
   ORDER BY id DESC
   LIMIT $2)
  UNION ALL
  (SELECT 'withdrawal' AS type, id FROM withdrawals
   WHERE ${where}
   ORDER BY id DESC
   LIMIT $2)
) AS page
ORDER BY id DESC, type
LIMIT $2
`;
}

// A row step 2 couldn't find. Thrown, so the API answers 500 and logs it: the page would
// otherwise be silently shorter than the one decided.
export class TransactionsInvariantError extends Error {
  constructor(missing: readonly string[]) {
    super(`transactions list: decided on, then not found by ID: ${missing.join(', ')}`);
    this.name = 'TransactionsInvariantError';
  }
}

// undefined if there is no such account (an account without transactions gets an empty page).
// The existence check is a read before a read: nothing is written based on it.
export async function listTransactions(
  db: Queryable,
  accountId: Uuid<'acct'>,
  page: { readonly limit: number; readonly before?: TransactionRef },
): Promise<TransactionPage | undefined> {
  const account = await db.query(ACCOUNT_EXISTS_SQL, [accountId]);
  if (account.rowCount === 0) return undefined;

  // Step 1: the page, decided on IDs. One more than the page size, to know whether there is a
  // next page.
  const params: unknown[] = [accountId, page.limit + 1];
  if (page.before !== undefined) params.push(page.before.id);
  const decided = await db.query<{ type: 'deposit' | 'withdrawal'; id: string }>(
    pageSql(page.before !== undefined),
    params,
  );
  const refs = decided.rows
    .slice(0, page.limit)
    .map((row): TransactionRef =>
      row.type === 'deposit'
        ? { type: 'deposit', id: row.id as Uuid<'dep'> }
        : { type: 'withdrawal', id: row.id as Uuid<'wd'> },
    );
  const hasMore = decided.rows.length > page.limit;

  // Step 2: the full rows, per kind, mapped as their own endpoints map them.
  const depositIds = refs.flatMap((r) => (r.type === 'deposit' ? [r.id] : []));
  const withdrawalIds = refs.flatMap((r) => (r.type === 'withdrawal' ? [r.id] : []));
  const [deposits, withdrawals] = await Promise.all([
    getDeposits(db, accountId, depositIds),
    getWithdrawals(db, accountId, withdrawalIds),
  ]);
  const depositById = new Map(deposits.map((d) => [d.id, d]));
  const withdrawalById = new Map(withdrawals.map((w) => [w.id, w]));

  const missing: string[] = [];
  const transactions: Transaction[] = [];
  for (const ref of refs) {
    if (ref.type === 'deposit') {
      const deposit = depositById.get(ref.id);
      if (deposit === undefined) missing.push(toTypeId('dep', ref.id));
      else transactions.push({ type: 'deposit', deposit });
    } else {
      const withdrawal = withdrawalById.get(ref.id);
      if (withdrawal === undefined) missing.push(toTypeId('wd', ref.id));
      else transactions.push({ type: 'withdrawal', withdrawal });
    }
  }
  if (missing.length > 0) throw new TransactionsInvariantError(missing);

  return { transactions, nextBefore: hasMore ? (refs.at(-1) ?? null) : null };
}
