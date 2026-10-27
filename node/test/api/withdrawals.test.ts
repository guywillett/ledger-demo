import { randomUUID } from 'node:crypto';
import { afterAll, describe, expect, it } from 'vitest';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { fromTypeId, newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { expectEventMatchesContract } from '../contract.ts';
import {
  balancesOf,
  capturingLogger,
  createTestAccount,
  createTestApp,
  fundTestAccount,
  getJson,
  outboxRowsFor,
  postWithdrawal,
  withdrawalBody,
  type TestAccount,
} from '../helpers.ts';

// POST /accounts/:account_id/withdrawals and GET /withdrawals/:id against the test database.
// Every response is checked against contracts/openapi.yaml (postWithdrawal, getJson). Outbox
// assertions look only at this test's own account or withdrawal, so this file runs in parallel
// with the others; publishing is tested in test/relay/.

const { app, pool, close } = await createTestApp();
afterAll(close);

async function fundedAccount(amountMinor = 1000n): Promise<TestAccount> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, amountMinor);
  return account;
}

const withdrawalIdOf = (response: { json: Record<string, unknown> }): Uuid<'wd'> =>
  fromTypeId('wd', String(response.json.id));

async function withdrawalsOf(account: TestAccount): Promise<number> {
  const result = await pool.query<{ n: number }>(
    'SELECT count(*)::int AS n FROM withdrawals WHERE account_id = $1',
    [account.id],
  );
  return result.rows[0]?.n ?? 0;
}

async function holdsOf(account: TestAccount): Promise<number> {
  const result = await pool.query<{ n: number }>(
    `SELECT count(*)::int AS n FROM balance_movements
     WHERE account_id = $1 AND reference_type = 'withdrawal' AND reference_step = 'hold'`,
    [account.id],
  );
  return result.rows[0]?.n ?? 0;
}

// withdrawal.held events about this account (the payload carries its TypeID).
async function heldEventsOf(accountTypeId: string): Promise<number> {
  const result = await pool.query<{ n: number }>(
    `SELECT count(*)::int AS n FROM outbox
     WHERE subject = 'withdrawal.held' AND payload->>'account_id' = $1`,
    [accountTypeId],
  );
  return result.rows[0]?.n ?? 0;
}

describe('POST /accounts/:account_id/withdrawals', () => {
  it('creates the withdrawal and holds its funds: 201 with a Location header', async () => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const response = await postWithdrawal(app, account.typeId, withdrawalBody(), key);

    expect(response.statusCode).toBe(201);
    expect(response.json).toMatchObject({
      id: expect.stringMatching(/^wd_/),
      account_id: account.typeId,
      idempotency_key: key,
      amount_minor: '400',
      currency: 'EUR',
      creditor_iban: 'DE75512108001245126199',
      creditor_name: 'Jane Doe',
      remittance_info: 'Rent October',
      status: 'held',
      failure_reason: null,
    });
    // Provider identifiers stay at the edge.
    expect(response.json).not.toHaveProperty('provider_transfer_id');
    expect(response.headers.location).toBe(`/withdrawals/${String(response.json.id)}`);

    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 600n,
      held: 400n,
    });
    expect(await holdsOf(account)).toBe(1);
    const balance = await getJson(
      app,
      `/accounts/${account.typeId}/balance`,
      '/accounts/{account_id}/balance',
    );
    expect(balance.json).toEqual({
      pending_in: '0',
      available: '600',
      held: '400',
      currency: 'EUR',
    });
  });

  it('trims the text fields, and treats an empty or blank remittance as none', async () => {
    const account = await fundedAccount(1000n);
    const trimmed = await postWithdrawal(
      app,
      account.typeId,
      withdrawalBody({ creditor_name: '  Jane Doe ', remittance_info: ' Rent  October ' }),
      randomUUID(),
    );
    expect(trimmed.statusCode).toBe(201);
    // Only the ends: the double space inside is kept.
    expect(trimmed.json).toMatchObject({
      creditor_name: 'Jane Doe',
      remittance_info: 'Rent  October',
    });

    for (const remittance of [undefined, null, '', '   ']) {
      const response = await postWithdrawal(
        app,
        account.typeId,
        withdrawalBody({ amount_minor: '1', remittance_info: remittance }),
        randomUUID(),
      );
      expect(response.statusCode).toBe(201);
      expect(response.json.remittance_info).toBeNull();
    }
  });

  it('answers a repeated request with the same withdrawal: one 201, then 200s, one hold', async () => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const responses = [];
    for (let i = 0; i < 5; i++) {
      responses.push(await postWithdrawal(app, account.typeId, withdrawalBody(), key));
    }

    expect(responses.map((r) => r.statusCode)).toEqual([201, 200, 200, 200, 200]);
    expect(new Set(responses.map((r) => r.json.id)).size).toBe(1);
    expect(await withdrawalsOf(account)).toBe(1);
    expect(await holdsOf(account)).toBe(1);
    expect(await balancesOf(pool, account.id)).toMatchObject({ available: 600n, held: 400n });
  });

  it('treats a repeat that differs only in trimmed whitespace or blank remittance as the same', async () => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const first = await postWithdrawal(
      app,
      account.typeId,
      withdrawalBody({ remittance_info: undefined }),
      key,
    );
    const again = await postWithdrawal(
      app,
      account.typeId,
      withdrawalBody({ creditor_name: ' Jane Doe ', remittance_info: '' }),
      key,
    );
    expect([first.statusCode, again.statusCode]).toEqual([201, 200]);
  });

  it('creates one withdrawal for 10 concurrent requests with one key', async () => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const responses = await Promise.all(
      Array.from({ length: 10 }, () => postWithdrawal(app, account.typeId, withdrawalBody(), key)),
    );

    const codes = responses.map((r) => r.statusCode).sort((a, b) => a - b);
    expect(codes).toEqual([200, 200, 200, 200, 200, 200, 200, 200, 200, 201]);
    expect(new Set(responses.map((r) => r.json.id)).size).toBe(1);
    expect(await withdrawalsOf(account)).toBe(1);
    expect(await holdsOf(account)).toBe(1);
    expect(await heldEventsOf(account.typeId)).toBe(1);
  });

  it.each([
    ['amount_minor', { amount_minor: '401' }],
    ['creditor_iban', { creditor_iban: 'FR7612345000010000000000193' }],
    ['creditor_name', { creditor_name: 'John Doe' }],
    ['remittance_info', { remittance_info: 'Rent November' }],
  ])('refuses the same key with a different %s: 409, nothing changed', async (field, overrides) => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const first = await postWithdrawal(app, account.typeId, withdrawalBody(), key);
    const conflict = await postWithdrawal(app, account.typeId, withdrawalBody(overrides), key);

    expect(conflict.statusCode).toBe(409);
    expect(conflict.json).toMatchObject({
      type: 'urn:ledger-demo:problem:idempotency-key-conflict',
      differing_fields: [field],
    });
    expect(await withdrawalsOf(account)).toBe(1);
    expect(await balancesOf(pool, account.id)).toMatchObject({ available: 600n, held: 400n });
    const stored = await getJson(
      app,
      `/withdrawals/${String(first.json.id)}`,
      '/withdrawals/{withdrawal_id}',
    );
    expect(stored.json).toEqual(first.json);
  });

  it('refuses insufficient funds with 422, storing nothing, and the key stays free', async () => {
    const account = await fundedAccount(300n);
    const key = randomUUID();
    const refused = await postWithdrawal(app, account.typeId, withdrawalBody(), key);

    expect(refused.statusCode).toBe(422);
    expect(refused.json.type).toBe('urn:ledger-demo:problem:insufficient-funds');
    expect(await withdrawalsOf(account)).toBe(0);
    expect(await heldEventsOf(account.typeId)).toBe(0);
    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 300n,
      held: 0n,
    });

    // Decision 24: only a successful request takes a key. Once funded, the same key works.
    await fundTestAccount(pool, account, 200n);
    const retried = await postWithdrawal(app, account.typeId, withdrawalBody(), key);
    expect(retried.statusCode).toBe(201);
    expect(await balancesOf(pool, account.id)).toMatchObject({ available: 100n, held: 400n });
  });

  it('lets exactly one of two concurrent withdrawals that together exceed available through', async () => {
    const account = await fundedAccount(1000n);
    const responses = await Promise.all([
      postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '700' }), randomUUID()),
      postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '700' }), randomUUID()),
    ]);

    expect(responses.map((r) => r.statusCode).sort((a, b) => a - b)).toEqual([201, 422]);
    expect(await withdrawalsOf(account)).toBe(1);
    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 300n,
      held: 700n,
    });
  });

  it('answers 404 for an unknown account, storing nothing; the key then works on a real one', async () => {
    const unknown = toTypeId('acct', newUuid('acct'));
    const key = randomUUID();
    const response = await postWithdrawal(app, unknown, withdrawalBody(), key);
    expect(response.statusCode).toBe(404);
    expect(response.json.type).toBe('urn:ledger-demo:problem:account-not-found');
    expect(await heldEventsOf(unknown)).toBe(0);

    const account = await fundedAccount(1000n);
    expect((await postWithdrawal(app, account.typeId, withdrawalBody(), key)).statusCode).toBe(201);
  });

  describe('400', () => {
    it.each([
      ['a missing Idempotency-Key', {}, undefined, 'idempotency-key'],
      ['an Idempotency-Key with a space', {}, 'has space', 'idempotency-key'],
      ['an Idempotency-Key over 255 characters', {}, 'k'.repeat(256), 'idempotency-key'],
      ['an invalid IBAN', { creditor_iban: 'DE00512108001245126199' }, 'key', 'creditor_iban'],
      ['an accent in the name', { creditor_name: 'Zoë Müller' }, 'key', 'creditor_name'],
      ["'&' in the name", { creditor_name: 'Smith & Sons' }, 'key', 'creditor_name'],
      ['a blank name', { creditor_name: '   ' }, 'key', 'creditor_name'],
      ['a name over 70 characters', { creditor_name: 'a'.repeat(71) }, 'key', 'creditor_name'],
      ['an accent in the remittance', { remittance_info: 'Café' }, 'key', 'remittance_info'],
      ['a misspelt (unknown) field', { remitance_info: 'Rent' }, 'key', ''],
      ['amount_minor "0"', { amount_minor: '0' }, 'key', 'amount_minor'],
      ['amount_minor as a number', { amount_minor: 400 }, 'key', 'amount_minor'],
      ['another currency', { currency: 'USD' }, 'key', 'currency'],
    ] as const)('for %s, storing nothing', async (_case, overrides, key, path) => {
      const account = await fundedAccount(1000n);
      const response = await postWithdrawal(
        app,
        account.typeId,
        withdrawalBody(overrides),
        key === undefined ? undefined : `${key === 'key' ? randomUUID() : key}`,
      );
      expect(response.statusCode).toBe(400);
      expect(response.json.type).toBe('urn:ledger-demo:problem:invalid-request');
      expect(response.json.errors).toEqual(
        expect.arrayContaining([expect.objectContaining({ path })]),
      );
      expect(await withdrawalsOf(account)).toBe(0);
    });

    it('reports every problem at once: account, key and body', async () => {
      const response = await postWithdrawal(
        app,
        'acct_bad',
        withdrawalBody({ creditor_name: '' }),
        undefined,
      );
      expect(response.statusCode).toBe(400);
      const paths = (response.json.errors as { path: string }[]).map((e) => e.path);
      expect(paths).toEqual(['account_id', 'idempotency-key', 'creditor_name']);
    });
  });

  it('stores withdrawal.held in the outbox in the same transaction, and nothing for a repeat', async () => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const response = await postWithdrawal(app, account.typeId, withdrawalBody(), key);
    const id = withdrawalIdOf(response);

    const rows = await outboxRowsFor(pool, id);
    expect(rows).toHaveLength(1);
    const [row] = rows;
    expect(row).toMatchObject({
      entity_type: 'withdrawal',
      entity_id: id,
      subject: 'withdrawal.held',
      msg_id: `${String(response.json.id)}.held`,
      published_at: null,
    });
    expect(row?.payload).toEqual({
      event_id: row && toTypeId('evt', row.id),
      event_type: 'withdrawal.held',
      schema_version: 1,
      withdrawal_id: response.json.id,
      account_id: account.typeId,
      amount_minor: '400',
      currency: 'EUR',
      occurred_at: response.json.created_at,
    });
    expectEventMatchesContract(row?.payload);
    // No personal data in the event.
    const text = JSON.stringify(row?.payload);
    for (const personal of ['DE75512108001245126199', 'Jane Doe', 'Rent October']) {
      expect(text).not.toContain(personal);
    }
    // Committed by the transaction that created the withdrawal.
    const xmin = await pool.query<{ same: boolean }>(
      `SELECT w.xmin::text = o.xmin::text AS same
       FROM withdrawals w JOIN outbox o ON o.entity_id = w.id WHERE w.id = $1`,
      [id],
    );
    expect(xmin.rows).toEqual([{ same: true }]);

    // A repeat and a conflict store no further event.
    await postWithdrawal(app, account.typeId, withdrawalBody(), key);
    await postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '1' }), key);
    expect(await outboxRowsFor(pool, id)).toHaveLength(1);
    expect(await heldEventsOf(account.typeId)).toBe(1);
  });

  it('logs outcomes without the name, the remittance text or the full IBAN', async () => {
    const { logger, lines } = capturingLogger();
    const logged = await createTestApp({ logger });
    const account = await fundedAccount(500n);
    const key = randomUUID();
    try {
      await postWithdrawal(logged.app, account.typeId, withdrawalBody(), key); // created
      await postWithdrawal(logged.app, account.typeId, withdrawalBody(), key); // repeat
      await postWithdrawal(logged.app, account.typeId, withdrawalBody({ amount_minor: '1' }), key);
      await postWithdrawal(logged.app, account.typeId, withdrawalBody(), randomUUID()); // 422
    } finally {
      await logged.close();
    }

    const domain = lines().filter(
      (line) => line.request_id !== undefined && line.msg !== 'request completed',
    );
    expect(domain.map((line) => [line.level, line.msg])).toEqual([
      ['info', 'withdrawal created'],
      ['info', 'repeated withdrawal request'],
      ['warn', 'idempotency key reused with different details'],
      ['info', 'withdrawal refused: insufficient funds'],
    ]);
    expect(domain[0]).toMatchObject({
      withdrawal_id: expect.stringMatching(/^wd_/),
      event_id: expect.stringMatching(/^evt_/),
      amount_minor: '400',
      creditor_iban: 'DE75…6199',
    });
    expect(domain[2]).toMatchObject({ differing_fields: ['amount_minor'] });

    const everything = JSON.stringify(lines());
    for (const personal of ['DE75512108001245126199', 'Jane Doe', 'Rent October']) {
      expect(everything).not.toContain(personal);
    }
  });
});

describe('GET /withdrawals/:withdrawal_id', () => {
  it('returns the withdrawal, and its history with ?include=history', async () => {
    const account = await fundedAccount(1000n);
    const created = await postWithdrawal(app, account.typeId, withdrawalBody(), randomUUID());
    const url = `/withdrawals/${String(created.json.id)}`;

    const plain = await getJson(app, url, '/withdrawals/{withdrawal_id}');
    expect(plain.statusCode).toBe(200);
    expect(plain.json).toEqual(created.json);

    const withHistory = await getJson(
      app,
      `${url}?include=history`,
      '/withdrawals/{withdrawal_id}',
    );
    expect(withHistory.json).toEqual({
      ...created.json,
      history: [{ from_status: null, to_status: 'held', reason: null, at: expect.any(String) }],
    });
  });

  it('answers 404 for an unknown withdrawal and 400 for an invalid ID', async () => {
    const missing = await getJson(
      app,
      `/withdrawals/${toTypeId('wd', newUuid('wd'))}`,
      '/withdrawals/{withdrawal_id}',
    );
    expect(missing.statusCode).toBe(404);
    expect(missing.json.type).toBe('urn:ledger-demo:problem:withdrawal-not-found');

    const invalid = await getJson(
      app,
      `/withdrawals/${toTypeId('dep', newUuid('dep'))}`,
      '/withdrawals/{withdrawal_id}',
    );
    expect(invalid.statusCode).toBe(400);
  });
});

describe('reconciliation after withdrawals through the API', () => {
  it('finds the account consistent', async () => {
    const account = await fundedAccount(1000n);
    await postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '300' }), 'a');
    await postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '300' }), 'a');
    await postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '200' }), 'b');
    await postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '900' }), 'c'); // 422

    const report = await reconcile(pool, {
      accountIds: [account.id],
      stuckAfterMs: 24 * 3600 * 1000,
    });
    expect(report).toMatchObject({
      withdrawalsChecked: 2,
      differences: [],
      withdrawalMismatches: [],
      consistent: true,
    });
  });
});
