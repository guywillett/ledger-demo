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
// completion, so `history` runs although nothing reads it (as for payments).
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

export async function getWithdrawal(
  db: Queryable,
  id: Uuid<'wd'>,
): Promise<Withdrawal | undefined> {
  const result = await db.query<WithdrawalRow>(GET_SQL, [id]);
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}
