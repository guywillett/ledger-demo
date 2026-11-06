import type { Queryable, Tx } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import { newUuid, type Uuid } from '../shared/ids.ts';
import { parseWithdrawalStatus, type WithdrawalStatus } from './status.ts';

// The withdrawal entity and how it is stored and read. Its lifecycle and transitions are in
// status.ts; holding the funds, executing and releasing them are ledger postings
// (ledger/withdrawals.ts).

export type Withdrawal = {
  readonly id: Uuid<'wd'>;
  readonly accountId: Uuid<'acct'>;
  readonly idempotencyKey: string;
  readonly amountMinor: bigint;
  readonly currency: 'EUR';
  readonly creditorIban: Iban;
  readonly creditorName: string;
  readonly remittanceInfo: string | null;
  readonly status: WithdrawalStatus;
  readonly failureReason: string | null;
  readonly providerTransferId: string | null;
  readonly createdAt: Date;
  readonly updatedAt: Date;
};

type WithdrawalRow = {
  id: string;
  account_id: string;
  idempotency_key: string;
  amount_minor: bigint;
  currency: string;
  creditor_iban: string;
  creditor_name: string;
  remittance_info: string | null;
  status: string;
  failure_reason: string | null;
  provider_transfer_id: string | null;
  created_at: Date;
  updated_at: Date;
};

function withdrawalFromRow(row: WithdrawalRow): Withdrawal {
  if (row.currency !== 'EUR') {
    throw new Error(`unexpected currency in database: ${row.currency}`);
  }
  return {
    id: row.id as Uuid<'wd'>,
    accountId: row.account_id as Uuid<'acct'>,
    idempotencyKey: row.idempotency_key,
    amountMinor: row.amount_minor,
    currency: row.currency,
    // Stored only after parseIban, and the column CHECK guards the format.
    creditorIban: row.creditor_iban as Iban,
    creditorName: row.creditor_name,
    remittanceInfo: row.remittance_info,
    status: parseWithdrawalStatus(row.status),
    failureReason: row.failure_reason,
    providerTransferId: row.provider_transfer_id,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

// The withdrawal's account doesn't exist: the insert's foreign key refused it (no existence
// read before the insert). Thrown, like InsufficientFundsError, because the failed statement has
// aborted the transaction; the caller handles it after the rollback (the API answers 404).
export class AccountNotFoundError extends Error {
  readonly accountId: Uuid<'acct'>;

  constructor(accountId: Uuid<'acct'>, cause: unknown) {
    super(`account ${accountId} does not exist`, { cause });
    this.name = 'AccountNotFoundError';
    this.accountId = accountId;
  }
}

// A foreign_key_violation (SQLSTATE 23503) on the withdrawal's account becomes an
// AccountNotFoundError; anything else is returned unchanged, to be rethrown as it was.
function asAccountNotFound(err: unknown, accountId: Uuid<'acct'>): unknown {
  if (typeof err !== 'object' || err === null) return err;
  if (!('code' in err) || err.code !== '23503') return err;
  if (!('constraint' in err) || err.constraint !== 'withdrawals_account_id_fkey') return err;
  return new AccountNotFoundError(accountId, err);
}

export type NewWithdrawal = {
  readonly accountId: Uuid<'acct'>;
  readonly idempotencyKey: string;
  readonly amountMinor: bigint;
  readonly currency: 'EUR';
  readonly creditorIban: Iban;
  readonly creditorName: string;
  readonly remittanceInfo: string | null;
};

// One statement inserts the withdrawal in `held` and its first history row, or nothing when the
// account already has a withdrawal with this idempotency key. Data-modifying CTEs always run to
// completion, so `history` runs although nothing reads it (as for deposits).
const INSERT_SQL = `
WITH inserted AS (
  INSERT INTO withdrawals (id, account_id, idempotency_key, amount_minor, currency,
                           creditor_iban, creditor_name, remittance_info, status)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'held')
  ON CONFLICT (account_id, idempotency_key) DO NOTHING
  RETURNING *
),
history AS (
  INSERT INTO withdrawal_status_history (withdrawal_id, from_status, to_status)
  SELECT id, NULL, 'held' FROM inserted
)
SELECT * FROM inserted
`;

// Inserts a new withdrawal in `held`. undefined when the key is already taken on this account
// (classifying the repeat, same request or conflicting one, is the caller's job).
//
// Not for creating withdrawals directly: use createWithdrawal (ledger/withdrawals.ts), which
// also holds the funds in the same transaction. A `held` withdrawal is only valid together with
// its hold; inserted alone, it would have nothing reserved.
//
// Throws AccountNotFoundError if the account doesn't exist (the foreign key decides).
//
// Concurrency: if another transaction is inserting the same key, ON CONFLICT waits for it; if it
// commits, this insert does nothing (undefined); if it rolls back, this one inserts.
export async function insertWithdrawal(tx: Tx, w: NewWithdrawal): Promise<Withdrawal | undefined> {
  const result = await tx
    .query<WithdrawalRow>(INSERT_SQL, [
      newUuid('wd'),
      w.accountId,
      w.idempotencyKey,
      w.amountMinor,
      w.currency,
      w.creditorIban,
      w.creditorName,
      w.remittanceInfo,
    ])
    .catch((err: unknown) => {
      throw asAccountNotFound(err, w.accountId);
    });
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}

const GET_SQL = 'SELECT * FROM withdrawals WHERE id = $1';

const BY_KEY_SQL = 'SELECT * FROM withdrawals WHERE account_id = $1 AND idempotency_key = $2';

// The withdrawal holding an account's idempotency key. For classifying a request whose insert
// found the key taken; never used to decide whether to insert.
export async function getWithdrawalByKey(
  db: Queryable,
  accountId: Uuid<'acct'>,
  idempotencyKey: string,
): Promise<Withdrawal | undefined> {
  const result = await db.query<WithdrawalRow>(BY_KEY_SQL, [accountId, idempotencyKey]);
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}

// By ID alone: for the system's own use (the submitter, the transfer-update webhook), which acts on
// a withdrawal whatever its account. The client API reads one with getWithdrawalInAccount.
export async function getWithdrawal(
  db: Queryable,
  id: Uuid<'wd'>,
): Promise<Withdrawal | undefined> {
  const result = await db.query<WithdrawalRow>(GET_SQL, [id]);
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}

// The withdrawal within its account (slice 4 decision 64): one statement, so "no such
// withdrawal" and "another account's withdrawal" are the same answer, decided by the database.
const GET_IN_ACCOUNT_SQL = 'SELECT * FROM withdrawals WHERE id = $1 AND account_id = $2';

// undefined if the account has no such withdrawal.
export async function getWithdrawalInAccount(
  db: Queryable,
  accountId: Uuid<'acct'>,
  id: Uuid<'wd'>,
): Promise<Withdrawal | undefined> {
  const result = await db.query<WithdrawalRow>(GET_IN_ACCOUNT_SQL, [id, accountId]);
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}

// Several of the account's withdrawals by ID, one statement (for the transactions list, which has
// decided its page on IDs alone: transactions/transactions.ts).
const GET_MANY_SQL = 'SELECT * FROM withdrawals WHERE account_id = $1 AND id = ANY($2)';

// The account's withdrawals among ids, in no particular order; an ID the account has no
// withdrawal for is simply absent (the caller decides what that means).
export async function getWithdrawals(
  db: Queryable,
  accountId: Uuid<'acct'>,
  ids: readonly Uuid<'wd'>[],
): Promise<Withdrawal[]> {
  if (ids.length === 0) return [];
  const result = await db.query<WithdrawalRow>(GET_MANY_SQL, [accountId, ids]);
  return result.rows.map(withdrawalFromRow);
}

// A page of an account's withdrawals, newest first.
export type WithdrawalPage = {
  readonly withdrawals: readonly Withdrawal[];
  // Pass as `before` to get the next (older) page; null on the last page.
  readonly nextBefore: Uuid<'wd'> | null;
};

const ACCOUNT_EXISTS_SQL = 'SELECT 1 FROM accounts WHERE id = $1';

// As the deposits list (deposits/deposits.ts): seek pagination on the ID, served by the
// (account_id, id DESC) index, and one statement assembled from fixed fragments, one per
// condition present, the values always parameters.
function listSql(conditions: readonly string[]): string {
  return `
SELECT * FROM withdrawals
WHERE ${conditions.join(' AND ')}
ORDER BY id DESC
LIMIT $1
`;
}

// undefined if there is no such account (an account without withdrawals gets an empty page).
// The existence check is a read before a read: nothing is written based on it. The status
// filter and the cursor apply together, so nextBefore is the last of the filtered page.
export async function listWithdrawals(
  db: Queryable,
  accountId: Uuid<'acct'>,
  page: {
    readonly limit: number;
    readonly before?: Uuid<'wd'>;
    readonly status?: WithdrawalStatus;
  },
): Promise<WithdrawalPage | undefined> {
  const account = await db.query(ACCOUNT_EXISTS_SQL, [accountId]);
  if (account.rowCount === 0) return undefined;

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
  const result = await db.query<WithdrawalRow>(listSql(conditions), params);

  const withdrawals = result.rows.slice(0, page.limit).map(withdrawalFromRow);
  const hasMore = result.rows.length > page.limit;
  return { withdrawals, nextBefore: hasMore ? (withdrawals.at(-1)?.id ?? null) : null };
}
