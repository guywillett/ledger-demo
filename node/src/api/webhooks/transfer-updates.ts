import type pg from 'pg';
import { recordTransferUpdate, type UpdateResult } from '../../ingestion/transfer-updates.ts';
import { isInvariantViolation } from '../../ledger/invariants.ts';
import { withTransaction } from '../../shared/db.ts';
import { toTypeId } from '../../shared/ids.ts';
import { problems, sendProblem, validationErrors } from '../problem.ts';
import { toTransferUpdate, transferUpdateBodySchema } from './schemas.ts';
import type { App } from '../types.ts';

// POST /provider/transfer-updates: the provider's result for a transfer we sent (its webhook).
// Recorded and settled in one transaction (ingestion/transfer-updates.ts).
//
// The real webhook contract (slice 2 decision 37): a provider retries anything but a 2xx, so
// - 204, no body, for everything recorded or deliberately handled: applied, duplicate,
//   conflict, event conflict, unmatched. No outcome in the response: echoing it would let any
//   caller probe which withdrawals exist and their state, and providers ignore webhook bodies.
// - 400 only for a body that doesn't parse: if the bug is ours, the provider's retries deliver
//   the update once it's fixed.
// - 500 when a retry can help once something is fixed: an invariant violation (rolled back,
//   alerted) or a transient failure.
// Contradictions and unmatched updates are logged as errors with alert: true: a person should
// look, but answering non-2xx would only make the provider resend them.
export function registerTransferUpdateRoutes(app: App, deps: { pool: pg.Pool }): void {
  app.post('/provider/transfer-updates', async (request, reply) => {
    const parsed = transferUpdateBodySchema.safeParse(request.body);
    if (!parsed.success) {
      const errors = validationErrors(parsed.error);
      request.log.warn({ errors }, 'invalid transfer update');
      return sendProblem(reply, problems.invalidTransferUpdate(errors));
    }

    const update = toTransferUpdate(parsed.data);
    const log = request.log.child({
      provider_event_id: update.providerEventId,
      transfer_id: update.transferId,
      status: update.status,
      ...(update.withdrawalId === null
        ? {}
        : { withdrawal_id: toTypeId('wd', update.withdrawalId) }),
    });

    let result: UpdateResult;
    try {
      result = await withTransaction(deps.pool, (tx) => recordTransferUpdate(tx, update));
    } catch (err) {
      if (isInvariantViolation(err)) {
        // A ledger bug: rolled back, nothing recorded. 500, so the provider keeps resending it
        // (and it stays visible) until someone has looked.
        log.fatal({ err, alert: true }, 'invariant violated applying a transfer update');
        return sendProblem(reply, problems.http(500));
      }
      throw err;
    }

    switch (result.kind) {
      case 'applied':
        log.info('transfer update applied');
        break;
      case 'duplicate':
        log.info({ notification_count: result.notificationCount }, 'duplicate transfer update');
        break;
      case 'conflict':
        log.error(
          {
            alert: true,
            stored_status: result.stored.status,
            stored_transfer_id: result.stored.transferId,
          },
          'transfer update contradicts the withdrawal; recorded, nothing moved',
        );
        break;
      case 'event_conflict':
        log.error(
          { alert: true },
          'provider reused an event ID with other details; nothing changed',
        );
        break;
      case 'unmatched':
        log.error(
          { alert: true, reference: result.reference },
          'transfer update matches no withdrawal; recorded',
        );
        break;
    }
    return reply.code(204).send();
  });
}
