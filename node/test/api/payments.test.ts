import { afterAll, describe, expect, it } from 'vitest';
import { buildApp } from '../../src/api/app.ts';
import { contractRequestExample } from '../contract.ts';
import {
  ALICE,
  BOB,
  CAROL,
  FOREIGN_IBAN,
  capturingLogger,
  createTestApp,
  newNetworkTransactionId,
  notification,
  postPayment,
  refusingDb,
  unusedEvents,
} from '../helpers.ts';

const { app, pool, close } = await createTestApp();

afterAll(close);

async function storedPayments(
  networkTransactionId: string,
): Promise<
  { status: string; amount_minor: bigint; creditor_iban: string; notification_count: number }[]
> {
  const result = await pool.query<{
    status: string;
    amount_minor: bigint;
    creditor_iban: string;
    notification_count: number;
  }>(
    `SELECT status, amount_minor, creditor_iban, notification_count
     FROM incoming_payments WHERE network = 'SEPA' AND network_transaction_id = $1`,
    [networkTransactionId],
  );
  return result.rows;
}

async function historyCount(networkTransactionId: string): Promise<number> {
  const result = await pool.query<{ n: number }>(
    `SELECT COUNT(*)::int AS n
     FROM payment_status_history h JOIN incoming_payments p ON p.id = h.payment_id
     WHERE p.network_transaction_id = $1`,
    [networkTransactionId],
  );
  return result.rows[0]?.n ?? 0;
}

describe('POST /payments: new notifications', () => {
  it('records a payment as created, mapped to our account by IBAN', async () => {
    const body = notification({ provider_account_id: 'prov_acc_0001' });
    const response = await postPayment(app, body);

    expect(response.statusCode).toBe(201);
    expect(response.json).toMatchObject({
      account_id: ALICE.id,
      creditor_iban: ALICE.iban,
      provider_account_id: 'prov_acc_0001',
      amount_minor: '1250',
      currency: 'EUR',
      network: 'SEPA',
      network_transaction_id: body.network_transaction_id,
      status: 'created',
      notification_count: 1,
    });
    expect(response.json.id).toMatch(/^pay_/);
    expect(await historyCount(String(body.network_transaction_id))).toBe(1);
  });

  it('accepts the request example from the contract', async () => {
    const example = contractRequestExample('POST', '/payments') as Record<string, unknown>;
    const response = await postPayment(app, {
      ...example,
      network_transaction_id: newNetworkTransactionId(),
    });
    expect(response.statusCode).toBe(201);
  });

  it('normalises the IBAN before matching', async () => {
    const response = await postPayment(
      app,
      notification({ creditor_iban: 'fr76 1234 5000 0100 0000 0000 290' }),
    );
    expect(response.statusCode).toBe(201);
    expect(response.json).toMatchObject({ account_id: BOB.id, creditor_iban: BOB.iban });
  });

  it('works for an account without a provider account ID', async () => {
    const response = await postPayment(app, notification({ creditor_iban: CAROL.iban }));
    expect(response.statusCode).toBe(201);
    expect(response.json).toMatchObject({ account_id: CAROL.id, provider_account_id: null });
  });

  it('keeps amounts beyond 2^53 exact', async () => {
    const response = await postPayment(app, notification({ amount_minor: '9007199254740993' }));
    expect(response.statusCode).toBe(201);
    expect(response.json.amount_minor).toBe('9007199254740993');
  });

  it('ignores unknown fields instead of rejecting them', async () => {
    const response = await postPayment(app, notification({ payer_name: 'Jane Doe' }));
    expect(response.statusCode).toBe(201);
    expect(response.json).not.toHaveProperty('payer_name');
  });
});

describe('POST /payments: duplicates', () => {
  it('records the same notification sent 5 times once, counting the repeats', async () => {
    const body = notification();
    const statuses: number[] = [];
    let firstId: unknown;
    for (let i = 0; i < 5; i++) {
      const response = await postPayment(app, body);
      statuses.push(response.statusCode);
      firstId ??= response.json.id;
      expect(response.json.id).toBe(firstId);
    }

    expect(statuses).toEqual([201, 200, 200, 200, 200]);
    const rows = await storedPayments(String(body.network_transaction_id));
    expect(rows).toHaveLength(1);
    expect(rows[0]?.notification_count).toBe(5);
    expect(await historyCount(String(body.network_transaction_id))).toBe(1);
  });

  it('records 10 concurrent identical notifications once', async () => {
    const body = notification();
    const responses = await Promise.all(Array.from({ length: 10 }, () => postPayment(app, body)));

    const statuses = responses.map((r) => r.statusCode).sort((a, b) => a - b);
    expect(statuses).toEqual([200, 200, 200, 200, 200, 200, 200, 200, 200, 201]);
    expect(new Set(responses.map((r) => r.json.id)).size).toBe(1);
    const rows = await storedPayments(String(body.network_transaction_id));
    expect(rows).toHaveLength(1);
    expect(rows[0]?.notification_count).toBe(10);
    expect(await historyCount(String(body.network_transaction_id))).toBe(1);
  });

  it('treats a retry with or without provider_account_id as a duplicate, keeping the first value', async () => {
    const body = notification({ provider_account_id: 'prov_acc_0001' });
    expect((await postPayment(app, body)).statusCode).toBe(201);

    const { provider_account_id: _omitted, ...withoutProviderId } = body;
    const retry = await postPayment(app, withoutProviderId);
    expect(retry.statusCode).toBe(200);
    expect(retry.json.provider_account_id).toBe('prov_acc_0001');

    const other = await postPayment(app, { ...body, provider_account_id: 'something-else' });
    expect(other.statusCode).toBe(200);
    expect(other.json.provider_account_id).toBe('prov_acc_0001');
  });

  it('treats formatting differences as the same details', async () => {
    const body = notification();
    expect((await postPayment(app, body)).statusCode).toBe(201);
    const reformatted = `{ "network": "SEPA", "currency": "EUR",
      "creditor_iban": "fr76 1234 5000 0100 0000 0000 193",
      "amount_minor": "1250", "network_transaction_id": "${String(body.network_transaction_id)}" }`;
    expect((await postPayment(app, reformatted)).statusCode).toBe(200);
  });
});

describe('POST /payments: conflicts', () => {
  it('rejects a different amount with 409 and changes nothing', async () => {
    const body = notification();
    expect((await postPayment(app, body)).statusCode).toBe(201);

    const response = await postPayment(app, { ...body, amount_minor: '1251' });
    expect(response.statusCode).toBe(409);
    expect(response.json.differing_fields).toEqual(['amount_minor']);

    const rows = await storedPayments(String(body.network_transaction_id));
    expect(rows).toEqual([
      { status: 'created', amount_minor: 1250n, creditor_iban: ALICE.iban, notification_count: 1 },
    ]);
  });

  it('rejects a different IBAN of ours with 409', async () => {
    const body = notification();
    expect((await postPayment(app, body)).statusCode).toBe(201);

    const response = await postPayment(app, {
      ...body,
      creditor_iban: BOB.iban,
      amount_minor: '1',
    });
    expect(response.statusCode).toBe(409);
    expect(response.json.differing_fields).toEqual(['creditor_iban', 'amount_minor']);
    expect((await storedPayments(String(body.network_transaction_id)))[0]?.creditor_iban).toBe(
      ALICE.iban,
    );
  });

  it('prefers 409 over 422 when an unknown IBAN reuses a network ID', async () => {
    const body = notification();
    expect((await postPayment(app, body)).statusCode).toBe(201);

    const response = await postPayment(app, { ...body, creditor_iban: FOREIGN_IBAN });
    expect(response.statusCode).toBe(409);
    expect(response.json.differing_fields).toEqual(['creditor_iban']);
  });

  it('lets exactly one of concurrent conflicting notifications create the payment', async () => {
    const networkTransactionId = newNetworkTransactionId();
    const amounts = ['100', '200', '300', '400', '500'];
    const responses = await Promise.all(
      amounts.map((amount) =>
        postPayment(
          app,
          notification({ network_transaction_id: networkTransactionId, amount_minor: amount }),
        ),
      ),
    );

    const statuses = responses.map((r) => r.statusCode).sort((a, b) => a - b);
    expect(statuses).toEqual([201, 409, 409, 409, 409]);
    const created = responses.find((r) => r.statusCode === 201);
    const rows = await storedPayments(networkTransactionId);
    expect(rows).toHaveLength(1);
    expect(rows[0]?.amount_minor.toString()).toBe(created?.json.amount_minor);
  });
});

describe('POST /payments: rejections', () => {
  it('answers 422 for a valid IBAN that is not ours, storing nothing', async () => {
    const body = notification({ creditor_iban: FOREIGN_IBAN });
    const response = await postPayment(app, body);
    expect(response.statusCode).toBe(422);
    expect(response.json.type).toBe('urn:ledger-demo:problem:unknown-account');
    expect(await storedPayments(String(body.network_transaction_id))).toEqual([]);
  });

  it.each([
    ['a bad IBAN checksum', { creditor_iban: 'FR7612345000010000000000194' }, 'creditor_iban'],
    ['a non-SEPA IBAN', { creditor_iban: 'BR1800360305000010009795493C1' }, 'creditor_iban'],
    ['a decimal amount', { amount_minor: '12.50' }, 'amount_minor'],
    ['a numeric amount', { amount_minor: 1250 }, 'amount_minor'],
    ['a zero amount', { amount_minor: '0' }, 'amount_minor'],
    ['a leading zero', { amount_minor: '0125' }, 'amount_minor'],
    ['an amount above int8', { amount_minor: '9223372036854775808' }, 'amount_minor'],
    ['another currency', { currency: 'USD' }, 'currency'],
    ['another network', { network: 'SWIFT' }, 'network'],
    ['an empty network transaction ID', { network_transaction_id: '' }, 'network_transaction_id'],
    ['a missing IBAN', { creditor_iban: undefined }, 'creditor_iban'],
  ])('answers 400 for %s', async (_label, overrides, path) => {
    const body = notification(overrides);
    const response = await postPayment(app, body);
    expect(response.statusCode).toBe(400);
    expect(response.json.type).toBe('urn:ledger-demo:problem:invalid-notification');
    expect(response.json.errors).toEqual([expect.objectContaining({ path })]);
    expect(await storedPayments(String(body.network_transaction_id))).toEqual([]);
  });

  it('answers 400 for malformed JSON', async () => {
    const response = await postPayment(app, '{"network_transaction_id": ');
    expect(response.statusCode).toBe(400);
    expect(response.json.type).toBe('about:blank');
  });

  it.each([
    ['text/plain', JSON.stringify(notification())],
    ['application/xml', '<payment/>'],
  ])('answers 415 for %s, even when the body is valid JSON', async (contentType, payload) => {
    const response = await postPayment(app, payload, { 'content-type': contentType });
    expect(response.statusCode).toBe(415);
  });

  it('answers 404 for an unknown route without echoing the URL', async () => {
    const response = await app.inject({
      method: 'GET',
      url: '/nope?iban=FR7612345000010000000000193',
    });
    expect(response.statusCode).toBe(404);
    expect(response.body).not.toContain('nope');
    expect(response.body).not.toContain('FR76');
  });
});

describe('POST /payments: request IDs', () => {
  it('reuses a well-formed x-request-id and echoes it', async () => {
    const response = await postPayment(app, notification(), { 'x-request-id': 'provider-req.42' });
    expect(response.headers['x-request-id']).toBe('provider-req.42');
  });

  // HTTP header values can't contain a newline at all, but they can contain a tab.
  it('replaces an x-request-id that is too long or has characters outside the allowed set', async () => {
    for (const bad of ['x'.repeat(65), 'has spaces', 'has\ttab', 'drop;table']) {
      const response = await postPayment(app, notification(), { 'x-request-id': bad });
      expect(response.headers['x-request-id']).not.toBe(bad);
      expect(String(response.headers['x-request-id'])).toMatch(/^[0-9a-f-]{36}$/);
    }
  });
});

describe('POST /payments: logging', () => {
  it('logs each outcome at its level, masks IBANs and never logs the body', async () => {
    const { logger, lines } = capturingLogger();
    const logged = await createTestApp({ logger });
    try {
      const body = notification({ payer_name: 'Jane Doe' });
      await postPayment(logged.app, body); // created
      await postPayment(logged.app, body); // duplicate
      await postPayment(logged.app, { ...body, amount_minor: '1' }); // conflict
      await postPayment(logged.app, notification({ creditor_iban: FOREIGN_IBAN })); // 422
      await postPayment(logged.app, notification({ amount_minor: 'lots' })); // 400
    } finally {
      await logged.close();
    }

    // The request's own lines (they carry request_id), not the access lines or connection status.
    const domain = lines().filter(
      (line) => line.request_id !== undefined && line.msg !== 'request completed',
    );
    expect(domain.map((line) => [line.level, line.msg])).toEqual([
      ['info', 'payment created'],
      ['info', 'duplicate payment notification'],
      ['error', 'payment notification conflicts with the stored payment'],
      ['warn', 'payment notification for an unknown account'],
      ['warn', 'invalid payment notification'],
    ]);
    expect(domain[0]).toMatchObject({ creditor_iban: 'FR76…0193', amount_minor: '1250' });
    expect(domain[0]?.payment_id).toMatch(/^pay_/);
    expect(domain[1]).toMatchObject({ duplicate: true, notification_count: 2 });
    expect(domain[2]).toMatchObject({
      differing_fields: ['amount_minor'],
      amount_minor: '1',
      stored_amount_minor: '1250',
      stored_creditor_iban: 'FR76…0193',
    });
    expect(domain[3]).toMatchObject({ creditor_iban: 'DE75…6199' });

    const access = lines().filter((line) => line.msg === 'request completed');
    expect(access.map((line) => line.status_code)).toEqual([201, 200, 409, 422, 400]);
    expect(access.every((line) => typeof line.request_id === 'string')).toBe(true);
    expect(access.every((line) => line.path === '/payments')).toBe(true);

    const everything = JSON.stringify(lines());
    expect(everything).not.toContain('Jane Doe');
    expect(everything).not.toContain(ALICE.iban);
    expect(everything).not.toContain(FOREIGN_IBAN);
  });
});

describe('POST /payments: access log', () => {
  it('logs the path without the query string', async () => {
    const { logger, lines } = capturingLogger();
    const logged = await createTestApp({ logger });
    try {
      const response = await logged.app.inject({
        method: 'POST',
        url: `/payments?iban=${ALICE.iban}&name=Jane%20Doe`,
        headers: { 'content-type': 'application/json' },
        payload: JSON.stringify(notification()),
      });
      expect(response.statusCode).toBe(201);
    } finally {
      await logged.close();
    }

    const access = lines().find((line) => line.msg === 'request completed');
    expect(access).toMatchObject({ method: 'POST', path: '/payments', status_code: 201 });
    const everything = JSON.stringify(lines());
    expect(everything).not.toContain('Jane');
    expect(everything).not.toContain(ALICE.iban);
  });
});

describe('POST /payments: internal inconsistency', () => {
  // A refused notification whose stored payment differs in no compared field is our problem,
  // not the caller's. The real database can't produce it without changing shared seed data,
  // so the app runs against a stub that refuses and then returns an identical stored payment.
  it('answers 500 and logs an error, without blaming the caller', async () => {
    const { logger, lines } = capturingLogger();
    const stubbed = buildApp({
      db: refusingDb({ creditor_iban: ALICE.iban, amount_minor: 1250n, currency: 'EUR' }),
      events: unusedEvents,
      logger,
    });
    try {
      const response = await postPayment(stubbed, notification());
      expect(response.statusCode).toBe(500);
      expect(response.json).toEqual({
        type: 'about:blank',
        title: 'Internal Server Error',
        status: 500,
      });
    } finally {
      await stubbed.close();
    }

    const errors = lines().filter((line) => line.level === 'error');
    expect(errors).toEqual([
      expect.objectContaining({
        msg: expect.stringContaining('no field differs'),
        creditor_iban: 'FR76…0193',
      }),
    ]);
    expect(errors[0]?.payment_id).toMatch(/^pay_/);
    expect(JSON.stringify(lines())).not.toContain(ALICE.iban);
  });
});
