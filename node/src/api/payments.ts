import type { FastifyReply } from 'fastify';
import {
  PublishFailedError,
  type PaymentEvents,
  type PublishedEvent,
} from '../ingestion/events.ts';
import { recordNotification } from '../ingestion/notifications.ts';
import { type Payment } from '../payments/payments.ts';
import type { Queryable } from '../shared/db.ts';
import { maskIban } from '../shared/iban.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import { problems, sendProblem, validationErrors } from './problem.ts';
import { notificationBodySchema, toNotification, toPaymentResponse } from './schemas.ts';
import type { App } from './types.ts';

// POST /payments: an inbound notification from the network / BaaS provider (webhook-style).
//
// Status codes are conventional API codes for the demo (201, 200, 400, 409, 422). A real
// provider webhook would answer 2xx for anything recorded or deliberately handled and 5xx only
// when a retry could help; see "Webhook semantics" in docs/slices/slice-1-incoming-payments.md.
//
// After the insert commits, the payment is announced with a payment.created event. If that
// publish fails, the payment stays `created` and the caller gets 503: the provider's retry is
// a duplicate, and a duplicate of a payment still `created` announces it again. That retry is
// what closes the gap until a transactional outbox replaces it.
export function registerPaymentRoutes(
  app: App,
  deps: { db: Queryable; events: PaymentEvents },
): void {
  app.post('/payments', async (request, reply) => {
    const parsed = notificationBodySchema.safeParse(request.body);
    if (!parsed.success) {
      const errors = validationErrors(parsed.error);
      request.log.warn({ errors }, 'invalid payment notification');
      return sendProblem(reply, problems.invalidNotification(errors));
    }

    const notification = toNotification(parsed.data);
    const context = {
      network_transaction_id: notification.networkTransactionId,
      creditor_iban: maskIban(notification.creditorIban),
    };
    const result = await recordNotification(deps.db, notification);

    switch (result.kind) {
      case 'created': {
        const log = request.log.child({ payment_id: toTypeId('pay', result.payment.id) });
        const published = await announce(deps.events, result.payment, log, context);
        if (published === undefined) return sendNotAnnounced(reply);
        log.info(
          { ...context, amount_minor: notification.amountMinor, ...eventFields(published) },
          'payment created',
        );
        return reply.code(201).send(toPaymentResponse(result.payment));
      }
      case 'duplicate': {
        const log = request.log.child({ payment_id: toTypeId('pay', result.payment.id) });
        // Still `created`: an earlier publish may have failed, so announce it again. Once the
        // worker has moved it on, a duplicate has no further effect.
        //
        // The status comes from the upsert, and the publish happens after it, so the worker
        // may book the payment in between and get a payment.created for a payment already
        // `received`. That's no different from a redelivered message: consumers act through
        // conditional transitions (created -> received only if still created), so a stale
        // event is a no-op. A re-announcement within the duplicate window isn't even stored.
        let republished: PublishedEvent | undefined;
        if (result.payment.status === 'created') {
          republished = await announce(deps.events, result.payment, log, context);
          if (republished === undefined) return sendNotAnnounced(reply);
        }
        log.info(
          {
            ...context,
            duplicate: true,
            notification_count: result.payment.notificationCount,
            ...(republished === undefined ? {} : eventFields(republished)),
          },
          'duplicate payment notification',
        );
        return reply.code(200).send(toPaymentResponse(result.payment));
      }
      case 'conflict': {
        const log = request.log.child({ payment_id: toTypeId('pay', result.stored.paymentId) });
        log.error(
          {
            ...context,
            differing_fields: result.differingFields,
            amount_minor: notification.amountMinor,
            stored_amount_minor: result.stored.amountMinor,
            stored_creditor_iban: maskIban(result.stored.creditorIban),
          },
          'payment notification conflicts with the stored payment',
        );
        return sendProblem(reply, problems.notificationConflict(result.differingFields));
      }
      case 'unknown_account': {
        request.log.warn(context, 'payment notification for an unknown account');
        return sendProblem(reply, problems.unknownAccount());
      }
      case 'inconsistent': {
        // Our bug or our data, not the caller's: 500, which also makes a real provider retry,
        // so the problem keeps surfacing until someone looks.
        const log = request.log.child({ payment_id: toTypeId('pay', result.stored.paymentId) });
        log.error(
          context,
          'notification refused but no field differs from the stored payment: ' +
            'duplicate rule drift or a changed account IBAN',
        );
        return sendProblem(reply, problems.http(500));
      }
    }
  });
}

// Publishes payment.created; on failure logs it and returns undefined (the caller answers 503).
// This is where the publish actually happens: events.paymentCreated() builds the event and
// hands it to the EventPublisher that `events` was created with, which in the running API is
// JetStream (src/api/main.ts) and in tests a recording or failing stand-in.
async function announce(
  events: PaymentEvents,
  payment: Payment,
  log: Logger,
  context: Record<string, unknown>,
): Promise<PublishedEvent | undefined> {
  try {
    return await events.paymentCreated(payment);
  } catch (err) {
    // event_id lets this line be matched to the event if the publish timed out but was stored.
    const eventId = err instanceof PublishFailedError ? { event_id: err.eventId } : {};
    log.error(
      { ...context, ...eventId, err },
      'payment recorded but payment.created could not be published',
    );
    return undefined;
  }
}

// Log fields for a publish. When the stream already held this payment's payment.created
// (same message ID within the duplicate window), the new event wasn't stored: its event_id
// would point at nothing, so say so instead.
function eventFields(published: PublishedEvent): Record<string, unknown> {
  return published.duplicate ? { stream_duplicate: true } : { event_id: published.eventId };
}

// The 503 with a Retry-After (a response header, so not part of the problem itself).
function sendNotAnnounced(reply: FastifyReply): FastifyReply {
  return sendProblem(reply.header('retry-after', '5'), problems.notAnnounced());
}
