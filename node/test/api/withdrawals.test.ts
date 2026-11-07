import { randomUUID } from 'node:crypto';
import { afterAll, describe, expect, it } from 'vitest';
import { execute, markSubmitted, release } from '../../src/ledger/withdrawals.ts';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { fromTypeId, newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { expectEventMatchesContract } from '../contract.ts';
import {
  apiBalances,
  apiWithdrawals,
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

// POST /accounts/:account_id/withdrawals, the account's withdrawals list and the single
// withdrawal, against the test database.
// Every response is checked against contracts/openapi.yaml (postWithdrawal, getJson). Outbox
// assertions look only at this test's own account or withdrawal, so this file runs in parallel
// with the others; publishing is tested in test/relay/.

const { app, pool, close } = await createTestApp();
afterAll(close);

const WITHDRAWALS = '/accounts/{account_id}/withdrawals';
const WITHDRAWAL = '/accounts/{account_id}/withdrawals/{withdrawal_id}';
const WITHDRAWAL_NOT_FOUND = {
  type: 'urn:ledger-demo:problem:withdrawal-not-found',
  title: 'Withdrawal not found',
  status: 404,
};
const STATUS_MESSAGE = 'Invalid option: expected one of "held"|"submitted"|"executed"|"failed"';

async function fundedAccount(amountMinor = 1000n): Promise<TestAccount> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, amountMinor);
  return account;
}

const withdrawalIdOf = (response: { json: Record<string, unknown> }): Uuid<'wd'> =>
  fromTypeId('wd', String(response.json.id));

// How many withdrawals the account's list shows.
async function withdrawalsOf(account: TestAccount): Promise<number> {
  return (await apiWithdrawals(app, account.typeId)).length;
}
// The ledger's hold movements for the account: the journal isn't in the client API.
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
    // type first (slice 4 decision 67), in every response that carries a withdrawal.
    expect(response.body.startsWith('{"type":"withdrawal","id":"wd_')).toBe(true);
    expect(response.json).toMatchObject({
      type: 'withdrawal',
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
    expect(response.headers.location).toBe(
      `/accounts/${account.typeId}/withdrawals/${String(response.json.id)}`,
    );

    expect(await holdsOf(account)).toBe(1);
    const balance = await getJson(
      app,
      `/accounts/${account.typeId}/balance`,
      '/accounts/{account_id}/balance',
    );
    expect(balance.json).toEqual({
      account_id: account.typeId,
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
    expect(await apiBalances(app, account.typeId)).toMatchObject({ available: 600n, held: 400n });
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
    expect(await apiBalances(app, account.typeId)).toMatchObject({ available: 600n, held: 400n });
    const stored = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals/${String(first.json.id)}`,
      WITHDRAWAL,
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
    expect(await apiBalances(app, account.typeId)).toEqual({
      pendingIn: 0n,
      available: 300n,
      held: 0n,
    });

    // Decision 24: only a successful request takes a key. Once funded, the same key works.
    await fundTestAccount(pool, account, 200n);
    const retried = await postWithdrawal(app, account.typeId, withdrawalBody(), key);
    expect(retried.statusCode).toBe(201);
    expect(await apiBalances(app, account.typeId)).toMatchObject({ available: 100n, held: 400n });
  });

  it('lets exactly one of two concurrent withdrawals that together exceed available through', async () => {
    const account = await fundedAccount(1000n);
    const responses = await Promise.all([
      postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '700' }), randomUUID()),
      postWithdrawal(app, account.typeId, withdrawalBody({ amount_minor: '700' }), randomUUID()),
    ]);

    expect(responses.map((r) => r.statusCode).sort((a, b) => a - b)).toEqual([201, 422]);
    expect(await withdrawalsOf(account)).toBe(1);
    expect(await apiBalances(app, account.typeId)).toEqual({
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
      // Decision 60: above the SEPA Credit Transfer maximum (a 500 before, from an overflow).
      [
        'amount_minor above the SEPA maximum',
        { amount_minor: '100000000000' },
        'key',
        'amount_minor',
      ],
      ['the int8 maximum', { amount_minor: '9223372036854775807' }, 'key', 'amount_minor'],
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

// A withdrawal of 100 requested through the API on the account, then taken to `status` with the
// ledger's own functions (what the submitter and the provider's webhook do).
async function withdrawalIn(
  account: TestAccount,
  status: 'held' | 'submitted' | 'executed' | 'failed',
): Promise<string> {
  const created = await postWithdrawal(
    app,
    account.typeId,
    withdrawalBody({ amount_minor: '100' }),
    randomUUID(),
  );
  const id = withdrawalIdOf(created);
  if (status === 'submitted') await withTransaction(pool, (tx) => markSubmitted(tx, id, 'tr_1'));
  if (status === 'executed') await withTransaction(pool, (tx) => execute(tx, id));
  if (status === 'failed') {
    await withTransaction(pool, (tx) => release(tx, id, 'rejected by the beneficiary bank'));
  }
  return String(created.json.id);
}

function listedIds(response: { json: Record<string, unknown> }): unknown[] {
  return (response.json.data as { id: string }[]).map((w) => w.id);
}

describe('GET /accounts/:account_id/withdrawals/:withdrawal_id', () => {
  it('returns the withdrawal, and its history with ?include=history', async () => {
    const account = await fundedAccount(1000n);
    const created = await postWithdrawal(app, account.typeId, withdrawalBody(), randomUUID());
    const url = `/accounts/${account.typeId}/withdrawals/${String(created.json.id)}`;

    const plain = await getJson(app, url, WITHDRAWAL);
    expect(plain.statusCode).toBe(200);
    expect(plain.body).toBe(created.body); // the same representation as the POST's, byte for byte

    const withHistory = await getJson(app, `${url}?include=history`, WITHDRAWAL);
    expect(withHistory.json).toEqual({
      ...created.json,
      history: [{ from_status: null, to_status: 'held', reason: null, at: expect.any(String) }],
    });
  });

  // The provider's reason is meant for the customer (slice 4 decision 76): shown in the body and
  // on the history's change to failed.
  it("shows a failed withdrawal's provider reason in its body and its history", async () => {
    const account = await fundedAccount(1000n);
    const id = await withdrawalIn(account, 'failed');
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals/${id}?include=history`,
      WITHDRAWAL,
    );
    expect(response.json.failure_reason).toBe('rejected by the beneficiary bank');
    expect(
      (response.json.history as Record<string, unknown>[]).map((h) => [h.to_status, h.reason]),
    ).toEqual([
      ['held', null],
      ['failed', 'rejected by the beneficiary bank'],
    ]);
  });

  // Slice 4 decision 64: the account in the path is a scope, decided by the one query.
  it("answers 404 withdrawal-not-found for another account's withdrawal", async () => {
    const owner = await fundedAccount(1000n);
    const other = await createTestAccount(pool);
    const id = await withdrawalIn(owner, 'held');
    const response = await getJson(app, `/accounts/${other.typeId}/withdrawals/${id}`, WITHDRAWAL);
    expect(response.statusCode).toBe(404);
    expect(response.json).toEqual(WITHDRAWAL_NOT_FOUND);
  });

  it('answers 404 withdrawal-not-found for a real withdrawal under an unknown account', async () => {
    const owner = await fundedAccount(1000n);
    const id = await withdrawalIn(owner, 'held');
    const unknown = toTypeId('acct', newUuid('acct'));
    const response = await getJson(app, `/accounts/${unknown}/withdrawals/${id}`, WITHDRAWAL);
    expect(response.statusCode).toBe(404);
    expect(response.json).toEqual(WITHDRAWAL_NOT_FOUND);
  });

  it('answers 404 for an unknown withdrawal and 400 for an invalid ID', async () => {
    const account = await createTestAccount(pool);
    const missing = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals/${toTypeId('wd', newUuid('wd'))}`,
      WITHDRAWAL,
    );
    expect(missing.statusCode).toBe(404);
    expect(missing.json).toEqual(WITHDRAWAL_NOT_FOUND);

    const invalid = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals/${toTypeId('dep', newUuid('dep'))}`,
      WITHDRAWAL,
    );
    expect(invalid.statusCode).toBe(400);
  });

  // The single read has no list: the list's parameters are unknown to it, so ignored.
  it.each(['limit=0', 'before=nope', 'status=nope'])('ignores %s', async (query) => {
    const account = await fundedAccount(1000n);
    const id = await withdrawalIn(account, 'held');
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals/${id}?${query}`,
      WITHDRAWAL,
    );
    expect(response.statusCode).toBe(200);
  });

  // Slice 4 decision 68: both IDs and the query at once, in the contract's order.
  it('reports both invalid IDs and an invalid include at once', async () => {
    const response = await getJson(
      app,
      `/accounts/nope/withdrawals/${toTypeId('dep', newUuid('dep'))}?include=everything`,
      WITHDRAWAL,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json).toEqual({
      type: 'urn:ledger-demo:problem:invalid-request',
      title: 'Invalid request',
      status: 400,
      errors: [
        { path: 'account_id', message: 'must be a acct_ ID' },
        { path: 'withdrawal_id', message: 'must be a wd_ ID' },
        { path: 'include', message: 'Invalid input: expected "history"' },
      ],
    });
  });
});

describe('GET /accounts/:account_id/withdrawals', () => {
  it("lists only this account's withdrawals, newest first, as the single read shows them", async () => {
    const account = await fundedAccount(1000n);
    const other = await fundedAccount(1000n);
    const first = await withdrawalIn(account, 'executed');
    await withdrawalIn(other, 'held');
    const second = await withdrawalIn(account, 'held');

    const response = await getJson(app, `/accounts/${account.typeId}/withdrawals`, WITHDRAWALS);
    expect(response.statusCode).toBe(200);
    expect(listedIds(response)).toEqual([second, first]);
    expect(response.json.next_before).toBeNull();
    const single = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals/${second}`,
      WITHDRAWAL,
    );
    expect((response.json.data as unknown[])[0]).toEqual(single.json);
    expect((response.json.data as Record<string, unknown>[])[0]).not.toHaveProperty('history');
  });

  it('returns an empty list for an account without withdrawals, and 404 for an unknown one', async () => {
    const account = await createTestAccount(pool);
    const empty = await getJson(app, `/accounts/${account.typeId}/withdrawals`, WITHDRAWALS);
    expect(empty.json).toEqual({ data: [], next_before: null });

    const unknown = await getJson(
      app,
      `/accounts/${toTypeId('acct', newUuid('acct'))}/withdrawals`,
      WITHDRAWALS,
    );
    expect(unknown.statusCode).toBe(404);
    expect(unknown.json.type).toBe('urn:ledger-demo:problem:account-not-found');
  });

  it('pages through the withdrawals with limit and before', async () => {
    const account = await fundedAccount(1000n);
    const created: string[] = [];
    for (let i = 0; i < 5; i++) created.push(await withdrawalIn(account, 'held'));
    const newestFirst = [...created].reverse();

    const seen: unknown[] = [];
    const cursors: unknown[] = [];
    let url = `/accounts/${account.typeId}/withdrawals?limit=2`;
    for (;;) {
      const page = await getJson(app, url, WITHDRAWALS);
      seen.push(...listedIds(page));
      const next = page.json.next_before;
      cursors.push(next);
      if (typeof next !== 'string') break;
      url = `/accounts/${account.typeId}/withdrawals?limit=2&before=${next}`;
    }
    expect(seen).toEqual(newestFirst);
    expect(cursors).toEqual([newestFirst[1], newestFirst[3], null]);
  });

  it('includes each withdrawal’s history with include=history', async () => {
    const account = await fundedAccount(1000n);
    const failed = await withdrawalIn(account, 'failed');
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals?include=history`,
      WITHDRAWALS,
    );
    const [item] = response.json.data as { id: string; history: Record<string, unknown>[] }[];
    expect(item?.id).toBe(failed);
    expect(item?.history.map((h) => [h.from_status, h.to_status, h.reason])).toEqual([
      [null, 'held', null],
      ['held', 'failed', 'rejected by the beneficiary bank'],
    ]);
  });

  it.each(['held', 'submitted', 'executed', 'failed'] as const)(
    'filters by status=%s',
    async (status) => {
      const account = await fundedAccount(1000n);
      const byStatus: Record<string, string> = {};
      for (const s of ['held', 'submitted', 'executed', 'failed'] as const) {
        byStatus[s] = await withdrawalIn(account, s);
      }
      const response = await getJson(
        app,
        `/accounts/${account.typeId}/withdrawals?status=${status}`,
        WITHDRAWALS,
      );
      expect(listedIds(response)).toEqual([byStatus[status]]);
    },
  );

  // limit and before apply within the filtered list; next_before is the last withdrawal of the
  // filtered page, so following it skips the withdrawals the filter leaves out.
  it('pages through a filtered list with its own cursors', async () => {
    const account = await fundedAccount(1000n);
    const executed: string[] = [];
    for (let i = 0; i < 5; i++) {
      executed.push(await withdrawalIn(account, 'executed'));
      await withdrawalIn(account, 'held'); // between them
    }
    const newestFirst = [...executed].reverse();

    const seen: unknown[] = [];
    const cursors: unknown[] = [];
    let url = `/accounts/${account.typeId}/withdrawals?status=executed&limit=2`;
    for (;;) {
      const page = await getJson(app, url, WITHDRAWALS);
      seen.push(...listedIds(page));
      const next = page.json.next_before;
      cursors.push(next);
      if (typeof next !== 'string') break;
      url = `/accounts/${account.typeId}/withdrawals?status=executed&limit=2&before=${next}`;
    }
    expect(seen).toEqual(newestFirst);
    expect(cursors).toEqual([newestFirst[1], newestFirst[3], null]);
  });

  it.each([
    ['limit=0', 'limit', 'Too small: expected number to be >=1'],
    ['limit=201', 'limit', 'Too big: expected number to be <=200'],
    ['limit=lots', 'limit', 'Invalid input: expected number, received NaN'],
    [`before=${toTypeId('dep', newUuid('dep'))}`, 'before', 'must be a wd_ ID'],
    ['before=', 'before', 'must be a wd_ ID'],
    [
      `before=${toTypeId('wd', newUuid('wd'))}&before=${toTypeId('wd', newUuid('wd'))}`,
      'before',
      'Invalid input: expected string, received array',
    ],
    ['include=everything', 'include', 'Invalid input: expected "history"'],
    ['status=nope', 'status', STATUS_MESSAGE],
    ['status=', 'status', STATUS_MESSAGE],
    ['status=HELD', 'status', STATUS_MESSAGE],
    ['status=held&status=failed', 'status', STATUS_MESSAGE],
  ])('answers 400 for %s', async (query, path, message) => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals?${query}`,
      WITHDRAWALS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json.errors).toEqual([{ path, message }]);
  });

  // Slice 4 decision 68: every problem at once, in the contract's parameter order (limit,
  // before, include, status), whatever order the query sends them in.
  it("reports every problem at once, in the contract's order", async () => {
    const response = await getJson(
      app,
      '/accounts/nope/withdrawals?status=nope&include=x&before=nope&limit=0',
      WITHDRAWALS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json).toEqual({
      type: 'urn:ledger-demo:problem:invalid-request',
      title: 'Invalid request',
      status: 400,
      errors: [
        { path: 'account_id', message: 'must be a acct_ ID' },
        { path: 'limit', message: 'Too small: expected number to be >=1' },
        { path: 'before', message: 'must be a wd_ ID' },
        { path: 'include', message: 'Invalid input: expected "history"' },
        { path: 'status', message: STATUS_MESSAGE },
      ],
    });
  });

  it('ignores unknown query parameters', async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/withdrawals?sort=asc&network_transaction_id=x`,
      WITHDRAWALS,
    );
    expect(response.statusCode).toBe(200);
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
