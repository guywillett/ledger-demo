import { afterAll, describe, expect, it } from 'vitest';
import { decide, startReview, type Decision } from '../../src/compliance/transitions.ts';
import type { Payment } from '../../src/ingestion/payments.ts';
import { book, postDecision } from '../../src/ledger/posting.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import {
  createTestAccount,
  createTestApp,
  createTestPayment,
  getJson,
  type TestAccount,
} from '../helpers.ts';

// GET /accounts/:id/balance and /transactions against the test database. Payments are put
// through the lifecycle with the ledger and compliance functions directly (no worker needed).
// Every response is checked against contracts/openapi.yaml by getJson.

const testApp = await createTestApp();
const { app, pool } = testApp;
afterAll(testApp.close);

const BALANCE = '/accounts/{account_id}/balance';
const TRANSACTIONS = '/accounts/{account_id}/transactions';

async function process(payment: Payment, decision: Decision): Promise<void> {
  await withTransaction(pool, (tx) => book(tx, payment.id));
  await withTransaction(pool, (tx) => startReview(tx, payment.id));
  await withTransaction(pool, (tx) => decide(tx, payment.id, decision));
  await withTransaction(pool, (tx) => postDecision(tx, payment.id));
}

function balanceOf(account: TestAccount): Promise<Awaited<ReturnType<typeof getJson>>> {
  return getJson(app, `/accounts/${account.typeId}/balance`, BALANCE);
}

describe('GET /accounts/:id/balance', () => {
  it('starts at zero', async () => {
    const account = await createTestAccount(pool);
    const response = await balanceOf(account);
    expect(response.statusCode).toBe(200);
    expect(response.json).toEqual({ pending_in: '0', available: '0', currency: 'EUR' });
  });

  it('shows funds in pending_in once booked, then in available once credited', async () => {
    const account = await createTestAccount(pool);
    const payment = await createTestPayment(pool, account);

    await withTransaction(pool, (tx) => book(tx, payment.id));
    expect((await balanceOf(account)).json).toMatchObject({ pending_in: '1250', available: '0' });

    await withTransaction(pool, (tx) => startReview(tx, payment.id));
    await withTransaction(pool, (tx) => decide(tx, payment.id, { outcome: 'approved' }));
    await withTransaction(pool, (tx) => postDecision(tx, payment.id));
    expect((await balanceOf(account)).json).toMatchObject({ pending_in: '0', available: '1250' });
  });

  it('keeps amounts beyond 2^53 exact', async () => {
    const account = await createTestAccount(pool);
    const payment = await createTestPayment(pool, account, 9007199254740993n);
    await withTransaction(pool, (tx) => book(tx, payment.id));
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

  it.each(['acct_garbage', toTypeId('pay', newUuid('pay')), 'alice'])(
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

describe('GET /accounts/:id/transactions', () => {
  it('lists only this account’s payments, newest first, with their current status', async () => {
    const account = await createTestAccount(pool);
    const other = await createTestAccount(pool);
    const first = await createTestPayment(pool, account, 100n);
    await createTestPayment(pool, other, 999n);
    const second = await createTestPayment(pool, account, 200n);
    await process(first, { outcome: 'approved' });

    const response = await getJson(app, `/accounts/${account.typeId}/transactions`, TRANSACTIONS);
    expect(response.statusCode).toBe(200);
    const payments = response.json.payments as Record<string, unknown>[];
    expect(payments.map((p) => [p.id, p.amount_minor, p.status])).toEqual([
      [toTypeId('pay', second.id), '200', 'created'],
      [toTypeId('pay', first.id), '100', 'credited'],
    ]);
    expect(payments[0]).not.toHaveProperty('history');
    expect(response.json.next_before).toBeNull();
  });

  it('returns an empty page for an account without payments, and 404 for an unknown one', async () => {
    const account = await createTestAccount(pool);
    const empty = await getJson(app, `/accounts/${account.typeId}/transactions`, TRANSACTIONS);
    expect(empty.json).toEqual({ payments: [], next_before: null });

    const unknown = await getJson(
      app,
      `/accounts/${toTypeId('acct', newUuid('acct'))}/transactions`,
      TRANSACTIONS,
    );
    expect(unknown.statusCode).toBe(404);
  });

  it('pages through the payments with limit and before', async () => {
    const account = await createTestAccount(pool);
    const created: Payment[] = [];
    for (let i = 0; i < 5; i++) created.push(await createTestPayment(pool, account));
    const newestFirst = created.map((p) => toTypeId('pay', p.id)).reverse();

    const seen: unknown[] = [];
    const cursors: unknown[] = [];
    let url = `/accounts/${account.typeId}/transactions?limit=2`;
    for (;;) {
      const page = await getJson(app, url, TRANSACTIONS);
      seen.push(...(page.json.payments as { id: string }[]).map((p) => p.id));
      const next = page.json.next_before;
      cursors.push(next);
      if (typeof next !== 'string') break; // null on the last page
      url = `/accounts/${account.typeId}/transactions?limit=2&before=${next}`;
    }
    expect(seen).toEqual(newestFirst);
    expect(cursors).toEqual([newestFirst[1], newestFirst[3], null]);
  });

  it('includes the status history with include=history', async () => {
    const account = await createTestAccount(pool);
    const approved = await createTestPayment(pool, account);
    const rejected = await createTestPayment(pool, account);
    await process(approved, { outcome: 'approved' });
    await process(rejected, { outcome: 'rejected', reason: 'sanctions screening hit' });

    const response = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?include=history`,
      TRANSACTIONS,
    );
    const byId = new Map(
      (response.json.payments as { id: string; history: Record<string, unknown>[] }[]).map((p) => [
        p.id,
        p.history,
      ]),
    );
    expect(
      byId.get(toTypeId('pay', approved.id))?.map((h) => [h.from_status, h.to_status]),
    ).toEqual([
      [null, 'created'],
      ['created', 'received'],
      ['received', 'under_review'],
      ['under_review', 'approved'],
      ['approved', 'credited'],
    ]);
    expect(byId.get(toTypeId('pay', rejected.id))?.[3]).toMatchObject({
      from_status: 'under_review',
      to_status: 'rejected',
      reason: 'sanctions screening hit',
    });
  });

  it.each([
    ['limit=0', 'limit'],
    ['limit=201', 'limit'],
    ['limit=lots', 'limit'],
    [`before=${toTypeId('acct', newUuid('acct'))}`, 'before'],
    ['include=everything', 'include'],
  ])('answers 400 for %s', async (query, path) => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?${query}`,
      TRANSACTIONS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json.errors).toEqual([expect.objectContaining({ path })]);
  });

  it('ignores unknown query parameters', async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?sort=asc`,
      TRANSACTIONS,
    );
    expect(response.statusCode).toBe(200);
  });
});
