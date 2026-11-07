import { afterAll, describe, expect, it } from 'vitest';
import { decide, startReview } from '../../src/compliance/transitions.ts';
import { book, postDecision } from '../../src/ledger/posting.ts';
import { buildApp } from '../../src/api/app.ts';
import { createPool, withTransaction } from '../../src/shared/db.ts';
import { fromTypeId, newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import {
  ALICE,
  capturingLogger,
  createTestAccount,
  createTestApp,
  createTestDeposit,
  getJson,
  type TestAccount,
} from '../helpers.ts';

// GET /accounts/:id and /accounts/:id/balance against the test database. Deposits are put through
// the lifecycle with the ledger and compliance functions directly (no deposit-worker needed). Every
// response is checked against contracts/openapi.yaml by getJson. The deposits are in
// deposits.test.ts.

const testApp = await createTestApp();
const { app, pool } = testApp;
afterAll(testApp.close);

const ACCOUNT = '/accounts/{account_id}';
const BALANCE = '/accounts/{account_id}/balance';

function balanceOf(account: TestAccount): Promise<Awaited<ReturnType<typeof getJson>>> {
  return getJson(app, `/accounts/${account.typeId}/balance`, BALANCE);
}

// The account's created_at as the API writes it, read from the database (set when it was inserted).
async function createdAt(id: Uuid<'acct'>): Promise<string> {
  const result = await pool.query<{ created_at: Date }>(
    'SELECT created_at FROM accounts WHERE id = $1',
    [id],
  );
  const row = result.rows[0];
  if (row === undefined) throw new Error(`no account ${id}`);
  return row.created_at.toISOString();
}

describe('GET /accounts/:id', () => {
  // Byte for byte, in this order (Go's must match): id, name, iban, created_at.
  it('returns a seeded account', async () => {
    const response = await getJson(app, `/accounts/${ALICE.id}`, ACCOUNT);
    expect(response.statusCode).toBe(200);
    expect(response.body).toBe(
      JSON.stringify({
        id: ALICE.id,
        name: 'alice',
        iban: ALICE.iban,
        created_at: await createdAt(fromTypeId('acct', ALICE.id)),
      }),
    );
  });

  it('returns a test account', async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(app, `/accounts/${account.typeId}`, ACCOUNT);
    expect(response.statusCode).toBe(200);
    expect(response.body).toBe(
      JSON.stringify({
        id: account.typeId,
        name: `test-${account.id}`,
        iban: account.iban,
        created_at: await createdAt(account.id),
      }),
    );
  });

  it('answers 404 for an unknown account', async () => {
    const response = await getJson(app, `/accounts/${toTypeId('acct', newUuid('acct'))}`, ACCOUNT);
    expect(response.statusCode).toBe(404);
    expect(response.json).toEqual({
      type: 'urn:ledger-demo:problem:account-not-found',
      title: 'Account not found',
      status: 404,
    });
  });

  it.each(['acct_garbage', toTypeId('dep', newUuid('dep')), 'alice'])(
    'answers 400 for account_id %s',
    async (id) => {
      const response = await getJson(app, `/accounts/${id}`, ACCOUNT);
      expect(response.statusCode).toBe(400);
      expect(response.json).toEqual({
        type: 'urn:ledger-demo:problem:invalid-request',
        title: 'Invalid request',
        status: 400,
        errors: [{ path: 'account_id', message: 'must be a acct_ ID' }],
      });
    },
  );

  it('answers HEAD with the headers only', async () => {
    const get = await app.inject({ method: 'GET', url: `/accounts/${ALICE.id}` });
    const head = await app.inject({ method: 'HEAD', url: `/accounts/${ALICE.id}` });
    expect(head.statusCode).toBe(200);
    expect(head.body).toBe('');
    expect(head.headers['content-length']).toBe(String(Buffer.byteLength(get.body)));
    expect(head.headers['content-type']).toBe('application/json; charset=utf-8');
  });
});

describe('GET /accounts/:id/balance', () => {
  // The body byte for byte: account_id first (the contract's order), as Go writes it.
  it('starts at zero', async () => {
    const account = await createTestAccount(pool);
    const response = await balanceOf(account);
    expect(response.statusCode).toBe(200);
    expect(response.body).toBe(
      JSON.stringify({
        account_id: account.typeId,
        pending_in: '0',
        available: '0',
        held: '0',
        currency: 'EUR',
      }),
    );
  });

  it('shows funds in pending_in once booked, then in available once credited', async () => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account);

    await withTransaction(pool, (tx) => book(tx, deposit.id));
    expect((await balanceOf(account)).json).toMatchObject({ pending_in: '1250', available: '0' });

    await withTransaction(pool, (tx) => startReview(tx, deposit.id));
    await withTransaction(pool, (tx) => decide(tx, deposit.id, { outcome: 'approved' }));
    await withTransaction(pool, (tx) => postDecision(tx, deposit.id));
    expect((await balanceOf(account)).json).toMatchObject({ pending_in: '0', available: '1250' });
  });

  it('keeps amounts beyond 2^53 exact', async () => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account, 9007199254740993n);
    await withTransaction(pool, (tx) => book(tx, deposit.id));
    expect((await balanceOf(account)).json.pending_in).toBe('9007199254740993');
  });

  it('answers 404 for an unknown account', async () => {
    const response = await getJson(
      app,
      `/accounts/${toTypeId('acct', newUuid('acct'))}/balance`,
      BALANCE,
    );
    expect(response.statusCode).toBe(404);
    expect(response.json.type).toBe('urn:ledger-demo:problem:account-not-found');
  });

  it.each(['acct_garbage', toTypeId('dep', newUuid('dep')), 'alice'])(
    'answers 400 for account_id %s',
    async (id) => {
      const response = await getJson(app, `/accounts/${id}/balance`, BALANCE);
      expect(response.statusCode).toBe(400);
      expect(response.json).toMatchObject({
        type: 'urn:ledger-demo:problem:invalid-request',
        errors: [{ path: 'account_id', message: 'must be a acct_ ID' }],
      });
      // The message doesn't repeat the input.
      expect(response.body).not.toContain(id);
    },
  );
});

describe('an unexpected error on a GET', () => {
  // The database unreachable (a pool on a port nothing listens on): the error handler's 500
  // problem, which the contract documents for the GET operations too (getJson checks it).
  it('answers 500 with a problem', async () => {
    const own = capturingLogger();
    const pool = createPool({
      databaseUrl: 'postgres://ledger:ledger@127.0.0.1:1/none_test',
      logger: own.logger,
    });
    const broken = buildApp({ pool, logger: own.logger });
    try {
      const response = await getJson(
        broken,
        `/accounts/${toTypeId('acct', newUuid('acct'))}/balance`,
        BALANCE,
      );
      expect(response.statusCode).toBe(500);
      expect(response.json).toEqual({
        type: 'about:blank',
        title: 'Internal Server Error',
        status: 500,
      });
    } finally {
      await broken.close();
      await pool.end();
    }
  });
});
