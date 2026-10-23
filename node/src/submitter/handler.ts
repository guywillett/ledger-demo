import type pg from 'pg';
import {
  finishDelivery,
  startDelivery,
  type DeliveryId,
  type DeliveryOutcome,
} from '../consumer/deliveries.ts';
import { parseEvent, peekEventId, type DeliveredMessage } from '../consumer/messages.ts';
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

export async function handleSubmission(
  deps: SubmitterDeps,
  msg: DeliveredMessage,
): Promise<DeliveryOutcome> {
  // 1. Log the delivery before anything else, with the event_id if it can be read at all.
  const peekedEventId = peekEventId(msg.body);
  let log = deps.logger.child({
    nats_msg_id: msg.natsMsgId,
    delivery_attempt: msg.deliveryAttempt,
    ...(peekedEventId === null ? {} : { event_id: toTypeId('evt', peekedEventId) }),
  });

  let deliveryId: DeliveryId;
  try {
    deliveryId = await startDelivery(deps.pool, {
      consumer: 'submitter',
      eventId: peekedEventId,
      natsMsgId: msg.natsMsgId,
      deliveryAttempt: msg.deliveryAttempt,
    });
  } catch (err) {
    log.error({ err }, 'could not record delivery; will retry');
    msg.nak(retryDelay(deps, msg.deliveryAttempt));
    return 'failed';
  }

  // Signal vs record (consumer/messages.ts): record, then ack; term or nak, then record.
  const finish = async (outcome: DeliveryOutcome): Promise<DeliveryOutcome> => {
    await finishDelivery(deps.pool, deliveryId, outcome);
    return outcome;
  };
  const record = async (outcome: DeliveryOutcome): Promise<DeliveryOutcome> => {
    try {
      await finishDelivery(deps.pool, deliveryId, outcome);
    } catch (err) {
      log.error({ err, outcome }, 'could not record the delivery outcome');
    }
    return outcome;
  };
  const stop = (outcome: DeliveryOutcome, reason: string): Promise<DeliveryOutcome> => {
    msg.term(reason);
    return record(outcome);
  };
  const retryLater = (delayMs: number): Promise<DeliveryOutcome> => {
    msg.nak(delayMs);
    return record('failed');
  };

  // Our bug, not a transient failure: retrying can't fix it, and nothing may move money on it.
  // Terminated with an alert, whether or not the outcome can be recorded; the withdrawal stays
  // held (funds reserved) for a person to resolve, and reconciliation lists it as stuck.
  const bug = (fields: Record<string, unknown>, why: string): Promise<DeliveryOutcome> => {
    log.fatal({ ...fields, alert: true }, `${why}; terminating the message, withdrawal left held`);
    return stop('failed', why);
  };

  try {
    // 2. Validate. An invalid message is terminated: redelivering it can't fix it.
    const event = parseEvent(withdrawalHeldV1Schema, msg.body);
    if (event === undefined) {
      log.error('invalid withdrawal.held event; terminating it');
      return await stop('invalid', 'invalid withdrawal.held event');
    }
    const withdrawalId = event.withdrawal_id;
    log = log.child({ withdrawal_id: toTypeId('wd', withdrawalId) });

    // 3. Read the withdrawal: the request is built from what is stored, not from the event.
    const withdrawal = await getWithdrawal(deps.pool, withdrawalId);
    if (withdrawal === undefined) {
      log.error('withdrawal.held event for an unknown withdrawal; terminating it');
      return await stop('invalid', 'unknown withdrawal');
    }
    // Already submitted or settled (e.g. the provider's update overtook us, decision 27): no
    // call needed.
    //
    // A deliberate exception to "no check-then-act" (CLAUDE.md): this read decides whether to
    // make the provider call. That rule prevents races where acting on a stale read changes
    // state wrongly. Here a stale read can only cause one extra call, which changes nothing: the
    // provider answers our repeated key with the same transfer, and markSubmitted's conditional
    // transition skips. Money and status are still decided only by the conditional transitions;
    // the read exists to avoid a needless provider call.
    if (withdrawal.status !== 'held') {
      log.info({ status: withdrawal.status }, 'withdrawal no longer held; not submitting');
      const outcome = await finish('duplicate');
      msg.ack();
      return outcome;
    }

    // 4. The call, outside any transaction. working() first resets the message's ack_wait, so
    // the call (at most PROVIDER_TIMEOUT_MS, well under ack_wait) starts with the full lease,
    // however long the database steps before it took.
    msg.working();
    const result = await deps.provider.submitTransfer(withdrawal);

    // 5. Record the outcome.
    switch (result.kind) {
      case 'accepted': {
        const recorded = await withTransaction(deps.pool, (tx) =>
          markSubmitted(tx, withdrawalId, result.transferId),
        );
        log.info(
          { transfer_id: result.transferId, submitted: recorded },
          recorded === 'applied' ? 'withdrawal submitted' : 'withdrawal already moved on',
        );
        const outcome = await finish(recorded === 'applied' ? 'applied' : 'duplicate');
        msg.ack();
        return outcome;
      }
      case 'refused': {
        const released = await withTransaction(deps.pool, (tx) =>
          release(tx, withdrawalId, result.reason),
        );
        log.info(
          { reason: result.reason, released },
          released === 'applied'
            ? 'withdrawal refused by the provider; funds released'
            : 'withdrawal refused by the provider, but already settled',
        );
        const outcome = await finish(released === 'applied' ? 'applied' : 'duplicate');
        msg.ack();
        return outcome;
      }
      case 'unknown': {
        // The transfer may or may not exist at the provider. The withdrawal stays held, funds
        // reserved, which is correct either way; the redelivery asks again with the same key.
        const delayMs = retryDelay(deps, msg.deliveryAttempt);
        log.warn(
          { cause: result.cause, retry_in_ms: delayMs },
          'provider outcome unknown; will retry',
        );
        return await retryLater(delayMs);
      }
      case 'invalid':
        return await bug(
          // The status and the summary only; never the provider's body (see SubmitOutcome).
          {
            provider_status: 400,
            provider_error_fields: result.fields,
            provider_body_bytes: result.bodyBytes,
          },
          'provider rejected our request as invalid',
        );
      case 'key_conflict':
        return await bug(
          { provider_status: 409 },
          'provider reports our idempotency key reused with another request',
        );
    }
  } catch (err) {
    if (err instanceof TransferIdMismatchError) {
      // The provider's update stored another transfer ID than the one this submission got.
      return await bug(
        { stored_transfer_id: err.storedTransferId, reported_transfer_id: err.reportedTransferId },
        'provider transfer ID contradicts the stored one',
      );
    }
    // Transient (e.g. the database went away after an accepted call). The redelivery calls
    // again with the same key, gets the same transfer, and records it.
    log.error({ err }, 'submission failed; will retry');
    return await retryLater(retryDelay(deps, msg.deliveryAttempt));
  }
}
