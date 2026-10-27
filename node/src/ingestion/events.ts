import { enqueue } from '../outbox/outbox.ts';
import type { Tx } from '../shared/db.ts';
import { SUBJECTS, type DepositCreatedV1Wire } from '../shared/events.ts';
import { newUuid, toTypeId, type TypeIdString, type Uuid } from '../shared/ids.ts';
import type { Deposit } from '../deposits/deposits.ts';

// The events ingestion produces: only deposit.created. They are written to the outbox in the
// transaction that records the deposit, and the relay publishes them (src/relay/), so the
// deposit and its event commit together or not at all.

// The Nats-Msg-Id for an event: one per deposit and event type, not the event_id. If the relay
// publishes the same outbox row twice (it crashed after publishing but before marking it), the
// stream acknowledges the repeat as a duplicate and doesn't store it again, within its duplicate
// window. Beyond the window, consumers' idempotency on deposit state handles repeats.
export function depositMessageId(deposit: Deposit, event: 'created'): string {
  return `${toTypeId('dep', deposit.id)}.${event}`;
}

export function depositCreatedEvent(
  deposit: Deposit,
  eventId: TypeIdString<'evt'>,
): DepositCreatedV1Wire {
  return {
    event_id: eventId,
    event_type: 'deposit.created',
    schema_version: 1,
    deposit_id: toTypeId('dep', deposit.id),
    network: deposit.network,
    network_transaction_id: deposit.networkTransactionId,
    account_id: toTypeId('acct', deposit.accountId),
    amount_minor: deposit.amountMinor.toString(),
    currency: deposit.currency,
    // When the deposit entered `created`.
    occurred_at: deposit.createdAt.toISOString(),
  };
}

// Stores deposit.created in the outbox, in the transaction that created the deposit. Returns the
// event's ID (the outbox row's ID), for logging.
export async function enqueueDepositCreated(tx: Tx, deposit: Deposit): Promise<Uuid<'evt'>> {
  const id = newUuid('evt');
  await enqueue(tx, {
    id,
    entityType: 'deposit',
    entityId: deposit.id,
    subject: SUBJECTS.depositCreated,
    msgId: depositMessageId(deposit, 'created'),
    payload: depositCreatedEvent(deposit, toTypeId('evt', id)),
  });
  return id;
}
