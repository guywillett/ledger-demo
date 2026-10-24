import { createWithdrawal } from '../ledger/withdrawals.ts';
import type { Tx } from '../shared/db.ts';
import type { Uuid } from '../shared/ids.ts';
import { enqueueWithdrawalHeld } from './events.ts';
import { getWithdrawalByKey, type NewWithdrawal, type Withdrawal } from './withdrawals.ts';

// Client withdrawal requests: turns a request into a result, so the route stays thin. The
// client-facing counterpart of ingestion/incoming-payments.ts (provider notifications).
//
// Idempotency (slice 2 decision 24): the key is a unique column on the withdrawal itself, so
// only a request that commits takes it. Everything that fails rolls back and leaves it free:
// insufficient funds and an unknown account are thrown (InsufficientFundsError from
// ledger/withdrawals.ts, AccountNotFoundError from withdrawals.ts) for the caller to handle
// after the rollback.

// The details that must match for a repeat with the same key to count as the same request.
export type ComparedWithdrawalField =
  'amount_minor' | 'currency' | 'creditor_iban' | 'creditor_name' | 'remittance_info';

export type RequestResult =
  // eventId: the withdrawal.held event stored in the outbox with it.
  | { readonly kind: 'created'; readonly withdrawal: Withdrawal; readonly eventId: Uuid<'evt'> }
  // The same request again: the withdrawal in its current status (not a copy of the first
  // response).
  | { readonly kind: 'repeat'; readonly withdrawal: Withdrawal }
  // The key was used for another request.
  | {
      readonly kind: 'conflict';
      readonly differingFields: readonly ComparedWithdrawalField[];
      readonly stored: Withdrawal;
    };

// In the caller's transaction: create the withdrawal with its funds held and store its
// withdrawal.held event, or, if the account has already used the key, classify the repeat.
//
// The read of the stored withdrawal only classifies: the insert's ON CONFLICT has already
// decided not to insert, and the row it conflicted with is committed (ON CONFLICT waited for any
// transaction still inserting it). Under READ COMMITTED the read, a new statement, sees it.
export async function requestWithdrawal(tx: Tx, request: NewWithdrawal): Promise<RequestResult> {
  const withdrawal = await createWithdrawal(tx, request);
  if (withdrawal !== undefined) {
    const eventId = await enqueueWithdrawalHeld(tx, withdrawal);
    return { kind: 'created', withdrawal, eventId };
  }

  const stored = await getWithdrawalByKey(tx, request.accountId, request.idempotencyKey);
  if (stored === undefined) {
    // ON CONFLICT found a committed row with this key, and withdrawals are never deleted.
    throw new Error(
      `withdrawal key conflict on account ${request.accountId}, but no withdrawal holds the key`,
    );
  }
  const differingFields = compare(stored, request);
  return differingFields.length === 0
    ? { kind: 'repeat', withdrawal: stored }
    : { kind: 'conflict', differingFields, stored };
}

// Field by field, no request hash (as slice 1 decision 16), so Node and Go share one rule.
function compare(stored: Withdrawal, request: NewWithdrawal): ComparedWithdrawalField[] {
  const fields: ComparedWithdrawalField[] = [];
  if (stored.amountMinor !== request.amountMinor) fields.push('amount_minor');
  if (stored.currency !== request.currency) fields.push('currency');
  if (stored.creditorIban !== request.creditorIban) fields.push('creditor_iban');
  if (stored.creditorName !== request.creditorName) fields.push('creditor_name');
  if (stored.remittanceInfo !== request.remittanceInfo) fields.push('remittance_info');
  return fields;
}
