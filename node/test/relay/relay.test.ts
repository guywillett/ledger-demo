import { setTimeout as sleep } from 'node:timers/promises';
import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, beforeEach, describe, expect, it } from 'vitest';
import { claim } from '../../src/outbox/outbox.ts';
import { relayBatch } from '../../src/relay/relay.ts';
import { paymentCreatedV1Schema } from '../../src/shared/events.ts';
import { toTypeId } from '../../src/shared/ids.ts';
import { STREAMS } from '../../src/shared/nats.ts';
import type { Payment } from '../../src/payments/payments.ts';
import {
  capturingLogger,
  connectTestNats,
  createTestAccount,
  createTestPayment,
  createTestPool,
  drainOutbox,
  failingPublisher,
  outboxRowsFor,
  recordingPublisher,
  testRelayDeps,
  testStreamPublisher,
} from '../helpers.ts';

// relayBatch against the test database and the test NATS stream. Runs in the serial "relay"
// project (vitest.config.ts): each test drains the outbox first, so its own events are the only
// ones left to claim (except, rarely, an earlier test's backed-off event becoming claimable, so
// assertions look only at this test's events).

const pool = createTestPool();
const nc = await connectTestNats();
const streamPublisher = testStreamPublisher(nc);

beforeEach(() => drainOutbox(pool, streamPublisher));

afterAll(async () => {
  await nc.drain();
  await pool.end();
});

async function newPayment(): Promise<Payment> {
  return createTestPayment(pool, await createTestAccount(pool));
}

const msgIdOf = (payment: Payment): string => `${toTypeId('pay', payment.id)}.created`;

describe('relayBatch', () => {
  it('publishes each event with its msg_id as Nats-Msg-Id, and marks it published', async () => {
    const payments = [await newPayment(), await newPayment()];
    const { publisher, published } = recordingPublisher(streamPublisher);
    const { logger, lines } = capturingLogger();

    const { claimed, published: acknowledged } = await relayBatch(
      testRelayDeps(pool, publisher, { logger }),
    );
    expect(claimed).toBeGreaterThanOrEqual(2);
    expect(acknowledged).toBe(claimed);

    const jsm = await jetstreamManager(nc);
    for (const payment of payments) {
      const [row] = await outboxRowsFor(pool, payment.id);
      expect(row).toMatchObject({ attempts: 1, last_error: null, claimed_until: null });
      expect(row?.published_at).toBeInstanceOf(Date);

      const ours = published.filter((p) => p.msgId === msgIdOf(payment));
      expect(ours).toHaveLength(1);
      expect(ours[0]?.subject).toBe('payment.created');
      expect(ours[0]?.result.duplicate).toBe(false);

      // Read it back from the stream: what consumers will actually get.
      const stored = await jsm.streams.getMessage(STREAMS.payments, {
        seq: ours[0]?.result.seq ?? 0,
      });
      expect(stored?.header.get('Nats-Msg-Id')).toBe(msgIdOf(payment));
      const decoded = stored?.json<Record<string, unknown>>();
      // The outbox row's ID is the event_id.
      expect(decoded?.event_id).toBe(row && toTypeId('evt', row.id));
      expect(decoded).toEqual(row?.payload);
      expect(paymentCreatedV1Schema.safeParse(decoded).success).toBe(true);
    }

    // In id order: the earlier payment's event first.
    const order = published.map((p) => p.msgId).filter((id) => payments.map(msgIdOf).includes(id));
    expect(order).toEqual(payments.map(msgIdOf));

    const logged = lines().filter((l) => l.msg === 'event published');
    expect(logged.length).toBe(claimed);
    const firstPayment = payments[0] && toTypeId('pay', payments[0].id);
    const first = logged.find((l) => l.payment_id === firstPayment);
    expect(first).toMatchObject({
      level: 'info',
      event_id: expect.stringMatching(/^evt_/),
      subject: 'payment.created',
    });
    expect(first).not.toHaveProperty('stream_duplicate');
  });

  it('records a failure, leaves the event unpublished, and publishes it after the retry delay', async () => {
    const payment = await newPayment();
    const { logger, lines } = capturingLogger();

    const failed = await relayBatch(
      testRelayDeps(pool, failingPublisher, { logger, retryDelay: () => 300 }),
    );
    expect(failed.published).toBe(0);
    let [row] = await outboxRowsFor(pool, payment.id);
    expect(row).toMatchObject({
      attempts: 1,
      last_error: 'simulated publish failure',
      published_at: null,
    });
    const failure = lines().find((l) => l.msg === 'publish failed; will retry');
    expect(failure).toMatchObject({
      level: 'warn',
      attempts: 1,
      retry_in_ms: 300,
      payment_id: toTypeId('pay', payment.id),
    });
    expect(failure?.event_id).toMatch(/^evt_/);

    // Backed off: a batch straight away doesn't touch it.
    const { publisher, published } = recordingPublisher(streamPublisher);
    await relayBatch(testRelayDeps(pool, publisher));
    expect(published.filter((p) => p.msgId === msgIdOf(payment))).toEqual([]);

    // After the delay, the next batch publishes it: the second attempt.
    await sleep(400);
    await relayBatch(testRelayDeps(pool, publisher));
    expect(published.filter((p) => p.msgId === msgIdOf(payment))).toHaveLength(1);
    [row] = await outboxRowsFor(pool, payment.id);
    expect(row).toMatchObject({ attempts: 2, last_error: null });
    expect(row?.published_at).toBeInstanceOf(Date);
  });

  it('publishes again after a relay crashed between publishing and marking, and the stream drops the repeat', async () => {
    const payment = await newPayment();

    // The "crashed" relay: claims with a short lease and publishes, but never marks.
    const claimed = await claim(pool, { limit: 100, leaseMs: 300 });
    const event = claimed.find((e) => e.msgId === msgIdOf(payment));
    if (event === undefined) throw new Error('the payment’s event was not claimed');
    const first = await streamPublisher.publish(event.subject, event.payload, event.msgId);
    expect(first.duplicate).toBe(false);

    // Before the lease expires, another relay leaves it alone.
    const { publisher, published } = recordingPublisher(streamPublisher);
    await relayBatch(testRelayDeps(pool, publisher));
    expect(published.filter((p) => p.msgId === event.msgId)).toEqual([]);

    // Once it has expired, the event is claimed and published again, with the same msg_id: the
    // stream recognises the repeat within its duplicate window and stores nothing new.
    await sleep(400);
    const { logger, lines } = capturingLogger();
    await relayBatch(testRelayDeps(pool, publisher, { logger }));
    const again = published.filter((p) => p.msgId === event.msgId);
    expect(again).toHaveLength(1);
    expect(again[0]?.result).toMatchObject({ duplicate: true, seq: first.seq });
    // The same event, with the same event_id: it is the outbox row, not rebuilt.
    expect(again[0]?.event).toEqual(event.payload);

    const [row] = await outboxRowsFor(pool, payment.id);
    expect(row).toMatchObject({ attempts: 2, last_error: null });
    expect(row?.published_at).toBeInstanceOf(Date);
    expect(lines().find((l) => l.msg === 'event published')).toMatchObject({
      event_id: toTypeId('evt', event.id),
      stream_duplicate: true,
    });
  });
});
