import type { Queryable } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import type { Uuid } from '../shared/ids.ts';
import { parseStatus, type BookedStatus } from './status.ts';
import { accountExists } from '../accounts/accounts.ts';

// The deposit entity and how it is read. Its lifecycle and the transitions that move it
// along are in status.ts; recording a provider notification as a new deposit is in
// ingestion/incoming-payments.ts.

export type Deposit = {
  readonly id: Uuid<'dep'>;
  readonly accountId: Uuid<'acct'>;
  readonly creditorIban: Iban;
  readonly providerAccountId: string | null;
  readonly amountMinor: bigint;
  readonly currency: 'EUR';
  readonly network: 'SEPA';
  readonly networkTransactionId: string;
  readonly status: BookedStatus;
  readonly notificationCount: number;
  readonly createdAt: Date;
  readonly updatedAt: Date;
};

// A deposit in suspense (status unmatched): it matches none of our accounts, so it has none.
// Its own shape, so nothing that works with booked deposits has to handle a missing account.
export type UnmatchedDeposit = {
  readonly id: Uuid<'dep'>;
  readonly status: 'unmatched';
  readonly creditorIban: Iban;
  readonly providerAccountId: string | null;
  readonly amountMinor: bigint;
  readonly currency: 'EUR';
  readonly network: 'SEPA';
  readonly networkTransactionId: string;
  readonly notificationCount: number;
  readonly createdAt: Date;
};

export type DepositRow = {
  id: string;
  creditor_iban: string;
  provider_account_id: string | null;
  // NULL only for an unmatched deposit (a CHECK on the table).
  account_id: string | null;
  amount_minor: bigint;
  currency: string;
  network: string;
  network_transaction_id: string;
  status: string;
  notification_count: number;
  created_at: Date;
  updated_at: Date;
};

// A matched deposit's row. Throws for an unmatched one (use unmatchedDepositFromRow).
export function depositFromRow(row: DepositRow): Deposit {
  const status = parseStatus(row.status);
  if (status === 'unmatched' || row.account_id === null) {
    throw new Error(`deposit ${row.id} is unmatched; it has no account`);
  }
  return {
    id: row.id as Uuid<'dep'>,
    accountId: row.account_id as Uuid<'acct'>,
    // Stored only after parseIban, and the column CHECK guards the format.
    creditorIban: row.creditor_iban as Iban,
    providerAccountId: row.provider_account_id,
    amountMinor: row.amount_minor,
    currency: expectLiteral(row.currency, 'EUR', 'currency'),
    network: expectLiteral(row.network, 'SEPA', 'network'),
    networkTransactionId: row.network_transaction_id,
    status,
    notificationCount: row.notification_count,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

// An unmatched deposit's row. Throws for any other status.
export function unmatchedDepositFromRow(row: DepositRow): UnmatchedDeposit {
  if (row.status !== 'unmatched') {
    throw new Error(`deposit ${row.id} is ${row.status}, not unmatched`);
  }
  return {
    id: row.id as Uuid<'dep'>,
    status: 'unmatched',
    creditorIban: row.creditor_iban as Iban,
    providerAccountId: row.provider_account_id,
    amountMinor: row.amount_minor,
    currency: expectLiteral(row.currency, 'EUR', 'currency'),
    network: expectLiteral(row.network, 'SEPA', 'network'),
    networkTransactionId: row.network_transaction_id,
    notificationCount: row.notification_count,
    createdAt: row.created_at,
  };
}

function expectLiteral<T extends string>(value: string, expected: T, column: string): T {
  if (value !== expected) {
    throw new Error(`unexpected ${column} in database: ${value}`);
  }
  return expected;
}

// A page of an account's deposits, newest first.
export type DepositPage = {
  readonly deposits: readonly Deposit[];
  // Pass as `before` to get the next (older) page; null on the last page.
  readonly nextBefore: Uuid<'dep'> | null;
};

// Seek pagination on the deposit ID: UUIDv7 IDs sort by creation time, so "older than the
// last one seen" is `id < $before`, served by the (account_id, id DESC) index. The filters
// (slice 4 decision 70) are further conditions on the same index's rows: status is checked row
// by row (a known gap if accounts grow large).
//
// One statement assembled from fixed fragments, one per condition present, each with its own
// placeholder: the values are always parameters, never part of the SQL text. A condition left
// out entirely, rather than written as `($n IS NULL OR …)`, keeps each statement as simple to
// plan as the two fixed ones it replaces.
function listSql(conditions: readonly string[]): string {
  return `
SELECT * FROM deposits
WHERE ${conditions.join(' AND ')}
ORDER BY id DESC
LIMIT $1
`;
}

export type DepositFilters = {
  readonly status?: BookedStatus;
  readonly networkTransactionId?: string;
};

// undefined if there is no such account (an account without deposits gets an empty page).
// The existence check is a read before a read: nothing is written based on it. The filters and
// the cursor apply together, so a page holds the next matching deposits older than `before`, and
// nextBefore is the last of them.
export async function listDeposits(
  db: Queryable,
  accountId: Uuid<'acct'>,
  page: { readonly limit: number; readonly before?: Uuid<'dep'> } & DepositFilters,
): Promise<DepositPage | undefined> {
  if (!(await accountExists(db, accountId))) return undefined;

  // One more than the page size, to know whether there is a next page.
  const params: unknown[] = [page.limit + 1];
  const conditions: string[] = [];
  // Each condition's fragment takes the next placeholder: $2, $3, … in the order pushed.
  const where = (fragment: (placeholder: string) => string, value: unknown): void => {
    params.push(value);
    conditions.push(fragment(`$${params.length}`));
  };
  where((p) => `account_id = ${p}`, accountId);
  if (page.before !== undefined) where((p) => `id < ${p}`, page.before);
  if (page.status !== undefined) where((p) => `status = ${p}`, page.status);
  if (page.networkTransactionId !== undefined) {
    where((p) => `network_transaction_id = ${p}`, page.networkTransactionId);
  }
  const result = await db.query<DepositRow>(listSql(conditions), params);

  const deposits = result.rows.slice(0, page.limit).map(depositFromRow);
  const hasMore = result.rows.length > page.limit;
  return { deposits, nextBefore: hasMore ? (deposits.at(-1)?.id ?? null) : null };
}

// The deposit within its account (slice 4 decision 64): one statement, so "no such deposit" and
// "another account's deposit" are the same answer, decided by the database.
const GET_SQL = 'SELECT * FROM deposits WHERE id = $1 AND account_id = $2';

// undefined if the account has no such deposit.
export async function getDeposit(
  db: Queryable,
  accountId: Uuid<'acct'>,
  depositId: Uuid<'dep'>,
): Promise<Deposit | undefined> {
  const result = await db.query<DepositRow>(GET_SQL, [depositId, accountId]);
  const row = result.rows[0];
  return row === undefined ? undefined : depositFromRow(row);
}

// Several of the account's deposits by ID, one statement (for the transactions list, which has
// decided its page on IDs alone: transactions/transactions.ts).
const GET_MANY_SQL = 'SELECT * FROM deposits WHERE account_id = $1 AND id = ANY($2)';

// The account's deposits among ids, in no particular order; an ID the account has no deposit for
// is simply absent (the caller decides what that means).
export async function getDeposits(
  db: Queryable,
  accountId: Uuid<'acct'>,
  ids: readonly Uuid<'dep'>[],
): Promise<Deposit[]> {
  if (ids.length === 0) return [];
  const result = await db.query<DepositRow>(GET_MANY_SQL, [accountId, ids]);
  return result.rows.map(depositFromRow);
}

const EXISTS_SQL = 'SELECT 1 FROM deposits WHERE id = $1';

// For classifying an outcome after the fact (e.g. an event whose transitions all skipped:
// already processed, or a deposit that doesn't exist). Never used to decide whether to act.
export async function depositExists(db: Queryable, depositId: Uuid<'dep'>): Promise<boolean> {
  const result = await db.query(EXISTS_SQL, [depositId]);
  return result.rowCount === 1;
}
