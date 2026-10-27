import { readFileSync } from 'node:fs';
import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, beforeEach, describe, expect, it } from 'vitest';
import { relayBatch } from '../../src/relay/relay.ts';
import { toTypeId } from '../../src/shared/ids.ts';
import { STREAMS } from '../../src/shared/nats.ts';
import { expectEventMatchesContract } from '../contract.ts';
import {
  ALICE,
  FOREIGN_IBAN,
  connectTestNats,
  createTestApp,
  drainOutbox,
  notificationBody,
  outboxRowsFor,
  depositByNetworkId,
  postIncomingPayment,
  type StoredDeposit,
  recordingPublisher,
  testRelayDeps,
  testStreamPublisher,
} from '../helpers.ts';

// deposit.created: stored in the outbox by POST /provider/incoming-payments, in the deposit's
// own transaction, and published by the relay. Runs in the serial "relay" project
// (vitest.config.ts): nothing else writes to the outbox or the test stream meanwhile, so counts
// before and after a request are exact.

const testApp = await createTestApp();
const { app, pool } = testApp;
const nc = await connectTestNats();
const streamPublisher = testStreamPublisher(nc);

beforeEach(() => drainOutbox(pool, streamPublisher));

afterAll(async () => {
  await nc.drain();
  await testApp.close();
});

// The deposit a notification created, read from the database (the webhook answers 204 with no
// body).
async function storedDeposit(body: { network_transaction_id?: unknown }): Promise<StoredDeposit> {
  const deposit = await depositByNetworkId(pool, body.network_transaction_id);
  if (deposit === undefined) throw new Error('no deposit stored');
  return deposit;
}

async function outboxCount(): Promise<number> {
  const result = await pool.query<{ n: number }>('SELECT count(*)::int AS n FROM outbox');
  return result.rows[0]?.n ?? 0;
}

describe('deposit.created in the outbox', () => {
  it('is stored once for a new deposit, in the transaction that created it', async () => {
    const body = notificationBody();
    expect((await postIncomingPayment(app, body)).statusCode).toBe(204);
    const deposit = await storedDeposit(body);
    const depositId = deposit.id;
    const depositTypeId = toTypeId('dep', depositId);

    const rows = await outboxRowsFor(pool, depositId);
    expect(rows).toHaveLength(1);
    const [row] = rows;
    expect(row).toMatchObject({
      entity_type: 'deposit',
      entity_id: depositId,
      subject: 'deposit.created',
      msg_id: `${depositTypeId}.created`,
      attempts: 0,
      published_at: null,
    });
    expect(row?.payload).toEqual({
      // The outbox row's ID is the event_id.
      event_id: row && toTypeId('evt', row.id),
      event_type: 'deposit.created',
      schema_version: 1,
      deposit_id: depositTypeId,
      network: 'SEPA',
      network_transaction_id: body.network_transaction_id,
      account_id: ALICE.id,
      amount_minor: '1250',
      currency: 'EUR',
      occurred_at: deposit.created_at.toISOString(),
    });
    expectEventMatchesContract(row?.payload);
    // No provider identifiers downstream.
    expect(JSON.stringify(row?.payload)).not.toContain(ALICE.iban);

    // xmin is the ID of the transaction that wrote a row version: the same for both rows, so
    // they were committed by one transaction.
    const xmin = await pool.query<{ same: boolean }>(
      `SELECT p.xmin::text = o.xmin::text AS same
       FROM deposits p JOIN outbox o ON o.entity_id = p.id
       WHERE p.id = $1`,
      [depositId],
    );
    expect(xmin.rows).toEqual([{ same: true }]);
  });

  it('is not stored again for duplicates', async () => {
    const body = notificationBody();
    await postIncomingPayment(app, body);
    const before = await outboxCount();

    const repeats = [await postIncomingPayment(app, body), await postIncomingPayment(app, body)];
    expect(repeats.map((r) => r.statusCode)).toEqual([204, 204]);
    expect(await outboxCount()).toBe(before);
    expect(await outboxRowsFor(pool, (await storedDeposit(body)).id)).toHaveLength(1);
  });

  it('is not stored for conflicts, unmatched deposits or invalid bodies', async () => {
    const body = notificationBody();
    await postIncomingPayment(app, body);
    const before = await outboxCount();

    const responses = [
      await postIncomingPayment(app, { ...body, amount_minor: '1' }),
      await postIncomingPayment(app, notificationBody({ creditor_iban: FOREIGN_IBAN })),
      await postIncomingPayment(app, notificationBody({ amount_minor: '0' })),
    ];
    expect(responses.map((r) => r.statusCode)).toEqual([204, 204, 400]);
    expect(await outboxCount()).toBe(before);
    expect(await outboxRowsFor(pool, (await storedDeposit(body)).id)).toHaveLength(1);
  });
});

describe('deposit.created through the relay', () => {
  it('reaches the DEPOSITS stream once', async () => {
    const jsm = await jetstreamManager(nc);
    const messagesBefore = (await jsm.streams.info(STREAMS.deposits)).state.messages;

    const body = notificationBody();
    expect((await postIncomingPayment(app, body)).statusCode).toBe(204);
    const depositId = (await storedDeposit(body)).id;
    const [row] = await outboxRowsFor(pool, depositId);

    const msgId = `${toTypeId('dep', depositId)}.created`;
    const { publisher, published } = recordingPublisher(streamPublisher);
    await relayBatch(testRelayDeps(pool, publisher));
    // A second batch doesn't publish it again: it is marked published.
    await relayBatch(testRelayDeps(pool, publisher));
    const ours = published.filter((p) => p.msgId === msgId);
    expect(ours).toHaveLength(1);
    expect(ours[0]?.result.duplicate).toBe(false);
    // Every publish the stream didn't drop as a duplicate stored one message (this one, and any
    // earlier test's backed-off event that became claimable meanwhile), and nothing else did.
    const stored = published.filter((p) => !p.result.duplicate).length;
    expect((await jsm.streams.info(STREAMS.deposits)).state.messages).toBe(messagesBefore + stored);

    const message = await jsm.streams.getMessage(STREAMS.deposits, {
      seq: ours[0]?.result.seq ?? 0,
    });
    expect(message?.subject).toBe('deposit.created');
    expect(message?.header.get('Nats-Msg-Id')).toBe(msgId);
    expect(message?.json<unknown>()).toEqual(row?.payload);
  });
});

describe('DEPOSITS stream', () => {
  it('matches nats/streams/DEPOSITS.json', async () => {
    const definition = JSON.parse(
      readFileSync(new URL('../../../nats/streams/DEPOSITS.json', import.meta.url), 'utf8'),
    ) as Record<string, unknown>;
    const jsm = await jetstreamManager(nc);
    const { config } = await jsm.streams.info(STREAMS.deposits);
    expect(config).toMatchObject(definition);
  });
});
