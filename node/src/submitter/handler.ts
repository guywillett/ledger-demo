import type pg from 'pg';
import type { DeliveryOutcome } from '../consumer/deliveries.ts';
import { withDeliveryLog, type DeliveryContext, type Settled } from '../consumer/delivery-log.ts';
import type { DeliveredMessage } from '../consumer/messages.ts';
import { markSubmitted, release } from '../ledger/withdrawals.ts';
import { retryDelay } from '../shared/backoff.ts';
import { withTransaction } from '../shared/db.ts';
import { withdrawalHeldV1Schema } from '../shared/events.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import { TransferIdMismatchError } from '../withdrawals/status.ts';
import { getWithdrawal } from '../withdrawals/withdrawals.ts';
import type { ProviderClient } from './provider.ts';

// Handles one delivery of a withdrawal.held event: sends the withdrawal to the provider, with
// its TypeID as the Idempotency-Key, and records the outcome.
//   accepted     -> held -> submitted, with the provider's transfer ID
//   refused      -> held -> failed, funds released (only a definitive 422 releases funds)
//   unknown      -> nak: tried again later with the same key, never a new one (decision 26)
//   invalid, key conflict, transfer-ID mismatch -> our bug: terminated with an alert, the
//                   withdrawal left held for a person (decision 35)
// The provider call is never inside a database transaction (no transaction waits on the
// network); each recording step is a short transaction of its own afterwards.

export type SubmitterDeps = {
  readonly pool: pg.Pool;
  readonly logger: Logger;
  readonly provider: ProviderClient;
  // After an unknown outcome or a transient failure: nak with retryBaseDelayMs on the first
  // attempt, doubling per attempt, at most retryMaxDelayMs.
  readonly retryBaseDelayMs: number;
  readonly retryMaxDelayMs: number;
};

export function handleSubmission(
  deps: SubmitterDeps,
  msg: DeliveredMessage,
): Promise<DeliveryOutcome> {
  // 1-2. Recording the delivery and parsing the event are the shared delivery lifecycle's
  // (consumer/delivery-log.ts), as is settling the message and recording the outcome. Our bugs
  // are recognised here (bug() below), so any other error is transient.
  return withDeliveryLog(
    {
      consumer: 'submitter',
      eventType: 'withdrawal.held',
      schema: withdrawalHeldV1Schema,
      pool: deps.pool,
      logger: deps.logger,
      retry: deps,
      isBug: () => false,
      // E.g. the database went away after an accepted call. The redelivery calls again with
      // the same key, gets the same transfer, and records it.
      transientFailureMessage: 'submission failed; will retry',
    },
    msg,
    async (event, ctx) => {
      const withdrawalId = event.withdrawal_id;
      ctx.bind({ withdrawal_id: toTypeId('wd', withdrawalId) });

      // 3. Read the withdrawal: the request is built from what is stored, not from the event.
      const withdrawal = await getWithdrawal(deps.pool, withdrawalId);
      if (withdrawal === undefined) {
        ctx.log.error('withdrawal.held event for an unknown withdrawal; terminating it');
        return ctx.stop('invalid', 'unknown withdrawal');
      }
      // Already submitted or settled (e.g. the provider's update overtook us, decision 27): no
      // call needed.
      //
      // A deliberate exception to "no check-then-act" (CLAUDE.md): this read decides whether to
      // make the provider call. That rule prevents races where acting on a stale read changes
      // state wrongly. Here a stale read can only cause one extra call, which changes nothing:
      // the provider answers our repeated key with the same transfer, and markSubmitted's
      // conditional transition skips. Money and status are still decided only by the conditional
      // transitions; the read exists to avoid a needless provider call.
      if (withdrawal.status !== 'held') {
        ctx.log.info({ status: withdrawal.status }, 'withdrawal no longer held; not submitting');
        return ctx.done('duplicate');
      }

      // 4. The call, outside any transaction. working() first resets the message's ack_wait, so
      // the call (at most PROVIDER_TIMEOUT_MS, well under ack_wait) starts with the full lease,
      // however long the database steps before it took.
      ctx.message.working();
      const result = await deps.provider.submitTransfer(withdrawal);

      // 5. Record the outcome.
      switch (result.kind) {
        case 'accepted': {
          let recorded: 'applied' | 'skipped';
          try {
            recorded = await withTransaction(deps.pool, (tx) =>
              markSubmitted(tx, withdrawalId, result.transferId),
            );
          } catch (err) {
            if (!(err instanceof TransferIdMismatchError)) throw err;
            // The provider's update stored another transfer ID than this submission got.
            return bug(
              ctx,
              {
                stored_transfer_id: err.storedTransferId,
                reported_transfer_id: err.reportedTransferId,
              },
              'provider transfer ID contradicts the stored one',
            );
          }
          ctx.log.info(
            { transfer_id: result.transferId, submitted: recorded },
            recorded === 'applied' ? 'withdrawal submitted' : 'withdrawal already moved on',
          );
          return ctx.done(recorded === 'applied' ? 'applied' : 'duplicate');
        }
        case 'refused': {
          const released = await withTransaction(deps.pool, (tx) =>
            release(tx, withdrawalId, result.reason),
          );
          ctx.log.info(
            { reason: result.reason, released },
            released === 'applied'
              ? 'withdrawal refused by the provider; funds released'
              : 'withdrawal refused by the provider, but already settled',
          );
          return ctx.done(released === 'applied' ? 'applied' : 'duplicate');
        }
        case 'unknown': {
          // The transfer may or may not exist at the provider. The withdrawal stays held, funds
          // reserved, which is correct either way; the redelivery asks again with the same key.
          const delayMs = retryDelay(deps, msg.deliveryAttempt);
          ctx.log.warn(
            { cause: result.cause, retry_in_ms: delayMs },
            'provider outcome unknown; will retry',
          );
          return ctx.retryLater(delayMs);
        }
        case 'invalid':
          return bug(
            ctx,
            // The status and the summary only; never the provider's body (see SubmitOutcome).
            {
              provider_status: 400,
              provider_error_fields: result.fields,
              provider_body_bytes: result.bodyBytes,
            },
            'provider rejected our request as invalid',
          );
        case 'key_conflict':
          return bug(
            ctx,
            { provider_status: 409 },
            'provider reports our idempotency key reused with another request',
          );
      }
    },
  );
}

// Our bug, not a transient failure: retrying can't fix it, and nothing may move money on it.
// Terminated with an alert, whether or not the outcome can be recorded; the withdrawal stays
// held (funds reserved) for a person to resolve, and reconciliation lists it as stuck.
function bug(ctx: DeliveryContext, fields: Record<string, unknown>, why: string): Promise<Settled> {
  ctx.log.fatal(
    { ...fields, alert: true },
    `${why}; terminating the message, withdrawal left held`,
  );
  return ctx.stop('failed', why);
}
