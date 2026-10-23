import type pg from 'pg';
import type { z } from 'zod';
import { retryDelay, type Backoff } from '../shared/backoff.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import {
  finishDelivery,
  startDelivery,
  type ConsumerName,
  type DeliveryId,
  type DeliveryOutcome,
} from './deliveries.ts';
import { parseEvent, peekEventId, type DeliveredMessage } from './messages.ts';

// The delivery lifecycle every consumer's handler shares, so each handler holds only its domain
// steps: record the delivery, parse the event, run the handler, then settle the message (ack,
// term or nak) and record the outcome in the order the signal-vs-record rule requires
// (consumer/messages.ts). Unexpected errors are classified once here: our bug, terminated with
// an alert, or transient, retried later.

// What a handler must return: proof that it settled the message. Only the context's done(),
// stop() and retryLater() create one (as only withTransaction creates a Tx), so a handler path
// that never acks, terminates or naks doesn't compile. That guarantees at least once; settling a
// second time throws at run time (the types can't rule it out).
export type Settled = { readonly outcome: DeliveryOutcome } & { readonly __brand: 'Settled' };

export type DeliveryContext = {
  // The delivery's logger (nats_msg_id, delivery_attempt, event_id, plus whatever bind() adds).
  readonly log: Logger;
  // The message, for working() during long steps.
  readonly message: DeliveredMessage;
  // Adds fields (e.g. payment_id) to every later log line, the wrapper's own included.
  bind(fields: Record<string, unknown>): void;
  // Done: record the outcome, then ack. If recording fails, the error propagates and the
  // wrapper retries the message, whose redelivery finds the work done.
  done(outcome: 'applied' | 'duplicate'): Promise<Settled>;
  // Never deliver again: terminate, then record (a failed write is logged, not thrown).
  stop(outcome: 'invalid' | 'failed', reason: string): Promise<Settled>;
  // Deliver again after a delay (by default the backoff for this delivery attempt): nak, then
  // record 'failed'.
  retryLater(delayMs?: number): Promise<Settled>;
};

export type DeliveryLogOptions<S extends z.ZodType> = {
  readonly consumer: ConsumerName;
  // For log lines and term reasons, e.g. 'payment.created'.
  readonly eventType: string;
  readonly schema: S;
  readonly pool: pg.Pool;
  readonly logger: Logger;
  readonly retry: Backoff;
  // Classifies an error the handler didn't deal with: true for our bug (retrying can't fix it),
  // false for a transient failure (e.g. the database went away).
  readonly isBug: (err: unknown) => boolean;
  // The error line for a transient failure (default 'processing failed; will retry').
  readonly transientFailureMessage?: string;
};

export async function withDeliveryLog<S extends z.ZodType>(
  options: DeliveryLogOptions<S>,
  msg: DeliveredMessage,
  handle: (event: z.output<S>, ctx: DeliveryContext) => Promise<Settled>,
): Promise<DeliveryOutcome> {
  // Log the delivery before anything else, with the event_id if it can be read at all.
  const peekedEventId = peekEventId(msg.body);
  let log = options.logger.child({
    nats_msg_id: msg.natsMsgId,
    delivery_attempt: msg.deliveryAttempt,
    ...(peekedEventId === null ? {} : { event_id: toTypeId('evt', peekedEventId) }),
  });

  let deliveryId: DeliveryId;
  try {
    deliveryId = await startDelivery(options.pool, {
      consumer: options.consumer,
      eventId: peekedEventId,
      natsMsgId: msg.natsMsgId,
      deliveryAttempt: msg.deliveryAttempt,
    });
  } catch (err) {
    // Can't even record it (e.g. the database is down): try again later.
    log.error({ err }, 'could not record delivery; will retry');
    msg.nak(retryDelay(options.retry, msg.deliveryAttempt));
    return 'failed';
  }

  // The outcome the message was settled with, once it has been: a message is settled once.
  let settledWith: DeliveryOutcome | undefined;
  const settle = (outcome: DeliveryOutcome): void => {
    if (settledWith !== undefined) {
      throw new Error(`message already settled (${settledWith}); can't settle it as ${outcome}`);
    }
    settledWith = outcome;
  };
  const settled = (outcome: DeliveryOutcome): Settled => ({ outcome }) as Settled;
  // For stops and retries, which are signalled first: a failed write must not throw.
  const record = async (outcome: DeliveryOutcome): Promise<Settled> => {
    try {
      await finishDelivery(options.pool, deliveryId, outcome);
    } catch (err) {
      log.error({ err, outcome }, 'could not record the delivery outcome');
    }
    return settled(outcome);
  };

  const ctx: DeliveryContext = {
    get log() {
      return log;
    },
    message: msg,
    bind(fields) {
      log = log.child(fields);
    },
    async done(outcome) {
      if (settledWith !== undefined) settle(outcome); // throws
      await finishDelivery(options.pool, deliveryId, outcome);
      settle(outcome);
      msg.ack();
      return settled(outcome);
    },
    stop(outcome, reason) {
      settle(outcome);
      msg.term(reason);
      return record(outcome);
    },
    retryLater(delayMs = retryDelay(options.retry, msg.deliveryAttempt)) {
      settle('failed');
      msg.nak(delayMs);
      return record('failed');
    },
  };

  try {
    // Validate. An invalid message is terminated: redelivering it can't fix it.
    const event = parseEvent(options.schema, msg.body);
    if (event === undefined) {
      log.error(`invalid ${options.eventType} event; terminating it`);
      return (await ctx.stop('invalid', `invalid ${options.eventType} event`)).outcome;
    }
    return (await handle(event, ctx)).outcome;
  } catch (err) {
    // Already settled (e.g. an error after done(), or a second settle): never signal twice.
    if (settledWith !== undefined) {
      log.error({ err }, 'error after the message was settled; not settling it again');
      return settledWith;
    }
    if (options.isBug(err)) {
      // A bug, not a transient failure: retrying can't fix it, so don't loop on it. The entity
      // stays where it stopped (reconciliation shows it); someone has to look.
      log.fatal({ err, alert: true }, 'invariant violated; terminating the message');
      return (await ctx.stop('failed', 'invariant violation')).outcome;
    }
    // Transient (e.g. the database went away mid-way). Committed steps stay committed; the
    // redelivery carries on from there.
    log.error({ err }, options.transientFailureMessage ?? 'processing failed; will retry');
    return (await ctx.retryLater()).outcome;
  }
}
