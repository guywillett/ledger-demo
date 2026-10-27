import { setTimeout as sleep } from 'node:timers/promises';
import type pg from 'pg';
import { decide, startReview } from '../compliance/transitions.ts';
import type { Decision } from '../compliance/decision.ts';
import { isInvariantViolation } from '../ledger/invariants.ts';
import { book, postDecision } from '../ledger/posting.ts';
import { withTransaction } from '../shared/db.ts';
import { depositCreatedV1Schema } from '../shared/events.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import { depositExists } from '../deposits/deposits.ts';
import type { DeliveryOutcome } from '../consumer/deliveries.ts';
import { withDeliveryLog } from '../consumer/delivery-log.ts';
import type { DeliveredMessage } from '../consumer/messages.ts';

// Handles one delivery of a deposit.created event: the worker's steps 1-10 in the slice spec.
// Posting and compliance take turns, each step its own transaction:
//   Tx 1 book (posting) -> Tx 2 start review (compliance) -> review delay -> decision ->
//   Tx 3 decide (compliance) -> Tx 4 post the decision (posting)
// Every step is a conditional transition, so a redelivery (after a crash, a timeout or a
// duplicate publish) repeats only the steps that haven't happened yet: completed ones skip.

export type HandlerDeps = {
  readonly pool: pg.Pool;
  readonly logger: Logger;
  // The compliance decision for a deposit under review (random in the demo).
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

export function handleDelivery(deps: HandlerDeps, msg: DeliveredMessage): Promise<DeliveryOutcome> {
  // Step 1 (recording the delivery) and step 2 (validating the event) are the shared delivery
  // lifecycle's (consumer/delivery-log.ts), as is settling the message and recording the outcome.
  return withDeliveryLog(
    {
      consumer: 'worker',
      eventType: 'deposit.created',
      schema: depositCreatedV1Schema,
      pool: deps.pool,
      logger: deps.logger,
      retry: deps,
      isBug: isInvariantViolation,
    },
    msg,
    async (event, ctx) => {
      const depositId = event.deposit_id;
      ctx.bind({ deposit_id: toTypeId('dep', depositId) });

      // Steps 3-8. Each result is 'applied' (this delivery made the transition) or 'skipped'.
      const booked = await withTransaction(deps.pool, (tx) => book(tx, depositId));
      const reviewStarted = await withTransaction(deps.pool, (tx) => startReview(tx, depositId));
      // Only the delivery that started the review waits it out. A duplicate (everything will
      // skip), an unknown deposit, or a redelivery after a crash between Tx 2 and Tx 3 goes
      // straight on: the review has already happened from the system's point of view, and Tx 3
      // still records at most one decision.
      if (reviewStarted === 'applied') await review(deps, ctx.message);
      const decision = deps.decide();
      const decided = await withTransaction(deps.pool, (tx) => decide(tx, depositId, decision));
      // Which way to post comes from the recorded state, not from `decision`: on a redelivery
      // after an earlier decision, the one just made was discarded by Tx 3.
      const posted = await withTransaction(deps.pool, (tx) => postDecision(tx, depositId));

      const changed =
        booked === 'applied' ||
        reviewStarted === 'applied' ||
        decided === 'applied' ||
        posted !== 'skipped';

      if (changed) {
        ctx.log.info(
          {
            booked,
            review_started: reviewStarted,
            decided: decided === 'applied' ? decision.outcome : 'skipped',
            posted,
          },
          'deposit processed',
        );
        return ctx.done('applied');
      }

      // Nothing changed: either everything had already happened (a duplicate), or there is no
      // such deposit (the event refers to nothing, e.g. after a database reset). This read only
      // classifies the outcome; every transition has already been attempted.
      if (!(await depositExists(deps.pool, depositId))) {
        ctx.log.error('deposit.created event for an unknown deposit; terminating it');
        return ctx.stop('invalid', 'unknown deposit');
      }
      ctx.log.info('duplicate delivery: deposit already processed');
      return ctx.done('duplicate');
    },
  );
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
