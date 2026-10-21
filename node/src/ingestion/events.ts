import { SUBJECTS, type PaymentCreatedV1 } from '../shared/events.ts';
import { newUuid, toTypeId, type TypeIdString } from '../shared/ids.ts';
import type { EventPublisher, PublishResult } from '../shared/nats.ts';
import type { Payment } from './payments.ts';

// The events ingestion produces. Only payment.created in slice 1.

export type PublishedEvent = PublishResult & { readonly eventId: TypeIdString<'evt'> };

// A failed publish, carrying the event_id it was for: a publish that timed out may still have
// been stored, so logs need the ID to match the two up.
export class PublishFailedError extends Error {
  readonly eventId: TypeIdString<'evt'>;

  constructor(eventId: TypeIdString<'evt'>, cause: unknown) {
    super(`publishing event ${eventId} failed`, { cause });
    this.name = 'PublishFailedError';
    this.eventId = eventId;
  }
}

// The Nats-Msg-Id for an event: one per payment and event type, not the random event_id. A
// repeat of the same announcement within the stream's duplicate window (a publish that timed
// out but was stored, then retried; provider retries while the worker is slow) is then
// acknowledged as a duplicate and not stored again. Beyond the window, consumers' idempotency
// on payment state handles repeats.
export function paymentMessageId(payment: Payment, event: 'created'): string {
  return `${toTypeId('pay', payment.id)}.${event}`;
}

// The domain layer: which events ingestion announces, and what they contain. Built by
// paymentEvents() below on top of an EventPublisher (the transport), so the route only ever
// says "announce this payment" and never sees NATS.
export type PaymentEvents = {
  paymentCreated(payment: Payment): Promise<PublishedEvent>;
};

export function paymentCreatedEvent(
  payment: Payment,
  eventId: TypeIdString<'evt'>,
): PaymentCreatedV1 {
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

// A factory, not a class: the returned methods close over `publisher` (they keep a reference
// to the one passed in here), which is dependency injection without a constructor. Calling
// paymentEvents() publishes nothing; each later paymentCreated(payment) call builds the event
// and publishes it through that publisher.
//
// Each call builds a new event with a new event_id, also when it re-announces a payment whose
// earlier publish failed: without an outbox, event IDs aren't stored. If the stream already
// holds this payment's payment.created (same message ID, within the duplicate window), the
// result has duplicate: true and the stored event keeps its original event_id.
export function paymentEvents(publisher: EventPublisher): PaymentEvents {
  return {
    async paymentCreated(payment) {
      const eventId = toTypeId('evt', newUuid('evt'));
      const event = paymentCreatedEvent(payment, eventId);
      try {
        const result = await publisher.publish(
          SUBJECTS.paymentCreated,
          event,
          paymentMessageId(payment, 'created'),
        );
        return { ...result, eventId };
      } catch (err) {
        throw new PublishFailedError(eventId, err);
      }
    },
  };
}
