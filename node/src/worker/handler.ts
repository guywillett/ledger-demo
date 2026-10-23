import { setTimeout as sleep } from 'node:timers/promises';
import type pg from 'pg';
import { decide, startReview } from '../compliance/transitions.ts';
import type { Decision } from '../compliance/decision.ts';
import { LedgerInconsistencyError, MovementConflictError, OverdraftError } from '../ledger/move.ts';
import { book, postDecision } from '../ledger/posting.ts';
import { retryDelay } from '../shared/backoff.ts';
import { withTransaction } from '../shared/db.ts';
import { paymentCreatedV1Schema } from '../shared/events.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import { paymentExists } from '../payments/payments.ts';
import { parseEvent, peekEventId, type DeliveredMessage } from '../consumer/messages.ts';
import {
  finishDelivery,
  startDelivery,
  type DeliveryId,
  type DeliveryOutcome,
} from '../consumer/deliveries.ts';

// Handles one delivery of a payment.created event: the worker's steps 1-10 in the slice spec.
// Posting and compliance take turns, each step its own transaction:
//   Tx 1 book (posting) -> Tx 2 start review (compliance) -> review delay -> decision ->
//   Tx 3 decide (compliance) -> Tx 4 post the decision (posting)
// Every step is a conditional transition, so a redelivery (after a crash, a timeout or a
// duplicate publish) repeats only the steps that haven't happened yet: completed ones skip.

export type HandlerDeps = {
  readonly pool: pg.Pool;
  readonly logger: Logger;
  // The compliance decision for a payment under review (random in the demo).
  readonly decide: () => Decision;
  // How long the pseudo review takes.
  readonly reviewDelayMs: number;
  // During the review, signal working() at least this often (well under the consumer's
  // ack_wait), so the message isn't redelivered while it's being processed.
  readonly workingIntervalMs: number;
  // After a transient failure, the delay before JetStream redelivers: retryBaseDelayMs on the
  // first attempt, doubling with each further attempt, at most retryMaxDelayMs. Set on each
  // nak rather than as a consumer backoff, because a consumer backoff also replaces ack_wait
  // for messages still being processed.
  readonly retryBaseDelayMs: number;
  readonly retryMaxDelayMs: number;
};

export async function handleDelivery(
  deps: HandlerDeps,
  msg: DeliveredMessage,
): Promise<DeliveryOutcome> {
  // Step 1: log the delivery before anything else, with the event_id if it can be read at all.
  const peekedEventId = peekEventId(msg.body);
  let log = deps.logger.child({
    nats_msg_id: msg.natsMsgId,
    delivery_attempt: msg.deliveryAttempt,
    ...(peekedEventId === null ? {} : { event_id: toTypeId('evt', peekedEventId) }),
  });

  let deliveryId: DeliveryId;
  try {
    deliveryId = await startDelivery(deps.pool, {
      consumer: 'worker',
      eventId: peekedEventId,
      natsMsgId: msg.natsMsgId,
      deliveryAttempt: msg.deliveryAttempt,
    });
  } catch (err) {
    // Can't even record it (e.g. the database is down): try again later.
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
  const retryLater = (): Promise<DeliveryOutcome> => {
    msg.nak(retryDelay(deps, msg.deliveryAttempt));
    return record('failed');
  };

  try {
    // Step 2: validate. An invalid message is terminated: redelivering it can't fix it.
    const event = parseEvent(paymentCreatedV1Schema, msg.body);
    if (event === undefined) {
      log.error('invalid payment.created event; terminating it');
      return await stop('invalid', 'invalid payment.created event');
    }
    const paymentId = event.payment_id;
    log = log.child({ payment_id: toTypeId('pay', paymentId) });

    // Steps 3-8. Each result is 'applied' (this delivery made the transition) or 'skipped'.
    const booked = await withTransaction(deps.pool, (tx) => book(tx, paymentId));
    const reviewStarted = await withTransaction(deps.pool, (tx) => startReview(tx, paymentId));
    // Only the delivery that started the review waits it out. A duplicate (everything will
    // skip), an unknown payment, or a redelivery after a crash between Tx 2 and Tx 3 goes
    // straight on: the review has already happened from the system's point of view, and Tx 3
    // still records at most one decision.
    if (reviewStarted === 'applied') await review(deps, msg);
    const decision = deps.decide();
    const decided = await withTransaction(deps.pool, (tx) => decide(tx, paymentId, decision));
    // Which way to post comes from the recorded state, not from `decision`: on a redelivery
    // after an earlier decision, the one just made was discarded by Tx 3.
    const posted = await withTransaction(deps.pool, (tx) => postDecision(tx, paymentId));

    const changed =
      booked === 'applied' ||
      reviewStarted === 'applied' ||
      decided === 'applied' ||
      posted !== 'skipped';

    if (changed) {
      log.info(
        {
          booked,
          review_started: reviewStarted,
          decided: decided === 'applied' ? decision.outcome : 'skipped',
          posted,
        },
        'payment processed',
      );
      const outcome = await finish('applied');
      msg.ack();
      return outcome;
    }

    // Nothing changed: either everything had already happened (a duplicate), or there is no
    // such payment (the event refers to nothing, e.g. after a database reset). This read only
    // classifies the outcome; every transition has already been attempted.
    if (!(await paymentExists(deps.pool, paymentId))) {
      log.error('payment.created event for an unknown payment; terminating it');
      return await stop('invalid', 'unknown payment');
    }
    log.info('duplicate delivery: payment already processed');
    const outcome = await finish('duplicate');
    msg.ack();
    return outcome;
  } catch (err) {
    if (isInvariantViolation(err)) {
      // A bug, not a transient failure: retrying can't fix it, so don't loop on it. The
      // payment stays where it stopped (reconciliation shows it); someone has to look.
      log.fatal({ err, alert: true }, 'invariant violated; terminating the message');
      return await stop('failed', 'invariant violation');
    }
    // Transient (e.g. the database went away mid-way). Committed steps stay committed; the
    // redelivery carries on from there.
    log.error({ err }, 'processing failed; will retry');
    return await retryLater();
  }
}

// The pseudo review: wait reviewDelayMs, telling JetStream we're still working before each
// stretch so the message isn't redelivered meanwhile.
async function review(deps: HandlerDeps, msg: DeliveredMessage): Promise<void> {
  let remaining = deps.reviewDelayMs;
  while (remaining > 0) {
    msg.working();
    const wait = Math.min(remaining, deps.workingIntervalMs);
    await sleep(wait);
    remaining -= wait;
  }
}

// Errors that mean our data or code is wrong, as opposed to a dependency failing: the two
// guards disagree (LedgerInconsistencyError), a movement reference is reused for something
// else (MovementConflictError), a movement would overdraw a bucket (OverdraftError, which
// wraps the database's check_violation as its cause), or any other CHECK constraint rejected
// a change (Postgres error code 23514 on the error itself).
function isInvariantViolation(err: unknown): boolean {
  if (
    err instanceof LedgerInconsistencyError ||
    err instanceof MovementConflictError ||
    err instanceof OverdraftError
  ) {
    return true;
  }
  return typeof err === 'object' && err !== null && 'code' in err && err.code === '23514';
}
