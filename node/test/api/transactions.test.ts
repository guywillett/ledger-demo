import { randomUUID } from 'node:crypto';
import type pg from 'pg';
import { afterAll, describe, expect, it } from 'vitest';
import { buildApp } from '../../src/api/app.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import {
  capturingLogger,
  createTestAccount,
  createTestApp,
  createTestDeposit,
  fundTestAccount,
  getJson,
  postWithdrawal,
  withdrawalBody,
  type TestAccount,
} from '../helpers.ts';

// GET /accounts/:id/transactions (slice 4 decision 67): the account's deposits and withdrawals
// together, newest first, each item exactly what its own endpoint returns. Every response is
// checked against contracts/openapi.yaml by getJson.

const testApp = await createTestApp();
const { app, pool } = testApp;
afterAll(testApp.close);

const TRANSACTIONS = '/accounts/{account_id}/transactions';
const CURSOR_MESSAGE = 'must be a dep_ or wd_ ID';

type Item = { type: string; id: string };

function items(response: { json: Record<string, unknown> }): Item[] {
  return response.json.data as Item[];
}

async function deposit(account: TestAccount): Promise<string> {
  return toTypeId('dep', (await createTestDeposit(pool, account, 100n)).id);
}

async function withdrawal(account: TestAccount): Promise<string> {
  const created = await postWithdrawal(
    app,
    account.typeId,
    withdrawalBody({ amount_minor: '100' }),
    randomUUID(),
  );
  return String(created.json.id);
}

// An account with deposits and withdrawals interleaved, oldest first: the funding deposit, then
// deposit, withdrawal, deposit, withdrawal. Returns the account and the IDs newest first.
async function interleaved(): Promise<{ account: TestAccount; newestFirst: string[] }> {
  const account = await createTestAccount(pool);
  const funding = toTypeId('dep', (await fundTestAccount(pool, account, 1000n)).id);
  const created: string[] = [funding];
  created.push(await deposit(account));
  created.push(await withdrawal(account));
  created.push(await deposit(account));
  created.push(await withdrawal(account));
  return { account, newestFirst: created.reverse() };
}

// Every page of the account's list with the query, following next_before: the IDs seen and the
// cursors.
async function walk(
  account: TestAccount,
  query: string,
): Promise<{ seen: string[]; cursors: unknown[] }> {
  const seen: string[] = [];
  const cursors: unknown[] = [];
  let url = `/accounts/${account.typeId}/transactions?${query}`;
  for (;;) {
    const page = await getJson(app, url, TRANSACTIONS);
    expect(page.statusCode).toBe(200);
    seen.push(...items(page).map((i) => i.id));
    const next = page.json.next_before;
    cursors.push(next);
    if (typeof next !== 'string') return { seen, cursors };
    url = `/accounts/${account.typeId}/transactions?${query}&before=${next}`;
  }
}

describe('GET /accounts/:id/transactions', () => {
  it('lists deposits and withdrawals together, newest first, by when each was created', async () => {
    const { account, newestFirst } = await interleaved();
    const response = await getJson(app, `/accounts/${account.typeId}/transactions`, TRANSACTIONS);
    expect(response.statusCode).toBe(200);
    expect(items(response).map((i) => [i.type, i.id])).toEqual(
      newestFirst.map((id) => [id.startsWith('dep_') ? 'deposit' : 'withdrawal', id]),
    );
    expect(response.json.next_before).toBeNull();
  });

  it('gives each item exactly as its own endpoint does, byte for byte', async () => {
    const { account } = await interleaved();
    const response = await getJson(app, `/accounts/${account.typeId}/transactions`, TRANSACTIONS);
    const bodies: string[] = [];
    for (const item of items(response)) {
      const kind = item.type === 'deposit' ? 'deposits' : 'withdrawals';
      const own = await app.inject({
        method: 'GET',
        url: `/accounts/${account.typeId}/${kind}/${item.id}`,
      });
      expect(JSON.stringify(item)).toBe(own.body);
      bodies.push(own.body);
    }
    // And the list's body is exactly those bodies, in order.
    expect(response.body).toBe(`{"data":[${bodies.join(',')}],"next_before":null}`);
  });

  // limit=1 over deposit, withdrawal, deposit, withdrawal: every page ends on one kind and the next
  // begins on the other, and the cursors are of both kinds.
  it('pages across kinds, with a cursor of either kind', async () => {
    const { account, newestFirst } = await interleaved();
    const { seen, cursors } = await walk(account, 'limit=1');
    expect(seen).toEqual(newestFirst);
    expect(cursors).toEqual([...newestFirst.slice(0, -1), null]);
    expect(cursors.some((c) => typeof c === 'string' && c.startsWith('wd_'))).toBe(true);
    expect(cursors.some((c) => typeof c === 'string' && c.startsWith('dep_'))).toBe(true);
  });

  it('pages in twos, a page ending on a withdrawal and the next on a deposit, and the reverse', async () => {
    const { account, newestFirst } = await interleaved();
    const { seen, cursors } = await walk(account, 'limit=2');
    expect(seen).toEqual(newestFirst);
    // [W, D] [W, D] [D]: the first cursor is a deposit and the next page begins with a withdrawal.
    expect(cursors).toEqual([newestFirst[1], newestFirst[3], null]);
  });

  // limit + 1 detection: exactly limit items is one page with no next; one fewer leaves one over.
  it('ends exactly at the page boundary', async () => {
    const { account, newestFirst } = await interleaved();
    const exact = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?limit=${newestFirst.length}`,
      TRANSACTIONS,
    );
    expect(items(exact)).toHaveLength(newestFirst.length);
    expect(exact.json.next_before).toBeNull();

    const short = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?limit=${newestFirst.length - 1}`,
      TRANSACTIONS,
    );
    expect(short.json.next_before).toBe(newestFirst[newestFirst.length - 2]);
    const rest = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?limit=${newestFirst.length - 1}&before=${String(short.json.next_before)}`,
      TRANSACTIONS,
    );
    expect(items(rest).map((i) => i.id)).toEqual([newestFirst.at(-1)]);
    expect(rest.json.next_before).toBeNull();
  });

  // A withdrawal needs funds, so an account can't have withdrawals without a deposit: the one-kind
  // case the API can produce is deposits only.
  it('lists an account with deposits only', async () => {
    const account = await createTestAccount(pool);
    const first = await deposit(account);
    const second = await deposit(account);
    const response = await getJson(app, `/accounts/${account.typeId}/transactions`, TRANSACTIONS);
    expect(items(response).map((i) => [i.type, i.id])).toEqual([
      ['deposit', second],
      ['deposit', first],
    ]);
  });

  it('answers an empty list for an account without transactions, and 404 for an unknown one', async () => {
    const account = await createTestAccount(pool);
    const empty = await getJson(app, `/accounts/${account.typeId}/transactions`, TRANSACTIONS);
    expect(empty.statusCode).toBe(200);
    expect(empty.body).toBe('{"data":[],"next_before":null}');

    const unknown = await getJson(
      app,
      `/accounts/${toTypeId('acct', newUuid('acct'))}/transactions`,
      TRANSACTIONS,
    );
    expect(unknown.statusCode).toBe(404);
    expect(unknown.json.type).toBe('urn:ledger-demo:problem:account-not-found');
  });

  // Decision 66: a cursor is a position in time, not a reference. Another account's newest item
  // as before: every item of this account older than it.
  it('accepts a before of another account, as a position', async () => {
    const { account, newestFirst } = await interleaved();
    const other = await createTestAccount(pool);
    const later = await deposit(other); // newer than everything of account's
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?before=${later}`,
      TRANSACTIONS,
    );
    expect(response.statusCode).toBe(200);
    expect(items(response).map((i) => i.id)).toEqual(newestFirst);
  });

  it.each([
    [`before=${toTypeId('acct', newUuid('acct'))}`, CURSOR_MESSAGE],
    ['before=nope', CURSOR_MESSAGE],
    ['before=', CURSOR_MESSAGE],
    ['before=dep_x', CURSOR_MESSAGE],
    [`before=${toTypeId('dep', newUuid('dep')).toUpperCase()}`, CURSOR_MESSAGE],
    [
      `before=${toTypeId('dep', newUuid('dep'))}&before=${toTypeId('wd', newUuid('wd'))}`,
      'Invalid input: expected string, received array',
    ],
  ])('answers 400 for %s', async (query, message) => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?${query}`,
      TRANSACTIONS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json.errors).toEqual([{ path: 'before', message }]);
  });

  // Decision 68: every problem at once, the path then the query in the contract's order.
  it('reports every problem at once', async () => {
    const response = await getJson(
      app,
      '/accounts/nope/transactions?before=nope&limit=0',
      TRANSACTIONS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json).toEqual({
      type: 'urn:ledger-demo:problem:invalid-request',
      title: 'Invalid request',
      status: 400,
      errors: [
        { path: 'account_id', message: 'must be a acct_ ID' },
        { path: 'limit', message: 'Too small: expected number to be >=1' },
        { path: 'before', message: CURSOR_MESSAGE },
      ],
    });
  });

  // No include and no filters here (decision 67): unknown parameters, ignored, so the items carry
  // no history and nothing is filtered out.
  it('ignores include and the filters', async () => {
    const { account, newestFirst } = await interleaved();
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/transactions?include=history&status=held&include=nope`,
      TRANSACTIONS,
    );
    expect(response.statusCode).toBe(200);
    expect(items(response).map((i) => i.id)).toEqual(newestFirst);
    expect(items(response).some((i) => 'history' in i)).toBe(false);
  });
});

// Decision 67: a row the page was decided on but the read by ID doesn't return is an invariant
// violation, a 500 naming it in the log, never a shorter page. Impossible against the real
// database (rows are never deleted, an account never changes), so a stub pool answers the account
// check and the page's query, then returns no rows for the read by ID.
describe('GET /accounts/:id/transactions when a decided row is missing', () => {
  it('answers 500 and logs the missing IDs', async () => {
    const missing = newUuid('dep');
    const stub = {
      query: async (sql: string) => {
        if (sql.includes('FROM accounts')) return { rowCount: 1, rows: [{}] };
        if (sql.includes('UNION ALL'))
          return { rowCount: 1, rows: [{ type: 'deposit', id: missing }] };
        return { rowCount: 0, rows: [] };
      },
    };
    const own = capturingLogger();
    const broken = buildApp({ pool: stub as unknown as pg.Pool, logger: own.logger });
    try {
      const response = await getJson(
        broken,
        `/accounts/${toTypeId('acct', newUuid('acct'))}/transactions`,
        TRANSACTIONS,
      );
      expect(response.statusCode).toBe(500);
      expect(response.json).toEqual({
        type: 'about:blank',
        title: 'Internal Server Error',
        status: 500,
      });
      const errorLines = own.lines().filter((l) => l.level === 'error');
      expect(errorLines).toHaveLength(1);
      expect(JSON.stringify(errorLines[0])).toContain(toTypeId('dep', missing));
    } finally {
      await broken.close();
    }
  });
});
