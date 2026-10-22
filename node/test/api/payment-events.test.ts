import { readFileSync } from 'node:fs';
import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, beforeEach, describe, expect, it } from 'vitest';
import { relayBatch } from '../../src/relay/relay.ts';
import { fromTypeId, toTypeId, type Uuid } from '../../src/shared/ids.ts';
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
  postPayment,
  recordingPublisher,
  testRelayDeps,
  testStreamPublisher,
} from '../helpers.ts';

// payment.created: stored in the outbox by POST /payments, in the payment's own transaction,
// and published by the relay. Runs in the serial "relay" project (vitest.config.ts): nothing
// else writes to the outbox or the test stream meanwhile, so counts before and after a request
// are exact.

const testApp = await createTestApp();
const { app, pool } = testApp;
const nc = await connectTestNats();
const streamPublisher = testStreamPublisher(nc);

beforeEach(() => drainOutbox(pool, streamPublisher));

afterAll(async () => {
  await nc.drain();
  await testApp.close();
});

function paymentIdOf(response: { json: Record<string, unknown> }): Uuid<'pay'> {
  return fromTypeId('pay', String(response.json.id));
}

async function outboxCount(): Promise<number> {
  const result = await pool.query<{ n: number }>('SELECT count(*)::int AS n FROM outbox');
  return result.rows[0]?.n ?? 0;
}

describe('payment.created in the outbox', () => {
  it('is stored once for a new payment, in the transaction that created it', async () => {
    const body = notificationBody();
    const response = await postPayment(app, body);
    expect(response.statusCode).toBe(201);
    const paymentId = paymentIdOf(response);

    const rows = await outboxRowsFor(pool, paymentId);
    expect(rows).toHaveLength(1);
    const [row] = rows;
    expect(row).toMatchObject({
      entity_type: 'payment',
      entity_id: paymentId,
      subject: 'payment.created',
      msg_id: `${String(response.json.id)}.created`,
      attempts: 0,
      published_at: null,
    });
    expect(row?.payload).toEqual({
      // The outbox row's ID is the event_id.
      event_id: row && toTypeId('evt', row.id),
      event_type: 'payment.created',
      schema_version: 1,
      payment_id: response.json.id,
      network: 'SEPA',
      network_transaction_id: body.network_transaction_id,
      account_id: ALICE.id,
      amount_minor: '1250',
      currency: 'EUR',
      occurred_at: response.json.created_at,
    });
    expectEventMatchesContract(row?.payload);
    // No provider identifiers downstream.
    expect(JSON.stringify(row?.payload)).not.toContain(ALICE.iban);

    // xmin is the ID of the transaction that wrote a row version: the same for both rows, so
    // they were committed by one transaction.
    const xmin = await pool.query<{ same: boolean }>(
      `SELECT p.xmin::text = o.xmin::text AS same
       FROM incoming_payments p JOIN outbox o ON o.entity_id = p.id
       WHERE p.id = $1`,
      [paymentId],
    );
    expect(xmin.rows).toEqual([{ same: true }]);
  });

  it('is not stored again for duplicates', async () => {
    const body = notificationBody();
    const first = await postPayment(app, body);
    const before = await outboxCount();

    const repeats = [await postPayment(app, body), await postPayment(app, body)];
    expect([first.statusCode, ...repeats.map((r) => r.statusCode)]).toEqual([201, 200, 200]);
    expect(await outboxCount()).toBe(before);
    expect(await outboxRowsFor(pool, paymentIdOf(first))).toHaveLength(1);
  });

  it('is not stored for conflicts, unknown accounts or invalid bodies', async () => {
    const body = notificationBody();
    const created = await postPayment(app, body);
    const before = await outboxCount();

    const responses = [
      await postPayment(app, { ...body, amount_minor: '1' }),
      await postPayment(app, notificationBody({ creditor_iban: FOREIGN_IBAN })),
      await postPayment(app, notificationBody({ amount_minor: '0' })),
    ];
    expect(responses.map((r) => r.statusCode)).toEqual([409, 422, 400]);
    expect(await outboxCount()).toBe(before);
    expect(await outboxRowsFor(pool, paymentIdOf(created))).toHaveLength(1);
  });
});

describe('payment.created through the relay', () => {
  it('reaches the PAYMENTS stream once', async () => {
    const jsm = await jetstreamManager(nc);
    const messagesBefore = (await jsm.streams.info(STREAMS.payments)).state.messages;

    const response = await postPayment(app, notificationBody());
    expect(response.statusCode).toBe(201);
    const [row] = await outboxRowsFor(pool, paymentIdOf(response));

    const msgId = `${String(response.json.id)}.created`;
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
    expect((await jsm.streams.info(STREAMS.payments)).state.messages).toBe(messagesBefore + stored);

    const message = await jsm.streams.getMessage(STREAMS.payments, {
      seq: ours[0]?.result.seq ?? 0,
    });
    expect(message?.subject).toBe('payment.created');
    expect(message?.header.get('Nats-Msg-Id')).toBe(msgId);
    expect(message?.json<unknown>()).toEqual(row?.payload);
  });
});

describe('PAYMENTS stream', () => {
  it('matches nats/streams/PAYMENTS.json', async () => {
    const definition = JSON.parse(
      readFileSync(new URL('../../../nats/streams/PAYMENTS.json', import.meta.url), 'utf8'),
    ) as Record<string, unknown>;
    const jsm = await jetstreamManager(nc);
    const { config } = await jsm.streams.info(STREAMS.payments);
    expect(config).toMatchObject(definition);
  });
});
