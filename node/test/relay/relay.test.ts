import { setTimeout as sleep } from 'node:timers/promises';
import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, beforeEach, describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { claim, enqueue } from '../../src/outbox/outbox.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { relayBatch } from '../../src/relay/relay.ts';
import { depositCreatedV1Schema } from '../../src/shared/events.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import { STREAMS } from '../../src/shared/nats.ts';
import type { Deposit } from '../../src/deposits/deposits.ts';
import {
  capturingLogger,
  connectTestNats,
  createTestAccount,
  createTestDeposit,
  createTestPool,
  createTestWithdrawal,
  drainOutbox,
  failingPublisher,
  fundTestAccount,
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

async function newDeposit(): Promise<Deposit> {
  return createTestDeposit(pool, await createTestAccount(pool));
}

const msgIdOf = (deposit: Deposit): string => `${toTypeId('dep', deposit.id)}.created`;

describe('relayBatch', () => {
  it('publishes each event with its msg_id as Nats-Msg-Id, and marks it published', async () => {
    const deposits = [await newDeposit(), await newDeposit()];
    const { publisher, published } = recordingPublisher(streamPublisher);
    const { logger, lines } = capturingLogger();

    const { claimed, published: acknowledged } = await relayBatch(
      testRelayDeps(pool, publisher, { logger }),
    );
    expect(claimed).toBeGreaterThanOrEqual(2);
    expect(acknowledged).toBe(claimed);

    const jsm = await jetstreamManager(nc);
    for (const deposit of deposits) {
      const [row] = await outboxRowsFor(pool, deposit.id);
      expect(row).toMatchObject({ attempts: 1, last_error: null, claimed_until: null });
      expect(row?.published_at).toBeInstanceOf(Date);

      const ours = published.filter((p) => p.msgId === msgIdOf(deposit));
      expect(ours).toHaveLength(1);
      expect(ours[0]?.subject).toBe('deposit.created');
      expect(ours[0]?.result.duplicate).toBe(false);

      // Read it back from the stream: what consumers will actually get.
      const stored = await jsm.streams.getMessage(STREAMS.deposits, {
        seq: ours[0]?.result.seq ?? 0,
      });
      expect(stored?.header.get('Nats-Msg-Id')).toBe(msgIdOf(deposit));
      const decoded = stored?.json<Record<string, unknown>>();
      // The outbox row's ID is the event_id.
      expect(decoded?.event_id).toBe(row && toTypeId('evt', row.id));
      expect(decoded).toEqual(row?.payload);
      expect(depositCreatedV1Schema.safeParse(decoded).success).toBe(true);
    }

    // In id order: the earlier deposit's event first.
    const order = published.map((p) => p.msgId).filter((id) => deposits.map(msgIdOf).includes(id));
    expect(order).toEqual(deposits.map(msgIdOf));

    const logged = lines().filter((l) => l.msg === 'event published');
    expect(logged.length).toBe(claimed);
    const firstDeposit = deposits[0] && toTypeId('dep', deposits[0].id);
    const first = logged.find((l) => l.deposit_id === firstDeposit);
    expect(first).toMatchObject({
      level: 'info',
      event_id: expect.stringMatching(/^evt_/),
      subject: 'deposit.created',
    });
    expect(first).not.toHaveProperty('stream_duplicate');
  });

  it('records a failure, leaves the event unpublished, and publishes it after the retry delay', async () => {
    const deposit = await newDeposit();
    const { logger, lines } = capturingLogger();

    const failed = await relayBatch(
      testRelayDeps(pool, failingPublisher, { logger, retryDelay: () => 300 }),
    );
    expect(failed.published).toBe(0);
    let [row] = await outboxRowsFor(pool, deposit.id);
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
      deposit_id: toTypeId('dep', deposit.id),
    });
    expect(failure?.event_id).toMatch(/^evt_/);

    // Backed off: a batch straight away doesn't touch it.
    const { publisher, published } = recordingPublisher(streamPublisher);
    await relayBatch(testRelayDeps(pool, publisher));
    expect(published.filter((p) => p.msgId === msgIdOf(deposit))).toEqual([]);

    // After the delay, the next batch publishes it: the second attempt.
    await sleep(400);
    await relayBatch(testRelayDeps(pool, publisher));
    expect(published.filter((p) => p.msgId === msgIdOf(deposit))).toHaveLength(1);
    [row] = await outboxRowsFor(pool, deposit.id);
    expect(row).toMatchObject({ attempts: 2, last_error: null });
    expect(row?.published_at).toBeInstanceOf(Date);
  });

  it('publishes again after a relay crashed between publishing and marking, and the stream drops the repeat', async () => {
    const deposit = await newDeposit();

    // The "crashed" relay: claims with a short lease and publishes, but never marks.
    const claimed = await claim(pool, { limit: 100, leaseMs: 300 });
    const event = claimed.find((e) => e.msgId === msgIdOf(deposit));
    if (event === undefined) throw new Error('the deposit’s event was not claimed');
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

    const [row] = await outboxRowsFor(pool, deposit.id);
    expect(row).toMatchObject({ attempts: 2, last_error: null });
    expect(row?.published_at).toBeInstanceOf(Date);
    expect(lines().find((l) => l.msg === 'event published')).toMatchObject({
      event_id: toTypeId('evt', event.id),
      stream_duplicate: true,
    });
  });
});

describe('relay routing', () => {
  it('publishes each event to its own stream: deposit.created to DEPOSITS, withdrawal.held to WITHDRAWALS', async () => {
    const account = await createTestAccount(pool);
    const deposit = await fundTestAccount(pool, account, 1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 400n);
    const { publisher, published } = recordingPublisher(streamPublisher);
    const { logger, lines } = capturingLogger();

    await relayBatch(testRelayDeps(pool, publisher, { logger }));

    const depositEvent = published.find((p) => p.msgId === msgIdOf(deposit));
    const withdrawalEvent = published.find(
      (p) => p.msgId === `${toTypeId('wd', withdrawal.id)}.held`,
    );
    expect(depositEvent?.result.stream).toBe(STREAMS.deposits);
    expect(withdrawalEvent?.result.stream).toBe(STREAMS.withdrawals);
    expect(withdrawalEvent?.subject).toBe('withdrawal.held');

    const jsm = await jetstreamManager(nc);
    const stored = await jsm.streams.getMessage(STREAMS.withdrawals, {
      seq: withdrawalEvent?.result.seq ?? 0,
    });
    expect(stored?.json<unknown>()).toEqual(withdrawalEvent?.event);
    const [row] = await outboxRowsFor(pool, withdrawal.id);
    expect(row?.published_at).toBeInstanceOf(Date);

    // The relay's log line names the withdrawal.
    expect(lines().find((l) => l.withdrawal_id === toTypeId('wd', withdrawal.id))).toMatchObject({
      msg: 'event published',
      subject: 'withdrawal.held',
    });
  });

  it('records an event with an unknown subject as a publish failure', async () => {
    const id = newUuid('evt');
    const depositId = newUuid('dep');
    await withTransaction(pool, (tx) =>
      enqueue(tx, {
        id,
        entityType: 'deposit',
        entityId: depositId,
        subject: 'refund.created',
        msgId: `${toTypeId('dep', depositId)}.refunded`,
        payload: { event_id: toTypeId('evt', id) },
      }),
    );
    try {
      await relayBatch(testRelayDeps(pool, streamPublisher));
      const [row] = await outboxRowsFor(pool, depositId);
      expect(row).toMatchObject({
        attempts: 1,
        last_error: 'no stream for subject refund.created',
        published_at: null,
      });
    } finally {
      // Not a real event: take it out of later drains.
      await pool.query('UPDATE outbox SET published_at = now() WHERE id = $1', [id]);
    }
  });

  it('streams match their definitions in nats/streams/', async () => {
    const jsm = await jetstreamManager(nc);
    for (const stream of Object.values(STREAMS)) {
      const definition = JSON.parse(
        readFileSync(new URL(`../../../nats/streams/${stream}.json`, import.meta.url), 'utf8'),
      ) as Record<string, unknown>;
      const { config } = await jsm.streams.info(stream);
      expect(config).toMatchObject(definition);
    }
  });
});
