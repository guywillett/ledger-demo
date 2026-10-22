import { enqueue } from '../outbox/outbox.ts';
import type { Tx } from '../shared/db.ts';
import { SUBJECTS, type PaymentCreatedV1Wire } from '../shared/events.ts';
import { newUuid, toTypeId, type TypeIdString, type Uuid } from '../shared/ids.ts';
import type { Payment } from '../payments/payments.ts';

// The events ingestion produces: only payment.created. They are written to the outbox in the
// transaction that records the payment, and the relay publishes them (src/relay/), so the
// payment and its event commit together or not at all.

// The Nats-Msg-Id for an event: one per payment and event type, not the event_id. If the relay
// publishes the same outbox row twice (it crashed after publishing but before marking it), the
// stream acknowledges the repeat as a duplicate and doesn't store it again, within its duplicate
// window. Beyond the window, consumers' idempotency on payment state handles repeats.
export function paymentMessageId(payment: Payment, event: 'created'): string {
  return `${toTypeId('pay', payment.id)}.${event}`;
}

export function paymentCreatedEvent(
  payment: Payment,
  eventId: TypeIdString<'evt'>,
): PaymentCreatedV1Wire {
  return {
    event_id: eventId,
    event_type: 'payment.created',
    schema_version: 1,
    payment_id: toTypeId('pay', payment.id),
    network: payment.network,
    network_transaction_id: payment.networkTransactionId,
    account_id: toTypeId('acct', payment.accountId),
    amount_minor: payment.amountMinor.toString(),
    currency: payment.currency,
    // When the payment entered `created`.
    occurred_at: payment.createdAt.toISOString(),
  };
}

// Stores payment.created in the outbox, in the transaction that created the payment. Returns the
// event's ID (the outbox row's ID), for logging.
export async function enqueuePaymentCreated(tx: Tx, payment: Payment): Promise<Uuid<'evt'>> {
  const id = newUuid('evt');
  await enqueue(tx, {
    id,
    entityType: 'payment',
    entityId: payment.id,
    subject: SUBJECTS.paymentCreated,
    msgId: paymentMessageId(payment, 'created'),
    payload: paymentCreatedEvent(payment, toTypeId('evt', id)),
  });
  return id;
}
