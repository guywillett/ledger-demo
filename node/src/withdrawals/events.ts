import { enqueue } from '../outbox/outbox.ts';
import type { Tx } from '../shared/db.ts';
import { SUBJECTS, type WithdrawalHeldV1Wire } from '../shared/events.ts';
import { newUuid, toTypeId, type TypeIdString, type Uuid } from '../shared/ids.ts';
import type { Withdrawal } from './withdrawals.ts';

// The events withdrawals produce: only withdrawal.held so far (the submitter consumes it). They
// are written to the outbox in the transaction that creates the withdrawal, and the relay
// publishes them (src/relay/), to the WITHDRAWALS stream.

// The Nats-Msg-Id: one per withdrawal and status, as for payments, so the stream drops a repeat
// publish of the same outbox row within its duplicate window.
export function withdrawalMessageId(withdrawal: Withdrawal, event: 'held'): string {
  return `${toTypeId('wd', withdrawal.id)}.${event}`;
}

// IDs, amount and currency only: no IBAN, name or remittance text (see shared/events.ts).
export function withdrawalHeldEvent(
  withdrawal: Withdrawal,
  eventId: TypeIdString<'evt'>,
): WithdrawalHeldV1Wire {
  return {
    event_id: eventId,
    event_type: 'withdrawal.held',
    schema_version: 1,
    withdrawal_id: toTypeId('wd', withdrawal.id),
    account_id: toTypeId('acct', withdrawal.accountId),
    amount_minor: withdrawal.amountMinor.toString(),
    currency: withdrawal.currency,
    // When the withdrawal entered `held`: it was created held.
    occurred_at: withdrawal.createdAt.toISOString(),
  };
}

// Stores withdrawal.held in the outbox, in the transaction that created the withdrawal. Returns
// the event's ID (the outbox row's ID), for logging.
export async function enqueueWithdrawalHeld(tx: Tx, withdrawal: Withdrawal): Promise<Uuid<'evt'>> {
  const id = newUuid('evt');
  await enqueue(tx, {
    id,
    entityType: 'withdrawal',
    entityId: withdrawal.id,
    subject: SUBJECTS.withdrawalHeld,
    msgId: withdrawalMessageId(withdrawal, 'held'),
    payload: withdrawalHeldEvent(withdrawal, toTypeId('evt', id)),
  });
  return id;
}
