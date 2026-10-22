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
// Concurrency: if another transaction is inserting the same key, ON CONFLICT waits for it; if it
// commits, this insert does nothing (undefined); if it rolls back, this one inserts.
export async function insertWithdrawal(tx: Tx, w: NewWithdrawal): Promise<Withdrawal | undefined> {
  const result = await tx.query<WithdrawalRow>(INSERT_SQL, [
    newUuid('wd'),
    w.accountId,
    w.idempotencyKey,
    w.amountMinor,
    w.currency,
    w.creditorIban,
    w.creditorName,
    w.remittanceInfo,
  ]);
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}

const GET_SQL = 'SELECT * FROM withdrawals WHERE id = $1';

export async function getWithdrawal(
  db: Queryable,
  id: Uuid<'wd'>,
): Promise<Withdrawal | undefined> {
  const result = await db.query<WithdrawalRow>(GET_SQL, [id]);
  const row = result.rows[0];
  return row === undefined ? undefined : withdrawalFromRow(row);
}
