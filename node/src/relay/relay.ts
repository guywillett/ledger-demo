import { SpanKind, SpanStatusCode, context, propagation, trace } from '@opentelemetry/api';
import { claim, markFailed, markPublished, type OutboxEvent } from '../outbox/outbox.ts';
import type { Queryable } from '../shared/db.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import type { EventPublisher, PublishResult } from '../shared/nats.ts';
import { linksTo } from '../shared/trace-context.ts';

// The relay: publishes the outbox to JetStream. Each batch is
//   1. claimed (one statement: leased to this relay, attempts counted; see outbox.claim),
//   2. published event by event in id order, outside any transaction, each with its msg_id as
//      the Nats-Msg-Id, waiting for the stream's acknowledgement,
//   3. marked: published, or failed with a retry delay.
//
// At least once: a relay that crashes between 2 and 3 leaves the lease to expire, and the event
// is published again; within the stream's duplicate window the msg_id makes the stream drop the
// repeat, and beyond it consumers are idempotent on state. No ordering guarantee: retries and
// other relay instances publish out of order, and consumers don't depend on order.
//
// Traced (slice 5 decision 80): a batch that claimed events is a trace of its own, its root span
// covering the publishes and the marks (so their database spans are its children); each publish is
// a span of its own in it, linked to the trace of the request that enqueued the event, and its
// context goes to the consumers as the message's headers (decision 82). A claim that finds nothing
// is no trace (decision 85): it runs outside any span, so its query gets none either.

// The instrumentation scope of the relay's spans.
const tracer = trace.getTracer('ledger-demo/relay');

export type RelayDeps = {
  readonly pool: Queryable;
  readonly publisher: EventPublisher;
  readonly logger: Logger;
  readonly batchSize: number;
  readonly leaseMs: number;
  // How long to wait before retrying an event whose publish failed, given its attempts so far.
  readonly retryDelay: (attempts: number) => number;
};

// How many events a batch claimed, and how many of those the stream acknowledged. The poll loop
// needs both: a full batch with nothing published means publishing is failing (e.g. NATS is
// down), and claiming the next batch at once would only fail that one too.
export type BatchResult = { readonly claimed: number; readonly published: number };

// Claims and publishes one batch. A failed publish is recorded and the rest of the batch carries
// on (there is no order to preserve). A database error (claiming or marking) ends the batch and
// is thrown: the events still leased are retried once their lease expires.
export async function relayBatch(deps: RelayDeps): Promise<BatchResult> {
  const events = await claim(deps.pool, { limit: deps.batchSize, leaseMs: deps.leaseMs });
  if (events.length === 0) return { claimed: 0, published: 0 };
  return tracer.startActiveSpan(
    'relay batch',
    {
      root: true,
      attributes: { 'messaging.system': 'nats', 'messaging.batch.message_count': events.length },
    },
    async (span) => {
      try {
        let published = 0;
        for (const event of events) {
          if (await relayEvent(deps, event)) published += 1;
        }
        return { claimed: events.length, published };
      } finally {
        span.end();
      }
    },
  );
}

// true if the stream acknowledged the event.
async function relayEvent(deps: RelayDeps, event: OutboxEvent): Promise<boolean> {
  const log = deps.logger.child({
    event_id: toTypeId('evt', event.id),
    ...entityLogFields(event),
  });

  let result: PublishResult;
  try {
    result = await publish(deps, event);
  } catch (err) {
    const retryInMs = deps.retryDelay(event.attempts);
    // Logged before marking: if marking fails too (the database is down), the publish error
    // and the event it was for are still on record.
    log.warn(
      { err, subject: event.subject, attempts: event.attempts, retry_in_ms: retryInMs },
      'publish failed; will retry',
    );
    await markFailed(deps.pool, event.id, errorText(err), retryInMs);
    return false;
  }

  // 'skipped': another relay published and marked it after this one's lease ran out.
  const marked = await markPublished(deps.pool, event.id);
  log.info(
    {
      subject: event.subject,
      stream_seq: result.seq,
      // The stream already held this msg_id (a repeat publish within the duplicate window), so it
      // stored nothing new.
      ...(result.duplicate ? { stream_duplicate: true } : {}),
      ...(marked === 'skipped' ? { already_marked: true } : {}),
    },
    'event published',
  );
  return true;
}

// Publishes the event in a span of its own (kind PRODUCER, by the messaging conventions), linked to
// the trace it was enqueued in, its context sent as the message's trace headers, so the consumer's
// span links to this one (decision 82). The marks come after it, in the batch's span.
async function publish(deps: RelayDeps, event: OutboxEvent): Promise<PublishResult> {
  return tracer.startActiveSpan(
    `publish ${event.subject}`,
    {
      kind: SpanKind.PRODUCER,
      links: linksTo(event.traceContext),
      attributes: {
        'messaging.system': 'nats',
        'messaging.operation.name': 'publish',
        'messaging.operation.type': 'send',
        'messaging.destination.name': event.subject,
        'messaging.message.id': event.msgId,
        event_id: toTypeId('evt', event.id),
        ...entityLogFields(event),
      },
    },
    async (span) => {
      const traceHeaders: Record<string, string> = {};
      propagation.inject(context.active(), traceHeaders);
      try {
        return await deps.publisher.publish(
          event.subject,
          event.payload,
          event.msgId,
          traceHeaders,
        );
      } catch (err) {
        span.setStatus({ code: SpanStatusCode.ERROR, message: 'publish failed' });
        throw err;
      } finally {
        span.end();
      }
    },
  );
}

// The ID of what the event is about, as its TypeID, so a relay line can be found from the
// entity (a stuck dep_…) as well as from the event.
function entityLogFields(event: OutboxEvent): Record<string, string> {
  switch (event.entityType) {
    case 'deposit':
      return { deposit_id: toTypeId('dep', event.entityId) };
    case 'withdrawal':
      return { withdrawal_id: toTypeId('wd', event.entityId) };
  }
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
