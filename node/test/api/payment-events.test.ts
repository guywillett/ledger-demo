import { readFileSync } from 'node:fs';
import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, describe, expect, it } from 'vitest';
import { paymentCreatedV1Schema } from '../../src/shared/events.ts';
import { STREAMS } from '../../src/shared/nats.ts';
import {
  ALICE,
  FOREIGN_IBAN,
  capturingLogger,
  createTestApp,
  eventsFor,
  failingPublisher,
  notification,
  postPayment,
} from '../helpers.ts';

// payment.created publishing, against the compose NATS (stream created by nats-setup).

const testApp = await createTestApp();
const { app, pool, nc } = testApp;

afterAll(testApp.close);

describe('payment.created', () => {
  it('is published once for a new payment, stored with a per-payment Nats-Msg-Id', async () => {
    const body = notification();
    const response = await postPayment(app, body);
    expect(response.statusCode).toBe(201);

    const events = eventsFor(testApp, response.json.id);
    expect(events).toHaveLength(1);
    const [published] = events;
    expect(published?.subject).toBe('payment.created');
    expect(published?.event).toEqual({
      event_id: expect.stringMatching(/^evt_/),
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
    // No provider identifiers downstream.
    expect(JSON.stringify(published?.event)).not.toContain(ALICE.iban);
    expect(published?.msgId).toBe(`${String(response.json.id)}.created`);
    expect(published?.result.duplicate).toBe(false);

    // Read it back from the stream: what consumers will actually get.
    const jsm = await jetstreamManager(nc);
    const stored = await jsm.streams.getMessage(STREAMS.payments, {
      seq: published?.result.seq ?? 0,
    });
    expect(stored?.subject).toBe('payment.created');
    expect(stored?.header.get('Nats-Msg-Id')).toBe(`${String(response.json.id)}.created`);
    const decoded = stored?.json<unknown>();
    expect(decoded).toEqual(published?.event);
    // The consumer-side schema accepts it too.
    expect(paymentCreatedV1Schema.safeParse(decoded).success).toBe(true);
  });

  it('is announced again for a duplicate while still created, and the stream drops the repeat', async () => {
    const body = notification();
    const first = await postPayment(app, body);
    const second = await postPayment(app, body);
    expect([first.statusCode, second.statusCode]).toEqual([201, 200]);

    // Two publishes, same message ID: the second lands within the duplicate window, so the
    // stream acknowledges it as a duplicate of the first and stores nothing new.
    const events = eventsFor(testApp, first.json.id);
    expect(events).toHaveLength(2);
    expect(events[1]?.msgId).toBe(events[0]?.msgId);
    expect(events[1]?.event.event_id).not.toBe(events[0]?.event.event_id);
    expect(events.map((e) => e.result.duplicate)).toEqual([false, true]);
    expect(events[1]?.result.seq).toBe(events[0]?.result.seq);
  });

  it('is not published for a duplicate once the payment has moved on', async () => {
    const body = notification();
    const first = await postPayment(app, body);
    // Simulate the worker having booked it.
    await pool.query(
      `UPDATE incoming_payments SET status = 'received' WHERE network = 'SEPA' AND network_transaction_id = $1`,
      [body.network_transaction_id],
    );

    const second = await postPayment(app, body);
    expect(second.statusCode).toBe(200);
    expect(second.json.status).toBe('received');
    expect(eventsFor(testApp, first.json.id)).toHaveLength(1);
  });

  it('is not published for conflicts, unknown accounts or invalid bodies', async () => {
    const body = notification();
    const created = await postPayment(app, body);
    const before = testApp.published.length;

    const responses = [
      await postPayment(app, { ...body, amount_minor: '1' }),
      await postPayment(app, notification({ creditor_iban: FOREIGN_IBAN })),
      await postPayment(app, notification({ amount_minor: '0' })),
    ];
    expect(responses.map((r) => r.statusCode)).toEqual([409, 422, 400]);
    expect(testApp.published.length).toBe(before);
    expect(eventsFor(testApp, created.json.id)).toHaveLength(1);
  });
});

describe('payment.created publish failure', () => {
  it('answers 503 with the payment recorded, and a retry announces it', async () => {
    const { logger, lines } = capturingLogger();
    const failing = await createTestApp({ logger, publisher: failingPublisher });
    const body = notification();
    try {
      const response = await postPayment(failing.app, body);
      expect(response.statusCode).toBe(503);
      expect(response.headers['retry-after']).toBe('5');
      expect(response.json.type).toBe('urn:ledger-demo:problem:payment-not-announced');
      // A retry against a still-failing publisher fails the same way.
      expect((await postPayment(failing.app, body)).statusCode).toBe(503);
    } finally {
      await failing.close();
    }

    const stored = await pool.query<{ status: string; notification_count: number }>(
      `SELECT status, notification_count FROM incoming_payments
       WHERE network = 'SEPA' AND network_transaction_id = $1`,
      [body.network_transaction_id],
    );
    expect(stored.rows).toEqual([{ status: 'created', notification_count: 2 }]);

    // The failure is logged with the request's context and the event_id it was for.
    const failures = lines().filter((line) => line.level === 'error');
    expect(failures).toHaveLength(2);
    expect(failures[0]).toMatchObject({
      msg: 'payment recorded but payment.created could not be published',
      network_transaction_id: body.network_transaction_id,
      creditor_iban: 'FR76…0193',
      event_id: expect.stringMatching(/^evt_/),
      payment_id: expect.stringMatching(/^pay_/),
    });

    // The provider retries once NATS is back: a duplicate, announced now.
    const retry = await postPayment(app, body);
    expect(retry.statusCode).toBe(200);
    const announced = eventsFor(testApp, retry.json.id);
    expect(announced).toHaveLength(1);
    // Nothing was stored by the failed attempts, so this one is not a stream duplicate.
    expect(announced[0]?.result.duplicate).toBe(false);
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
