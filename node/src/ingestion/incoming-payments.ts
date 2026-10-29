import type { Queryable, Tx } from '../shared/db.ts';
import type { Iban } from '../shared/iban.ts';
import { newUuid, type Uuid } from '../shared/ids.ts';
import {
  depositFromRow,
  unmatchedDepositFromRow,
  type Deposit,
  type DepositRow,
  type UnmatchedDeposit,
} from '../deposits/deposits.ts';
import { enqueueDepositCreated } from './events.ts';

// Ingestion: records provider notifications as deposits, and a new deposit's deposit.created
// event in the outbox. This is the edge where provider identifiers (IBANs, provider account IDs)
// are mapped to our account; nothing downstream sees them. It never touches balances.
//
// Nothing is refused (the webhook contract, slice 2 decisions 37-38): a notification for an IBAN
// that matches none of our accounts is recorded in suspense (status unmatched), and one that
// conflicts with the stored deposit is recorded as a conflict. Both change no balance.

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
  readonly depositId: Uuid<'dep'>;
  readonly creditorIban: Iban;
  readonly amountMinor: bigint;
  readonly currency: string;
};

export type RecordResult =
  // eventId: the deposit.created event stored in the outbox with it.
  | { readonly kind: 'created'; readonly deposit: Deposit; readonly eventId: Uuid<'evt'> }
  // The same notification again, for a matched or an unmatched deposit: counted.
  | { readonly kind: 'duplicate'; readonly deposit: Deposit | UnmatchedDeposit }
  // A new notification for an IBAN that matches none of our accounts: recorded in suspense.
  | { readonly kind: 'unmatched'; readonly deposit: UnmatchedDeposit }
  // The network ID is stored with other details: recorded in deposit_notification_conflicts.
  | {
      readonly kind: 'conflict';
      readonly differingFields: readonly ComparedField[];
      readonly stored: StoredDetails;
    }
  // The statement refused to record the notification, a deposit with this network ID is
  // stored, yet no compared field differs. Not a client error: either the SQL rule and
  // compare() have drifted apart, or an account's IBAN changed (which must never happen).
  | { readonly kind: 'inconsistent'; readonly stored: StoredDetails };

// One statement records the notification: match the IBAN to our account (or none) and insert the
// deposit, created or unmatched, with its first history row; or recognise a duplicate (same
// details: count it). It returns the deposit in both cases, and no row when nothing was
// recorded: the network ID is already used with different details (conflict). The account is a
// LEFT JOIN, so an unknown IBAN still inserts, with account_id NULL and status unmatched.
//
// Postgres 18: RETURNING can reference old and new. old is NULL for a row that was just
// inserted, so (old.id IS NULL) means "inserted" (older examples use the xmax = 0 trick).
// Data-modifying CTEs always run to completion, so `history` runs although nothing reads it.
const RECORD_NOTIFICATION_SQL = `
WITH upsert AS (
  INSERT INTO deposits (id, creditor_iban, provider_account_id, account_id,
                                 amount_minor, currency, network, network_transaction_id, status)
  SELECT $1, $2, $3, a.id, $4, $5, $6, $7,
         CASE WHEN a.id IS NULL THEN 'unmatched' ELSE 'created' END
  FROM (SELECT $2::text AS iban) n
  LEFT JOIN accounts a ON a.iban = n.iban
  ON CONFLICT (network, network_transaction_id) DO UPDATE
    SET notification_count = deposits.notification_count + 1,
        updated_at = now()
    WHERE (deposits.creditor_iban, deposits.amount_minor,
           deposits.currency)
        = (EXCLUDED.creditor_iban, EXCLUDED.amount_minor, EXCLUDED.currency)
  RETURNING new.*, (old.id IS NULL) AS inserted
),
history AS (
  INSERT INTO deposit_status_history (deposit_id, from_status, to_status)
  SELECT id, NULL, status FROM upsert WHERE inserted
)
SELECT * FROM upsert
`;

// When nothing was recorded, a read of the stored deposit gives the details to compare (conflict,
// or, if none differs, our inconsistency). The decision not to record was already made,
// atomically, by the statement above; this read only classifies it.
//
// Concurrency: every notification is inserted (matched or not), so ON CONFLICT always decides.
// If another transaction is inserting the same network ID, Postgres waits for it and checks
// against the committed row, so the outcome is exact (same details -> duplicate, different ->
// conflict, exactly one caller creates), and the read below always finds the conflicting row:
// it is committed, and deposits are never deleted. Under READ COMMITTED the read, a new
// statement, sees it.
const STORED_DETAILS_SQL = `
SELECT id, creditor_iban, amount_minor, currency
FROM deposits
WHERE network = $1 AND network_transaction_id = $2
`;

const RECORD_CONFLICT_SQL = `
INSERT INTO deposit_notification_conflicts (deposit_id, creditor_iban, provider_account_id,
                                            amount_minor, currency, differing_fields)
VALUES ($1, $2, $3, $4, $5, $6)
`;

// In the caller's transaction: the upsert, then, only when it created a matched deposit, the
// deposit.created event in the outbox (unmatched and duplicate notifications store no event),
// or, for a conflict, the conflict record. A Tx, because each must commit with the upsert.
export async function recordNotification(
  tx: Tx,
  notification: Notification,
): Promise<RecordResult> {
  const result = await tx.query<DepositRow & { inserted: boolean }>(RECORD_NOTIFICATION_SQL, [
    newUuid('dep'),
    notification.creditorIban,
    notification.providerAccountId,
    notification.amountMinor,
    notification.currency,
    notification.network,
    notification.networkTransactionId,
  ]);
  const row = result.rows[0];
  if (row !== undefined) {
    if (row.status === 'unmatched') {
      const deposit = unmatchedDepositFromRow(row);
      return row.inserted ? { kind: 'unmatched', deposit } : { kind: 'duplicate', deposit };
    }
    const deposit = depositFromRow(row);
    if (!row.inserted) return { kind: 'duplicate', deposit };
    const eventId = await enqueueDepositCreated(tx, deposit);
    return { kind: 'created', deposit, eventId };
  }

  const stored = await readStoredDetails(tx, notification);
  if (stored === undefined) {
    // ON CONFLICT found a committed deposit with this network ID, and deposits are never deleted.
    throw new Error(
      `notification ${notification.networkTransactionId} was refused, but no deposit holds it`,
    );
  }
  const differingFields = differingDepositFields(stored, notification);
  if (differingFields.length === 0) {
    return { kind: 'inconsistent', stored };
  }
  await tx.query(RECORD_CONFLICT_SQL, [
    stored.depositId,
    notification.creditorIban,
    notification.providerAccountId,
    notification.amountMinor,
    notification.currency,
    differingFields,
  ]);
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
    depositId: row.id as Uuid<'dep'>,
    creditorIban: row.creditor_iban as Iban,
    amountMinor: row.amount_minor,
    currency: row.currency,
  };
}

// The fields in which a repeated notification differs from the stored deposit ([] = a duplicate).
// Must compare exactly the fields the upsert's ON CONFLICT … WHERE compares. Exported for the
// tests and the shared vectors (contracts/vectors/deposit-same-details.json), which pin the rule
// for both implementations.
export function differingDepositFields(
  stored: Pick<StoredDetails, 'creditorIban' | 'amountMinor' | 'currency'>,
  notification: Pick<Notification, 'creditorIban' | 'amountMinor' | 'currency'>,
): ComparedField[] {
  const fields: ComparedField[] = [];
  if (stored.creditorIban !== notification.creditorIban) fields.push('creditor_iban');
  if (stored.amountMinor !== notification.amountMinor) fields.push('amount_minor');
  if (stored.currency !== notification.currency) fields.push('currency');
  return fields;
}
