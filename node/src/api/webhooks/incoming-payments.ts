import type pg from 'pg';
import { recordNotification, type RecordResult } from '../../ingestion/incoming-payments.ts';
import { withTransaction } from '../../shared/db.ts';
import { maskIban } from '../../shared/iban.ts';
import { toTypeId } from '../../shared/ids.ts';
import { problems, sendProblem, validationErrors } from '../problem.ts';
import { notificationBodySchema, toNotification } from './schemas.ts';
import type { App } from '../types.ts';

// POST /provider/incoming-payments: the provider's notification of an incoming payment (its
// webhook). A new deposit and its deposit.created event are stored in one transaction (the event
// in the outbox, published by the relay); this request never talks to NATS.
//
// The real webhook contract (slice 2 decisions 37-38, revising slice 1 decision 13): a provider
// retries anything but a 2xx, so
// - 204, no body, for everything recorded or deliberately handled: created, duplicate, unmatched
//   (recorded in suspense: money can't be refused over HTTP) and conflict (recorded). No outcome
//   in the response: the endpoint is no oracle for which accounts or deposits exist.
// - 400 only for a body that doesn't parse, an invalid IBAN included: if our validation is the
//   problem (the SEPA country list isn't verified yet), the provider's retries deliver the
//   notification once it's fixed.
// - 500 when a retry can help once something is fixed: our inconsistency, or a transient failure.
// Unmatched and conflicting notifications are logged as errors with alert: true: a person
// should look, but a non-2xx would only make the provider resend them.
export function registerIncomingPaymentRoutes(app: App, deps: { pool: pg.Pool }): void {
  app.post('/provider/incoming-payments', async (request, reply) => {
    const parsed = notificationBodySchema.safeParse(request.body);
    if (!parsed.success) {
      const errors = validationErrors(parsed.error);
      request.log.warn({ errors }, 'invalid deposit notification');
      return sendProblem(reply, problems.invalidNotification(errors));
    }

    const notification = toNotification(parsed.data);
    const context = {
      network_transaction_id: notification.networkTransactionId,
      creditor_iban: maskIban(notification.creditorIban),
    };
    const result: RecordResult = await withTransaction(deps.pool, (tx) =>
      recordNotification(tx, notification),
    );

    switch (result.kind) {
      case 'created': {
        const log = request.log.child({ deposit_id: toTypeId('dep', result.deposit.id) });
        log.info(
          {
            ...context,
            amount_minor: notification.amountMinor,
            event_id: toTypeId('evt', result.eventId),
          },
          'deposit created',
        );
        break;
      }
      case 'duplicate': {
        const log = request.log.child({ deposit_id: toTypeId('dep', result.deposit.id) });
        log.info(
          {
            ...context,
            duplicate: true,
            status: result.deposit.status,
            notification_count: result.deposit.notificationCount,
          },
          'duplicate deposit notification',
        );
        break;
      }
      case 'unmatched': {
        const log = request.log.child({ deposit_id: toTypeId('dep', result.deposit.id) });
        log.error(
          { ...context, alert: true, amount_minor: notification.amountMinor },
          'deposit notification matches none of our accounts; recorded in suspense',
        );
        break;
      }
      case 'conflict': {
        const log = request.log.child({ deposit_id: toTypeId('dep', result.stored.depositId) });
        // Field names only, never the values (received or stored).
        log.error(
          { ...context, alert: true, differing_fields: result.differingFields },
          'deposit notification conflicts with the stored deposit; recorded, nothing changed',
        );
        break;
      }
      case 'inconsistent': {
        // Our bug or our data, not the provider's: 500, which also makes the provider retry, so
        // the problem keeps surfacing until someone looks.
        const log = request.log.child({ deposit_id: toTypeId('dep', result.stored.depositId) });
        log.error(
          { ...context, alert: true },
          'notification refused but no field differs from the stored deposit: ' +
            'duplicate rule drift or a changed account IBAN',
        );
        return sendProblem(reply, problems.http(500));
      }
    }
    return reply.code(204).send();
  });
}
