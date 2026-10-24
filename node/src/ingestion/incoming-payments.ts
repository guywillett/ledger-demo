import type { Queryable, Tx } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import { newUuid, type Uuid } from '../shared/ids.ts';
import { paymentFromRow, type Payment, type PaymentRow } from '../payments/payments.ts';
import { enqueuePaymentCreated } from './events.ts';

// Ingestion: records provider notifications as payments, and a new payment's payment.created
// event in the outbox. This is the edge where provider identifiers (IBANs, provider account IDs)
// are mapped to our account; nothing downstream sees them. It never touches balances.

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
  // eventId: the payment.created event stored in the outbox with it.
  | { readonly kind: 'created'; readonly payment: Payment; readonly eventId: Uuid<'evt'> }
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
// after the statement with a fresh snapshot (also inside the caller's transaction: under READ
// COMMITTED each statement takes its own), so a competing insert committed in the meantime
// is seen (conflict). One that has not committed yet when the read runs gives unknown_account:
// at that moment no payment with this network ID is stored, so that answer is accurate.
const STORED_DETAILS_SQL = `
SELECT id, creditor_iban, amount_minor, currency
FROM incoming_payments
WHERE network = $1 AND network_transaction_id = $2
`;

// Two statements in the caller's transaction: the upsert, then, only when it created the payment,
// the payment.created event in the outbox. Duplicates, conflicts and unknown accounts store no
// event. A Tx, because the payment and its event must commit together.
export async function recordNotification(
  tx: Tx,
  notification: Notification,
): Promise<RecordResult> {
  const result = await tx.query<PaymentRow & { inserted: boolean }>(RECORD_NOTIFICATION_SQL, [
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
    if (!row.inserted) return { kind: 'duplicate', payment };
    const eventId = await enqueuePaymentCreated(tx, payment);
    return { kind: 'created', payment, eventId };
  }

  const stored = await readStoredDetails(tx, notification);
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
