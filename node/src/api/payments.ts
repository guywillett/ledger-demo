import { recordNotification } from '../ingestion/payments.ts';
import type { Queryable } from '../shared/db.ts';
import { maskIban } from '../shared/iban.ts';
import { toTypeId } from '../shared/ids.ts';
import { httpProblem, PROBLEM_TYPES, sendProblem } from './problem.ts';
import { notificationBodySchema, toNotification, toPaymentResponse } from './schemas.ts';
import type { App } from './types.ts';

// POST /payments: an inbound notification from the network / BaaS provider (webhook-style).
//
// Status codes are conventional API codes for the demo (201, 200, 400, 409, 422). A real
// provider webhook would answer 2xx for anything recorded or deliberately handled and 5xx only
// when a retry could help; see "Webhook semantics" in docs/slices/slice-1-incoming-payments.md.
export function registerPaymentRoutes(app: App, deps: { db: Queryable }): void {
  app.post('/payments', async (request, reply) => {
    const parsed = notificationBodySchema.safeParse(request.body);
    if (!parsed.success) {
      // Paths and messages only: never the input values, which come from the request body.
      const errors = parsed.error.issues.map((issue) => ({
        path: issue.path.map(String).join('.'),
        message: issue.message,
      }));
      request.log.warn({ errors }, 'invalid payment notification');
      return sendProblem(reply, {
        type: PROBLEM_TYPES.invalidNotification,
        title: 'Invalid payment notification',
        status: 400,
        errors,
      });
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
        log.info({ ...context, amount_minor: notification.amountMinor }, 'payment created');
        // TODO (slice 1, next step): publish payment.created to JetStream (Nats-Msg-Id =
        // event_id) now that the insert has committed. Until then nothing books the payment.
        return reply.code(201).send(toPaymentResponse(result.payment));
      }
      case 'duplicate': {
        const log = request.log.child({ payment_id: toTypeId('pay', result.payment.id) });
        log.info(
          { ...context, duplicate: true, notification_count: result.payment.notificationCount },
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
        return sendProblem(reply, {
          type: PROBLEM_TYPES.notificationConflict,
          title: 'Notification conflicts with a stored payment',
          status: 409,
          detail:
            'A payment with this network transaction ID exists with different details. ' +
            'This is not treated as a retry.',
          differing_fields: result.differingFields,
        });
      }
      case 'unknown_account': {
        request.log.warn(context, 'payment notification for an unknown account');
        return sendProblem(reply, {
          type: PROBLEM_TYPES.unknownAccount,
          title: 'Unknown account',
          status: 422,
          detail: 'The creditor IBAN does not match any account managed by this application.',
        });
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
        return sendProblem(reply, httpProblem(500));
      }
    }
  });
}
