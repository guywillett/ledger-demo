import type { Queryable } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import { newUuid, type Uuid } from '../shared/ids.ts';
import { parseStatus, type PaymentStatus } from '../shared/payment-status.ts';

// Ingestion: records provider notifications as payments. This is the edge where provider
// identifiers (IBANs, provider account IDs) are mapped to our account; nothing downstream
// sees them. It never touches balances.

export type Payment = {
  readonly id: Uuid<'pay'>;
  readonly accountId: Uuid<'acct'>;
  readonly creditorIban: Iban;
  readonly providerAccountId: string | null;
  readonly amountMinor: bigint;
  readonly currency: 'EUR';
  readonly network: 'SEPA';
  readonly networkTransactionId: string;
  readonly status: PaymentStatus;
  readonly notificationCount: number;
  readonly createdAt: Date;
  readonly updatedAt: Date;
};

export type Notification = {
  readonly creditorIban: Iban;
  readonly providerAccountId: string | null;
  readonly amountMinor: bigint;
  readonly currency: 'EUR';
  readonly network: 'SEPA';
  readonly networkTransactionId: string;
};

// The details that must match for a repeat notification to count as a duplicate.
export type ComparedField = 'creditor_iban' | 'amount_minor' | 'currency';

export type StoredDetails = {
  readonly paymentId: Uuid<'pay'>;
  readonly creditorIban: Iban;
  readonly amountMinor: bigint;
  readonly currency: string;
};

export type RecordResult =
  | { readonly kind: 'created'; readonly payment: Payment }
  | { readonly kind: 'duplicate'; readonly payment: Payment }
  | {
      readonly kind: 'conflict';
      readonly differingFields: readonly ComparedField[];
      readonly stored: StoredDetails;
    }
  | { readonly kind: 'unknown_account' }
  // The statement refused to record the notification, a payment with this network ID is
  // stored, yet no compared field differs. Not a client error: either the SQL rule and
  // compare() have drifted apart, or an account's IBAN changed (which must never happen).
  | { readonly kind: 'inconsistent'; readonly stored: StoredDetails };

type PaymentRow = {
  id: string;
  creditor_iban: string;
  provider_account_id: string | null;
  account_id: string;
  amount_minor: bigint;
  currency: string;
  network: string;
  network_transaction_id: string;
  status: string;
  notification_count: number;
  created_at: Date;
  updated_at: Date;
};

// One statement records the notification: match the IBAN to our account and insert the payment
// with its first history row, or recognise a duplicate (same details: count it). It returns the
// payment in both cases, and no row when nothing was recorded: either the network ID is already
// used with different details (conflict), or the IBAN matches none of our accounts.
//
// Postgres 18: RETURNING can reference old and new. old is NULL for a row that was just
// inserted, so (old.id IS NULL) means "inserted" (older examples use the xmax = 0 trick).
// Data-modifying CTEs always run to completion, so `history` runs although nothing reads it.
const RECORD_NOTIFICATION_SQL = `
WITH upsert AS (
  INSERT INTO incoming_payments (id, creditor_iban, provider_account_id, account_id,
                                 amount_minor, currency, network, network_transaction_id, status)
  SELECT $1, $2, $3, accounts.id, $4, $5, $6, $7, 'created'
  FROM accounts
  WHERE accounts.iban = $2
  ON CONFLICT (network, network_transaction_id) DO UPDATE
    SET notification_count = incoming_payments.notification_count + 1,
        updated_at = now()
    WHERE (incoming_payments.creditor_iban, incoming_payments.amount_minor, incoming_payments.currency)
        = (EXCLUDED.creditor_iban, EXCLUDED.amount_minor, EXCLUDED.currency)
  RETURNING new.*, (old.id IS NULL) AS inserted
),
history AS (
  INSERT INTO payment_status_history (payment_id, from_status, to_status)
  SELECT id, NULL, 'created' FROM upsert WHERE inserted
)
SELECT * FROM upsert
`;

// When nothing was recorded, a fresh read of the network ID tells conflict from unknown
// account: a stored payment means conflict (and gives the details to compare), none means
// unknown account. The decision not to record was already made, atomically, by the statement
// above; this read only classifies it. Payments are never deleted.
//
// Concurrency: when the IBAN matches an account, ON CONFLICT decides. If another transaction is
// inserting the same network ID, Postgres waits for it and checks against the committed row,
// so the outcome is exact (same details -> duplicate, different -> conflict, exactly one caller
// creates), and the read below always finds the conflicting row. When the IBAN matches no
// account, nothing is inserted and ON CONFLICT never runs, so only this read decides. It runs
// after the statement with a fresh snapshot, so a competing insert committed in the meantime
// is seen (conflict). One that has not committed yet when the read runs gives unknown_account:
// at that moment no payment with this network ID is stored, so that answer is accurate.
const STORED_DETAILS_SQL = `
SELECT id, creditor_iban, amount_minor, currency
FROM incoming_payments
WHERE network = $1 AND network_transaction_id = $2
`;

export async function recordNotification(
  db: Queryable,
  notification: Notification,
): Promise<RecordResult> {
  const result = await db.query<PaymentRow & { inserted: boolean }>(RECORD_NOTIFICATION_SQL, [
    newUuid('pay'),
    notification.creditorIban,
    notification.providerAccountId,
    notification.amountMinor,
    notification.currency,
    notification.network,
    notification.networkTransactionId,
  ]);
  const row = result.rows[0];
  if (row !== undefined) {
    const payment = paymentFromRow(row);
    return row.inserted ? { kind: 'created', payment } : { kind: 'duplicate', payment };
  }

  const stored = await readStoredDetails(db, notification);
  if (stored === undefined) {
    return { kind: 'unknown_account' };
  }
  const differingFields = compare(stored, notification);
  if (differingFields.length === 0) {
    return { kind: 'inconsistent', stored };
  }
  return { kind: 'conflict', differingFields, stored };
}

async function readStoredDetails(
  db: Queryable,
  notification: Notification,
): Promise<StoredDetails | undefined> {
  const result = await db.query<{
    id: string;
    creditor_iban: string;
    amount_minor: bigint;
    currency: string;
  }>(STORED_DETAILS_SQL, [notification.network, notification.networkTransactionId]);
  const row = result.rows[0];
  if (row === undefined) {
    return undefined;
  }
  return {
    paymentId: row.id as Uuid<'pay'>,
    creditorIban: row.creditor_iban as Iban,
    amountMinor: row.amount_minor,
    currency: row.currency,
  };
}

// Must compare exactly the fields the upsert's ON CONFLICT … WHERE compares.
function compare(stored: StoredDetails, notification: Notification): ComparedField[] {
  const fields: ComparedField[] = [];
  if (stored.creditorIban !== notification.creditorIban) fields.push('creditor_iban');
  if (stored.amountMinor !== notification.amountMinor) fields.push('amount_minor');
  if (stored.currency !== notification.currency) fields.push('currency');
  return fields;
}

function paymentFromRow(row: PaymentRow): Payment {
  return {
    id: row.id as Uuid<'pay'>,
    accountId: row.account_id as Uuid<'acct'>,
    // Stored only after parseIban, and the column CHECK guards the format.
    creditorIban: row.creditor_iban as Iban,
    providerAccountId: row.provider_account_id,
    amountMinor: row.amount_minor,
    currency: expectLiteral(row.currency, 'EUR', 'currency'),
    network: expectLiteral(row.network, 'SEPA', 'network'),
    networkTransactionId: row.network_transaction_id,
    status: parseStatus(row.status),
    notificationCount: row.notification_count,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function expectLiteral<T extends string>(value: string, expected: T, column: string): T {
  if (value !== expected) {
    throw new Error(`unexpected ${column} in database: ${value}`);
  }
  return expected;
}

// A page of an account's payments, newest first.
export type PaymentPage = {
  readonly payments: readonly Payment[];
  // Pass as `before` to get the next (older) page; null on the last page.
  readonly nextBefore: Uuid<'pay'> | null;
};

const ACCOUNT_EXISTS_SQL = 'SELECT 1 FROM accounts WHERE id = $1';

// Seek pagination on the payment ID: UUIDv7 IDs sort by creation time, so "older than the
// last one seen" is `id < $before`, served by the (account_id, id DESC) index. Two fixed
// statements rather than one with an optional condition, which keeps each one simple to plan.
const FIRST_PAGE_SQL = `
SELECT * FROM incoming_payments
WHERE account_id = $1
ORDER BY id DESC
LIMIT $2
`;
const NEXT_PAGE_SQL = `
SELECT * FROM incoming_payments
WHERE account_id = $1 AND id < $3
ORDER BY id DESC
LIMIT $2
`;

// undefined if there is no such account (an account without payments gets an empty page).
// The existence check is a read before a read: nothing is written based on it.
export async function listPayments(
  db: Queryable,
  accountId: Uuid<'acct'>,
  page: { readonly limit: number; readonly before?: Uuid<'pay'> },
): Promise<PaymentPage | undefined> {
  const account = await db.query(ACCOUNT_EXISTS_SQL, [accountId]);
  if (account.rowCount === 0) return undefined;

  // One more than the page size, to know whether there is a next page.
  const params: unknown[] = [accountId, page.limit + 1];
  const sql = page.before === undefined ? FIRST_PAGE_SQL : NEXT_PAGE_SQL;
  if (page.before !== undefined) params.push(page.before);
  const result = await db.query<PaymentRow>(sql, params);

  const payments = result.rows.slice(0, page.limit).map(paymentFromRow);
  const hasMore = result.rows.length > page.limit;
  return { payments, nextBefore: hasMore ? (payments.at(-1)?.id ?? null) : null };
}
