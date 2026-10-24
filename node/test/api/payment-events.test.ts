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
  paymentByNetworkId,
  postPayment,
  type StoredPayment,
  recordingPublisher,
  testRelayDeps,
  testStreamPublisher,
} from '../helpers.ts';

// payment.created: stored in the outbox by POST /provider/incoming-payments, in the payment's own transaction,
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

// The payment a notification created, read from the database (the webhook answers 204 with no
// body).
async function storedPayment(body: { network_transaction_id?: unknown }): Promise<StoredPayment> {
  const payment = await paymentByNetworkId(pool, body.network_transaction_id);
  if (payment === undefined) throw new Error('no payment stored');
  return payment;
}

async function outboxCount(): Promise<number> {
  const result = await pool.query<{ n: number }>('SELECT count(*)::int AS n FROM outbox');
  return result.rows[0]?.n ?? 0;
}

describe('payment.created in the outbox', () => {
  it('is stored once for a new payment, in the transaction that created it', async () => {
    const body = notificationBody();
    expect((await postPayment(app, body)).statusCode).toBe(204);
    const payment = await storedPayment(body);
    const paymentId = payment.id;
    const paymentTypeId = toTypeId('pay', paymentId);

    const rows = await outboxRowsFor(pool, paymentId);
    expect(rows).toHaveLength(1);
    const [row] = rows;
    expect(row).toMatchObject({
      entity_type: 'payment',
      entity_id: paymentId,
      subject: 'payment.created',
      msg_id: `${paymentTypeId}.created`,
      attempts: 0,
      published_at: null,
    });
    expect(row?.payload).toEqual({
      // The outbox row's ID is the event_id.
      event_id: row && toTypeId('evt', row.id),
      event_type: 'payment.created',
      schema_version: 1,
      payment_id: paymentTypeId,
      network: 'SEPA',
      network_transaction_id: body.network_transaction_id,
      account_id: ALICE.id,
      amount_minor: '1250',
      currency: 'EUR',
      occurred_at: payment.created_at.toISOString(),
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
    await postPayment(app, body);
    const before = await outboxCount();

    const repeats = [await postPayment(app, body), await postPayment(app, body)];
    expect(repeats.map((r) => r.statusCode)).toEqual([204, 204]);
    expect(await outboxCount()).toBe(before);
    expect(await outboxRowsFor(pool, (await storedPayment(body)).id)).toHaveLength(1);
  });

  it('is not stored for conflicts, unmatched payments or invalid bodies', async () => {
    const body = notificationBody();
    await postPayment(app, body);
    const before = await outboxCount();

    const responses = [
      await postPayment(app, { ...body, amount_minor: '1' }),
      await postPayment(app, notificationBody({ creditor_iban: FOREIGN_IBAN })),
      await postPayment(app, notificationBody({ amount_minor: '0' })),
    ];
    expect(responses.map((r) => r.statusCode)).toEqual([204, 204, 400]);
    expect(await outboxCount()).toBe(before);
    expect(await outboxRowsFor(pool, (await storedPayment(body)).id)).toHaveLength(1);
  });
});

describe('payment.created through the relay', () => {
  it('reaches the PAYMENTS stream once', async () => {
    const jsm = await jetstreamManager(nc);
    const messagesBefore = (await jsm.streams.info(STREAMS.payments)).state.messages;

    const body = notificationBody();
    expect((await postPayment(app, body)).statusCode).toBe(204);
    const paymentId = (await storedPayment(body)).id;
    const [row] = await outboxRowsFor(pool, paymentId);

    const msgId = `${toTypeId('pay', paymentId)}.created`;
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
