import { claim, markFailed, markPublished, type OutboxEvent } from '../outbox/outbox.ts';
import type { Queryable } from '../shared/db.ts';
import { toTypeId } from '../shared/ids.ts';
import type { Logger } from '../shared/logger.ts';
import type { EventPublisher, PublishResult } from '../shared/nats.ts';

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
  let published = 0;
  for (const event of events) {
    if (await relayEvent(deps, event)) published += 1;
  }
  return { claimed: events.length, published };
}

// true if the stream acknowledged the event.
async function relayEvent(deps: RelayDeps, event: OutboxEvent): Promise<boolean> {
  const log = deps.logger.child({
    event_id: toTypeId('evt', event.id),
    ...entityLogFields(event),
  });

  let result: PublishResult;
  try {
    result = await deps.publisher.publish(event.subject, event.payload, event.msgId);
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

// The ID of what the event is about, as its TypeID, so a relay line can be found from the
// entity (a stuck pay_…) as well as from the event.
function entityLogFields(event: OutboxEvent): Record<string, string> {
  switch (event.entityType) {
    case 'payment':
      return { payment_id: toTypeId('pay', event.entityId) };
  }
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
