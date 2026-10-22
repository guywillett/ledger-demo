import type pg from 'pg';
import { recordNotification } from '../ingestion/notifications.ts';
import { withTransaction } from '../shared/db.ts';
import { maskIban } from '../shared/iban.ts';
import { toTypeId } from '../shared/ids.ts';
import { problems, sendProblem, validationErrors } from './problem.ts';
import { notificationBodySchema, toNotification, toPaymentResponse } from './schemas.ts';
import type { App } from './types.ts';

// POST /payments: an inbound notification from the network / BaaS provider (webhook-style).
//
// Status codes are conventional API codes for the demo (201, 200, 400, 409, 422). A real
// provider webhook would answer 2xx for anything recorded or deliberately handled and 5xx only
// when a retry could help; see "Webhook semantics" in docs/slices/slice-1-incoming-payments.md.
//
// A new payment and its payment.created event are stored in one transaction (the event in the
// outbox), so they commit together or not at all. The relay (src/relay/) publishes the event;
// this request never talks to NATS. A duplicate is just a duplicate: its event was stored with
// the payment.
export function registerPaymentRoutes(app: App, deps: { pool: pg.Pool }): void {
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
    const result = await withTransaction(deps.pool, (tx) => recordNotification(tx, notification));

    switch (result.kind) {
      case 'created': {
        const log = request.log.child({ payment_id: toTypeId('pay', result.payment.id) });
        log.info(
          {
            ...context,
            amount_minor: notification.amountMinor,
            event_id: toTypeId('evt', result.eventId),
          },
          'payment created',
        );
        return reply.code(201).send(toPaymentResponse(result.payment));
      }
      case 'duplicate': {
        const log = request.log.child({ payment_id: toTypeId('pay', result.payment.id) });
        log.info(
          {
            ...context,
            duplicate: true,
            notification_count: result.payment.notificationCount,
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
