import { setTimeout as sleep } from 'node:timers/promises';
import { afterAll, afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  claim,
  enqueue,
  markFailed,
  markPublished,
  type NewOutboxEvent,
} from '../../src/outbox/outbox.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import {
  connectTestNats,
  createTestPool,
  drainOutbox,
  outboxRowsFor,
  testStreamPublisher,
} from '../helpers.ts';

// The outbox's data access against the test database. claim() takes the oldest unpublished
// events in the whole table, so this file runs in the serial "relay" project (vitest.config.ts)
// and drains the backlog before each test. Assertions still look only at this file's events:
// an event backed off by an earlier test can become claimable again at any moment.

const pool = createTestPool();
const nc = await connectTestNats();
const publisher = testStreamPublisher(nc);

// Every event this file creates. Their payloads are not real events, so they are marked
// published after each test rather than left for the next drain to publish to the test stream.
const created: Uuid<'evt'>[] = [];

beforeEach(() => drainOutbox(pool, publisher));

afterEach(async () => {
  await pool.query('UPDATE outbox SET published_at = now() WHERE id = ANY($1)', [created]);
});

afterAll(async () => {
  await nc.drain();
  await pool.end();
});

// A minimal event about a fresh (never stored) deposit; the outbox doesn't check that it exists.
// Typed as the deposit branch, so tests can spread overrides onto it.
function testEvent(): Extract<NewOutboxEvent, { entityType: 'deposit' }> {
  const id = newUuid('evt');
  const depositId = newUuid('dep');
  created.push(id);
  return {
    id,
    entityType: 'deposit',
    entityId: depositId,
    subject: 'deposit.created',
    msgId: `${toTypeId('dep', depositId)}.created`,
    payload: { event_id: toTypeId('evt', id), nested: { amount_minor: '1250' } },
  };
}

async function enqueueAll(events: NewOutboxEvent[]): Promise<void> {
  await withTransaction(pool, async (tx) => {
    for (const e of events) await enqueue(tx, e);
  });
}

// Claims (limit 100) and keeps only the given events' IDs, in the order claimed.
async function claimOnly(events: readonly NewOutboxEvent[], leaseMs = 30_000): Promise<string[]> {
  const ids = events.map((e) => e.id as string);
  const claimed = await claim(pool, { limit: 100, leaseMs });
  return claimed.map((e) => e.id as string).filter((id) => ids.includes(id));
}

const idOrder = (events: readonly NewOutboxEvent[]): string[] => events.map((e) => e.id).sort();

describe('enqueue', () => {
  it('stores the event with its payload as JSON', async () => {
    const event = testEvent();
    await enqueueAll([event]);
    const [row] = await outboxRowsFor(pool, event.entityId);
    expect(row).toMatchObject({
      id: event.id,
      entity_type: 'deposit',
      subject: 'deposit.created',
      msg_id: event.msgId,
      payload: event.payload,
      attempts: 0,
      claimed_until: null,
      last_error: null,
      published_at: null,
    });
  });

  it('rolls back with its transaction', async () => {
    const event = testEvent();
    await expect(
      withTransaction(pool, async (tx) => {
        await enqueue(tx, event);
        throw new Error('the state change failed');
      }),
    ).rejects.toThrow('the state change failed');
    expect(await outboxRowsFor(pool, event.entityId)).toEqual([]);
  });

  it('throws on a second event with the same msg_id', async () => {
    const first = testEvent();
    await enqueueAll([first]);
    const second = { ...testEvent(), entityId: first.entityId, msgId: first.msgId };
    await expect(enqueueAll([second])).rejects.toThrow(/already holds an event with msg_id/);
    expect((await outboxRowsFor(pool, first.entityId)).map((r) => r.id)).toEqual([first.id]);
  });
});

describe('claim', () => {
  it('claims in id order, leasing each event and counting the attempt', async () => {
    const events = [testEvent(), testEvent(), testEvent()];
    await enqueueAll(events);

    expect(await claimOnly(events)).toEqual(idOrder(events));
    for (const event of events) {
      const [row] = await outboxRowsFor(pool, event.entityId);
      expect(row?.attempts).toBe(1);
      expect(row?.claimed_until?.getTime()).toBeGreaterThan(Date.now() + 25_000);
    }
  });

  it('respects the limit, oldest first', async () => {
    const events = [testEvent(), testEvent(), testEvent()];
    await enqueueAll(events);

    const claimed = await claim(pool, { limit: 2, leaseMs: 30_000 });
    expect(claimed).toHaveLength(2);
    // After the drain, these are the oldest claimable events (unless an earlier test's backed-off
    // event has just become claimable again, which would come first).
    const [, , newest] = idOrder(events);
    expect(claimed.map((e) => e.id)).not.toContain(newest);
  });

  it('skips leased and published events, and reclaims one once its lease expires', async () => {
    const leased = testEvent();
    const published = testEvent();
    await enqueueAll([leased, published]);

    expect(await claimOnly([leased, published], 300)).toEqual(idOrder([leased, published]));
    expect(await markPublished(pool, published.id)).toBe('applied');
    // Both leased: nothing of ours to claim yet.
    expect(await claimOnly([leased, published])).toEqual([]);

    await sleep(400);
    // The lease has expired: claimed again, the second attempt. The published one never is.
    expect(await claimOnly([leased, published])).toEqual([leased.id]);
    const [row] = await outboxRowsFor(pool, leased.entityId);
    expect(row?.attempts).toBe(2);
  });

  it('gives two concurrent claims disjoint events (SKIP LOCKED)', async () => {
    const events = Array.from({ length: 20 }, () => testEvent());
    await enqueueAll(events);

    // Two claims at once, on two connections, each for at most 10. Neither waits for the other's
    // locks: each skips the rows the other has locked and takes the next ones. No event is
    // claimed twice, and between them they take all twenty.
    const [a, b] = await Promise.all([
      claim(pool, { limit: 10, leaseMs: 30_000 }),
      claim(pool, { limit: 10, leaseMs: 30_000 }),
    ]);
    const ids = (claimed: { id: string }[]): string[] => claimed.map((e) => e.id);
    expect(ids(a).filter((id) => ids(b).includes(id))).toEqual([]);
    const ours = [...ids(a), ...ids(b)].filter((id) => created.includes(id as Uuid<'evt'>));
    expect(ours.sort()).toEqual(idOrder(events));
  });
});

describe('markPublished and markFailed', () => {
  it('records a failure with a retry delay, then publication, each once', async () => {
    const event = testEvent();
    await enqueueAll([event]);
    expect(await claimOnly([event])).toEqual([event.id]);

    expect(await markFailed(pool, event.id, 'stream unavailable', 60_000)).toBe('applied');
    let [row] = await outboxRowsFor(pool, event.entityId);
    expect(row?.last_error).toBe('stream unavailable');
    expect(row?.claimed_until?.getTime()).toBeGreaterThan(Date.now() + 55_000);
    // Backed off: not claimable until then.
    expect(await claimOnly([event])).toEqual([]);

    expect(await markPublished(pool, event.id)).toBe('applied');
    [row] = await outboxRowsFor(pool, event.entityId);
    expect(row?.published_at).toBeInstanceOf(Date);
    expect(row?.claimed_until).toBeNull();
    expect(row?.last_error).toBeNull();

    // Already published: both are no-ops.
    expect(await markPublished(pool, event.id)).toBe('skipped');
    expect(await markFailed(pool, event.id, 'late failure', 1000)).toBe('skipped');
  });
});
