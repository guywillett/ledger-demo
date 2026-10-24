import { afterAll, describe, expect, it } from 'vitest';
import { buildApp } from '../../src/api/app.ts';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import { contractRequestExample } from '../contract.ts';
import {
  ALICE,
  BOB,
  CAROL,
  FOREIGN_IBAN,
  capturingLogger,
  createTestApp,
  newNetworkTransactionId,
  newTestIban,
  notificationBody,
  paymentByNetworkId,
  postPayment,
  refusingPool,
} from '../helpers.ts';

// POST /provider/incoming-payments, the provider's incoming-payment webhook. Every notification it
// can parse gets 204 with no body, whatever it did (the webhook contract), so each test asserts
// the recorded effects: the payment, its outbox row, conflict records, the log line.

const { logger, lines } = capturingLogger();
const { app, pool, close } = await createTestApp({ logger });
afterAll(close);

const SEED_ACCOUNT_IDS = {
  [ALICE.iban]: '01a14c4e-e000-7f4f-896e-f8f54adff3bd',
  [BOB.iban]: '01a14c4e-e001-7331-bab2-75411dbbe55d',
  [CAROL.iban]: '01a14c4e-e002-71c0-a116-c28cf6a1c175',
};

async function historyCount(networkTransactionId: unknown): Promise<number> {
  const result = await pool.query<{ n: number }>(
    `SELECT COUNT(*)::int AS n
     FROM payment_status_history h JOIN incoming_payments p ON p.id = h.payment_id
     WHERE p.network_transaction_id = $1`,
    [networkTransactionId],
  );
  return result.rows[0]?.n ?? 0;
}

async function outboxRows(paymentId: string): Promise<number> {
  const result = await pool.query<{ n: number }>(
    'SELECT count(*)::int AS n FROM outbox WHERE entity_id = $1',
    [paymentId],
  );
  return result.rows[0]?.n ?? 0;
}

async function conflictsOf(
  paymentId: string,
): Promise<{ differing_fields: string[]; amount_minor: bigint; creditor_iban: string }[]> {
  const result = await pool.query<{
    differing_fields: string[];
    amount_minor: bigint;
    creditor_iban: string;
  }>(
    `SELECT differing_fields, amount_minor, creditor_iban FROM payment_notification_conflicts
     WHERE payment_id = $1 ORDER BY id`,
    [paymentId],
  );
  return result.rows;
}

function logOf(networkTransactionId: unknown, msg: string): Record<string, unknown> | undefined {
  return lines().find((l) => l.network_transaction_id === networkTransactionId && l.msg === msg);
}

describe('POST /provider/incoming-payments: new notifications', () => {
  it('records a payment as created, mapped to our account by IBAN, with its event', async () => {
    const body = notificationBody({ provider_account_id: 'prov_acc_0001' });
    const response = await postPayment(app, body);

    expect(response.statusCode).toBe(204);
    expect(response.body).toBe('');
    const payment = await paymentByNetworkId(pool, body.network_transaction_id);
    expect(payment).toMatchObject({
      account_id: SEED_ACCOUNT_IDS[ALICE.iban],
      status: 'created',
      creditor_iban: ALICE.iban,
      provider_account_id: 'prov_acc_0001',
      amount_minor: 1250n,
      notification_count: 1,
    });
    expect(await historyCount(body.network_transaction_id)).toBe(1);
    expect(await outboxRows(String(payment?.id))).toBe(1);
    expect(logOf(body.network_transaction_id, 'payment created')).toMatchObject({
      level: 'info',
      payment_id: payment && toTypeId('pay', payment.id),
    });
  });

  it('accepts the request example from the contract', async () => {
    const example = contractRequestExample('POST', '/provider/incoming-payments') as Record<
      string,
      unknown
    >;
    const body = { ...example, network_transaction_id: newNetworkTransactionId() };
    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect((await paymentByNetworkId(pool, body.network_transaction_id))?.status).toBe('created');
  });

  it('normalises the IBAN before matching', async () => {
    const body = notificationBody({ creditor_iban: 'fr76 1234 5000 0100 0000 0000 290' });
    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect(await paymentByNetworkId(pool, body.network_transaction_id)).toMatchObject({
      account_id: SEED_ACCOUNT_IDS[BOB.iban],
      creditor_iban: BOB.iban,
    });
  });

  it('works for an account without a provider account ID', async () => {
    const body = notificationBody({ creditor_iban: CAROL.iban });
    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect(await paymentByNetworkId(pool, body.network_transaction_id)).toMatchObject({
      account_id: SEED_ACCOUNT_IDS[CAROL.iban],
      provider_account_id: null,
    });
  });

  it('keeps amounts beyond 2^53 exact', async () => {
    const body = notificationBody({ amount_minor: '9007199254740993' });
    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect((await paymentByNetworkId(pool, body.network_transaction_id))?.amount_minor).toBe(
      9007199254740993n,
    );
  });

  it('ignores unknown fields instead of rejecting them', async () => {
    const body = notificationBody({ payer_name: 'Jane Doe' });
    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect((await paymentByNetworkId(pool, body.network_transaction_id))?.status).toBe('created');
  });
});

describe('POST /provider/incoming-payments: duplicates', () => {
  it('records the same notification sent 5 times once, counting the repeats', async () => {
    const body = notificationBody();
    const statuses: number[] = [];
    for (let i = 0; i < 5; i++) statuses.push((await postPayment(app, body)).statusCode);

    expect(statuses).toEqual([204, 204, 204, 204, 204]);
    const payment = await paymentByNetworkId(pool, body.network_transaction_id);
    expect(payment?.notification_count).toBe(5);
    expect(await historyCount(body.network_transaction_id)).toBe(1);
    expect(await outboxRows(String(payment?.id))).toBe(1);
    expect(logOf(body.network_transaction_id, 'duplicate payment notification')).toMatchObject({
      level: 'info',
    });
  });

  it('records 10 concurrent identical notifications once', async () => {
    const body = notificationBody();
    const responses = await Promise.all(Array.from({ length: 10 }, () => postPayment(app, body)));

    expect(responses.every((r) => r.statusCode === 204)).toBe(true);
    const count = await pool.query<{ n: number }>(
      'SELECT count(*)::int AS n FROM incoming_payments WHERE network_transaction_id = $1',
      [body.network_transaction_id],
    );
    expect(count.rows[0]?.n).toBe(1);
    expect((await paymentByNetworkId(pool, body.network_transaction_id))?.notification_count).toBe(
      10,
    );
    expect(await historyCount(body.network_transaction_id)).toBe(1);
  });

  it('treats a retry with or without provider_account_id as a duplicate, keeping the first value', async () => {
    const body = notificationBody({ provider_account_id: 'prov_acc_0001' });
    await postPayment(app, body);
    const { provider_account_id: _omitted, ...withoutProviderId } = body;
    await postPayment(app, withoutProviderId);
    await postPayment(app, { ...body, provider_account_id: 'something-else' });

    expect(await paymentByNetworkId(pool, body.network_transaction_id)).toMatchObject({
      provider_account_id: 'prov_acc_0001',
      notification_count: 3,
    });
  });

  it('treats formatting differences as the same details', async () => {
    const body = notificationBody();
    await postPayment(app, body);
    const reformatted = `{ "network": "SEPA", "currency": "EUR",
      "creditor_iban": "fr76 1234 5000 0100 0000 0000 193",
      "amount_minor": "1250", "network_transaction_id": "${String(body.network_transaction_id)}" }`;
    expect((await postPayment(app, reformatted)).statusCode).toBe(204);
    expect((await paymentByNetworkId(pool, body.network_transaction_id))?.notification_count).toBe(
      2,
    );
  });
});

describe('POST /provider/incoming-payments: conflicts', () => {
  it.each([
    ['a different amount', { amount_minor: '1251' }, ['amount_minor']],
    ['a different IBAN of ours', { creditor_iban: BOB.iban }, ['creditor_iban']],
    ['an IBAN that is not ours', { creditor_iban: FOREIGN_IBAN }, ['creditor_iban']],
  ])(
    'records %s as a conflict: 204, the payment unchanged, alerted',
    async (_case, overrides, differing) => {
      const body = notificationBody();
      await postPayment(app, body);
      const before = await paymentByNetworkId(pool, body.network_transaction_id);

      expect((await postPayment(app, { ...body, ...overrides })).statusCode).toBe(204);
      expect(await paymentByNetworkId(pool, body.network_transaction_id)).toEqual(before);
      expect(await conflictsOf(String(before?.id))).toEqual([
        expect.objectContaining({ differing_fields: differing }),
      ]);
      const alert = logOf(
        body.network_transaction_id,
        'payment notification conflicts with the stored payment; recorded, nothing changed',
      );
      expect(alert).toMatchObject({ level: 'error', alert: true, differing_fields: differing });
      // Field names, never the values.
      expect(JSON.stringify(alert)).not.toContain('1251');
    },
  );

  it('lets exactly one of concurrent conflicting notifications create the payment', async () => {
    const networkTransactionId = newNetworkTransactionId();
    const amounts = ['100', '200', '300', '400', '500'];
    const responses = await Promise.all(
      amounts.map((amount) =>
        postPayment(
          app,
          notificationBody({ network_transaction_id: networkTransactionId, amount_minor: amount }),
        ),
      ),
    );

    expect(responses.every((r) => r.statusCode === 204)).toBe(true);
    const payment = await paymentByNetworkId(pool, networkTransactionId);
    // The other four are conflict records, each differing in the amount.
    const conflicts = await conflictsOf(String(payment?.id));
    expect(conflicts).toHaveLength(4);
    expect(conflicts.every((c) => c.differing_fields.join() === 'amount_minor')).toBe(true);
    expect(
      [payment?.amount_minor, ...conflicts.map((c) => c.amount_minor)].map(String).sort(),
    ).toEqual(amounts);
  });
});

describe('POST /provider/incoming-payments: suspense', () => {
  it('records a valid IBAN that is not ours as unmatched: no account, no event, no movement', async () => {
    const body = notificationBody({ creditor_iban: FOREIGN_IBAN });
    expect((await postPayment(app, body)).statusCode).toBe(204);

    const payment = await paymentByNetworkId(pool, body.network_transaction_id);
    expect(payment).toMatchObject({ status: 'unmatched', account_id: null, notification_count: 1 });
    expect(await historyCount(body.network_transaction_id)).toBe(1);
    expect(await outboxRows(String(payment?.id))).toBe(0);
    const movements = await pool.query(
      `SELECT 1 FROM balance_movements WHERE reference_type = 'payment' AND reference_id = $1`,
      [payment?.id],
    );
    expect(movements.rowCount).toBe(0);
    expect(
      logOf(
        body.network_transaction_id,
        'payment notification matches none of our accounts; recorded in suspense',
      ),
    ).toMatchObject({ level: 'error', alert: true, creditor_iban: 'DE75…6199' });

    // A repeat is a duplicate of the suspense record.
    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect(await paymentByNetworkId(pool, body.network_transaction_id)).toMatchObject({
      status: 'unmatched',
      notification_count: 2,
    });

    // Whole-database reconciliation lists it in suspense; the books still agree for it.
    const report = await reconcile(pool, { stuckAfterMs: 24 * 3600 * 1000 });
    expect(report.suspense?.payments.map((p) => p.networkTransactionId)).toContain(
      body.network_transaction_id,
    );
    expect(report.mismatches.map((m) => m.paymentId)).not.toContain(payment?.id);
  });

  it.each([
    ['an unmatched payment with an account', 'unmatched', SEED_ACCOUNT_IDS[ALICE.iban]],
    ['a matched status without an account', 'created', null],
  ])('refuses %s (the database CHECK)', async (_case, status, accountId) => {
    // Resolving suspense must be an explicit status change, never a quiet account_id update.
    await expect(
      pool.query(
        `INSERT INTO incoming_payments (id, creditor_iban, account_id, amount_minor, currency,
                                        network, network_transaction_id, status)
         VALUES ($1, $2, $3, 100, 'EUR', 'SEPA', $4, $5)`,
        [newUuid('pay'), ALICE.iban, accountId, newNetworkTransactionId(), status],
      ),
    ).rejects.toMatchObject({ code: '23514', constraint: 'incoming_payments_unmatched_check' });
  });

  it('keeps it unmatched after an account with that IBAN is created: resolution is never automatic', async () => {
    const iban = newTestIban();
    const body = notificationBody({ creditor_iban: iban });
    await postPayment(app, body);
    await pool.query('INSERT INTO accounts (id, name, iban) VALUES ($1, $2, $3)', [
      newUuid('acct'),
      `late-${iban}`,
      iban,
    ]);

    expect((await postPayment(app, body)).statusCode).toBe(204);
    expect(await paymentByNetworkId(pool, body.network_transaction_id)).toMatchObject({
      status: 'unmatched',
      account_id: null,
      notification_count: 2,
    });
  });
});

describe('POST /provider/incoming-payments: rejections', () => {
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
  ])('answers 400 for %s, recording nothing', async (_label, overrides, path) => {
    const body = notificationBody(overrides);
    const response = await postPayment(app, body);
    expect(response.statusCode).toBe(400);
    expect(response.json.type).toBe('urn:ledger-demo:problem:invalid-notification');
    expect(response.json.errors).toEqual([expect.objectContaining({ path })]);
    expect(await paymentByNetworkId(pool, body.network_transaction_id)).toBeUndefined();
  });

  it('answers 400 for malformed JSON', async () => {
    const response = await postPayment(app, '{"network_transaction_id": ');
    expect(response.statusCode).toBe(400);
    expect(response.json.type).toBe('about:blank');
  });

  it.each([
    ['text/plain', JSON.stringify(notificationBody())],
    ['application/xml', '<payment/>'],
  ])(
    'answers 400 (not 415) for %s: a webhook has one 4xx, even for a valid JSON body',
    async (contentType, payload) => {
      const response = await postPayment(app, payload, { 'content-type': contentType });
      expect(response.statusCode).toBe(400);
    },
  );

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

describe('POST /provider/incoming-payments: request IDs', () => {
  it('reuses a well-formed x-request-id and echoes it', async () => {
    const response = await postPayment(app, notificationBody(), {
      'x-request-id': 'provider-req.42',
    });
    expect(response.headers['x-request-id']).toBe('provider-req.42');
  });

  // HTTP header values can't contain a newline at all, but they can contain a tab.
  it('replaces an x-request-id that is too long or has characters outside the allowed set', async () => {
    for (const bad of ['x'.repeat(65), 'has spaces', 'has\ttab', 'drop;table']) {
      const response = await postPayment(app, notificationBody(), { 'x-request-id': bad });
      expect(response.headers['x-request-id']).not.toBe(bad);
      expect(String(response.headers['x-request-id'])).toMatch(/^[0-9a-f-]{36}$/);
    }
  });
});

describe('POST /provider/incoming-payments: logging', () => {
  it('logs each outcome at its level, masks IBANs and never logs the body', async () => {
    const own = capturingLogger();
    const logged = await createTestApp({ logger: own.logger });
    try {
      const body = notificationBody({ payer_name: 'Jane Doe' });
      await postPayment(logged.app, body); // created
      await postPayment(logged.app, body); // duplicate
      await postPayment(logged.app, { ...body, amount_minor: '1' }); // conflict
      await postPayment(logged.app, notificationBody({ creditor_iban: FOREIGN_IBAN })); // unmatched
      await postPayment(logged.app, notificationBody({ amount_minor: 'lots' })); // 400
    } finally {
      await logged.close();
    }

    // The request's own lines (they carry request_id), not the access lines.
    const domain = own
      .lines()
      .filter((line) => line.request_id !== undefined && line.msg !== 'request completed');
    expect(domain.map((line) => [line.level, line.msg])).toEqual([
      ['info', 'payment created'],
      ['info', 'duplicate payment notification'],
      [
        'error',
        'payment notification conflicts with the stored payment; recorded, nothing changed',
      ],
      ['error', 'payment notification matches none of our accounts; recorded in suspense'],
      ['warn', 'invalid payment notification'],
    ]);
    expect(domain[0]).toMatchObject({ creditor_iban: 'FR76…0193', amount_minor: '1250' });
    expect(domain[0]?.event_id).toMatch(/^evt_/);
    expect(domain[1]).toMatchObject({ duplicate: true, notification_count: 2 });
    expect(domain[2]).toMatchObject({ alert: true, differing_fields: ['amount_minor'] });
    expect(domain[3]).toMatchObject({ alert: true, creditor_iban: 'DE75…6199' });

    const access = own.lines().filter((line) => line.msg === 'request completed');
    expect(access.map((line) => line.status_code)).toEqual([204, 204, 204, 204, 400]);
    expect(access.every((line) => line.path === '/provider/incoming-payments')).toBe(true);

    const everything = JSON.stringify(own.lines());
    expect(everything).not.toContain('Jane Doe');
    expect(everything).not.toContain(ALICE.iban);
    expect(everything).not.toContain(FOREIGN_IBAN);
  });

  it('logs the path without the query string', async () => {
    const own = capturingLogger();
    const logged = await createTestApp({ logger: own.logger });
    try {
      const response = await logged.app.inject({
        method: 'POST',
        url: `/provider/incoming-payments?iban=${ALICE.iban}&name=Jane%20Doe`,
        headers: { 'content-type': 'application/json' },
        payload: JSON.stringify(notificationBody()),
      });
      expect(response.statusCode).toBe(204);
    } finally {
      await logged.close();
    }
    const access = own.lines().find((line) => line.msg === 'request completed');
    expect(access).toMatchObject({
      method: 'POST',
      path: '/provider/incoming-payments',
      status_code: 204,
    });
    const everything = JSON.stringify(own.lines());
    expect(everything).not.toContain('Jane');
    expect(everything).not.toContain(ALICE.iban);
  });
});

describe('POST /provider/incoming-payments: internal inconsistency', () => {
  // A refused notification whose stored payment differs in no compared field is our problem,
  // not the provider's. The real database can't produce it without changing shared seed data,
  // so the app runs against a stub that refuses and then returns an identical stored payment.
  it('answers 500 and logs an error', async () => {
    const own = capturingLogger();
    const stubbed = buildApp({
      pool: refusingPool({ creditor_iban: ALICE.iban, amount_minor: 1250n, currency: 'EUR' }),
      logger: own.logger,
    });
    try {
      const response = await postPayment(stubbed, notificationBody());
      expect(response.statusCode).toBe(500);
      expect(response.json).toEqual({
        type: 'about:blank',
        title: 'Internal Server Error',
        status: 500,
      });
    } finally {
      await stubbed.close();
    }
    expect(own.lines().filter((line) => line.level === 'error')).toEqual([
      expect.objectContaining({
        msg: expect.stringContaining('no field differs'),
        creditor_iban: 'FR76…0193',
        alert: true,
      }),
    ]);
  });
});
