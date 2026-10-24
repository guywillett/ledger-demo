import type { Queryable } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import type { Uuid } from '../shared/ids.ts';
import { parseStatus, type PaymentStatus } from './status.ts';

// The payment entity and how it is read. Its lifecycle and the transitions that move it
// along are in status.ts; recording a provider notification as a new payment is in
// ingestion/incoming-payments.ts.

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

export type PaymentRow = {
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

export function paymentFromRow(row: PaymentRow): Payment {
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

const EXISTS_SQL = 'SELECT 1 FROM incoming_payments WHERE id = $1';

// For classifying an outcome after the fact (e.g. an event whose transitions all skipped:
// already processed, or a payment that doesn't exist). Never used to decide whether to act.
export async function paymentExists(db: Queryable, paymentId: Uuid<'pay'>): Promise<boolean> {
  const result = await db.query(EXISTS_SQL, [paymentId]);
  return result.rowCount === 1;
}
