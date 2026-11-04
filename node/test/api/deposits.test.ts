import { afterAll, describe, expect, it } from 'vitest';
import { decide, startReview } from '../../src/compliance/transitions.ts';
import type { Decision } from '../../src/compliance/decision.ts';
import type { Deposit } from '../../src/deposits/deposits.ts';
import { book, postDecision } from '../../src/ledger/posting.ts';
import { DEPOSIT_FAILURE_REASON } from '../../src/api/schemas.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import {
  createTestAccount,
  createTestApp,
  createTestDeposit,
  getJson,
  type TestAccount,
} from '../helpers.ts';

// GET /accounts/:id/deposits and /accounts/:id/deposits/:deposit_id against the test database.
// Deposits are put through the lifecycle with the ledger and compliance functions directly (no
// deposit-worker needed). Every response is checked against contracts/openapi.yaml by getJson.

const testApp = await createTestApp();
const { app, pool } = testApp;
afterAll(testApp.close);

const DEPOSITS = '/accounts/{account_id}/deposits';
const DEPOSIT = '/accounts/{account_id}/deposits/{deposit_id}';

// Books, reviews and decides the deposit; then posts the decision (credited or returned) unless
// post is false (approved or rejected, not yet posted).
async function processDeposit(deposit: Deposit, decision: Decision, post = true): Promise<void> {
  await withTransaction(pool, (tx) => book(tx, deposit.id));
  await withTransaction(pool, (tx) => startReview(tx, deposit.id));
  await withTransaction(pool, (tx) => decide(tx, deposit.id, decision));
  if (post) await withTransaction(pool, (tx) => postDecision(tx, deposit.id));
}

function depositUrl(account: TestAccount, deposit: Deposit, query = ''): string {
  return `/accounts/${account.typeId}/deposits/${toTypeId('dep', deposit.id)}${query}`;
}

const DEPOSIT_NOT_FOUND = {
  type: 'urn:ledger-demo:problem:deposit-not-found',
  title: 'Deposit not found',
  status: 404,
};

describe('GET /accounts/:id/deposits', () => {
  it('lists only this account’s deposits, newest first, with their current status', async () => {
    const account = await createTestAccount(pool);
    const other = await createTestAccount(pool);
    const first = await createTestDeposit(pool, account, 100n);
    await createTestDeposit(pool, other, 999n);
    const second = await createTestDeposit(pool, account, 200n);
    await processDeposit(first, { outcome: 'approved' });

    const response = await getJson(app, `/accounts/${account.typeId}/deposits`, DEPOSITS);
    expect(response.statusCode).toBe(200);
    const deposits = response.json.data as Record<string, unknown>[];
    expect(deposits.map((p) => [p.id, p.amount_minor, p.status])).toEqual([
      [toTypeId('dep', second.id), '200', 'created'],
      [toTypeId('dep', first.id), '100', 'credited'],
    ]);
    expect(deposits[0]).not.toHaveProperty('history');
    expect(response.json.next_before).toBeNull();
  });

  it('returns an empty list for an account without deposits, and 404 for an unknown one', async () => {
    const account = await createTestAccount(pool);
    const empty = await getJson(app, `/accounts/${account.typeId}/deposits`, DEPOSITS);
    expect(empty.json).toEqual({ data: [], next_before: null });

    const unknown = await getJson(
      app,
      `/accounts/${toTypeId('acct', newUuid('acct'))}/deposits`,
      DEPOSITS,
    );
    expect(unknown.statusCode).toBe(404);
    expect(unknown.json.type).toBe('urn:ledger-demo:problem:account-not-found');
  });

  it('pages through the deposits with limit and before', async () => {
    const account = await createTestAccount(pool);
    const created: Deposit[] = [];
    for (let i = 0; i < 5; i++) created.push(await createTestDeposit(pool, account));
    const newestFirst = created.map((p) => toTypeId('dep', p.id)).reverse();

    const seen: unknown[] = [];
    const cursors: unknown[] = [];
    let url = `/accounts/${account.typeId}/deposits?limit=2`;
    for (;;) {
      const page = await getJson(app, url, DEPOSITS);
      seen.push(...(page.json.data as { id: string }[]).map((p) => p.id));
      const next = page.json.next_before;
      cursors.push(next);
      if (typeof next !== 'string') break; // null on the last page
      url = `/accounts/${account.typeId}/deposits?limit=2&before=${next}`;
    }
    expect(seen).toEqual(newestFirst);
    expect(cursors).toEqual([newestFirst[1], newestFirst[3], null]);
  });

  // The history's reason is never the compliance decision's (slice 4 decision 76).
  it('includes the status history with include=history, with no reasons', async () => {
    const account = await createTestAccount(pool);
    const approved = await createTestDeposit(pool, account);
    const rejected = await createTestDeposit(pool, account);
    await processDeposit(approved, { outcome: 'approved', reason: 'an internal note' });
    await processDeposit(rejected, { outcome: 'rejected', reason: 'sanctions screening hit' });

    const response = await getJson(
      app,
      `/accounts/${account.typeId}/deposits?include=history`,
      DEPOSITS,
    );
    const byId = new Map(
      (response.json.data as { id: string; history: Record<string, unknown>[] }[]).map((p) => [
        p.id,
        p.history,
      ]),
    );
    expect(
      byId.get(toTypeId('dep', approved.id))?.map((h) => [h.from_status, h.to_status, h.reason]),
    ).toEqual([
      [null, 'created', null],
      ['created', 'received', null],
      ['received', 'under_review', null],
      ['under_review', 'approved', null],
      ['approved', 'credited', null],
    ]);
    expect(
      byId.get(toTypeId('dep', rejected.id))?.map((h) => [h.from_status, h.to_status, h.reason]),
    ).toEqual([
      [null, 'created', null],
      ['created', 'received', null],
      ['received', 'under_review', null],
      ['under_review', 'rejected', null],
      ['rejected', 'returned', null],
    ]);
    expect(response.body).not.toContain('sanctions');
    expect(response.body).not.toContain('internal note');
  });

  it.each([
    ['limit=0', 'limit'],
    ['limit=201', 'limit'],
    ['limit=lots', 'limit'],
    [`before=${toTypeId('acct', newUuid('acct'))}`, 'before'],
    ['include=everything', 'include'],
  ])('answers 400 for %s', async (query, path) => {
    const account = await createTestAccount(pool);
    const response = await getJson(app, `/accounts/${account.typeId}/deposits?${query}`, DEPOSITS);
    expect(response.statusCode).toBe(400);
    expect(response.json.errors).toEqual([expect.objectContaining({ path })]);
  });

  // Slice 4 decision 68: every problem at once, the path first, then the query in the
  // contract's parameter order (limit, before, include), whatever order the query sends them in.
  it("reports every problem at once: the path, then the query in the contract's order", async () => {
    const response = await getJson(
      app,
      '/accounts/nope/deposits?include=everything&before=nope&limit=0',
      DEPOSITS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json).toEqual({
      type: 'urn:ledger-demo:problem:invalid-request',
      title: 'Invalid request',
      status: 400,
      errors: [
        { path: 'account_id', message: 'must be a acct_ ID' },
        { path: 'limit', message: 'Too small: expected number to be >=1' },
        { path: 'before', message: 'must be a dep_ ID' },
        { path: 'include', message: 'Invalid input: expected "history"' },
      ],
    });
  });

  it("reports the query's problems in the contract's order for a valid account too", async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/deposits?include=x&limit=0`,
      DEPOSITS,
    );
    expect(response.json.errors).toEqual([
      expect.objectContaining({ path: 'limit' }),
      expect.objectContaining({ path: 'include' }),
    ]);
  });

  it('ignores unknown query parameters', async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(app, `/accounts/${account.typeId}/deposits?sort=asc`, DEPOSITS);
    expect(response.statusCode).toBe(200);
  });
});

// The list's filters (slice 4 decision 70): exact matches, combined with each other and with
// pagination.
describe('GET /accounts/:id/deposits filtered', () => {
  const STATUS_MESSAGE =
    'Invalid option: expected one of "created"|"received"|"under_review"|"approved"|"rejected"|"credited"|"returned"';

  // An account with a deposit in each of three statuses, newest last.
  async function accountWithStatuses(): Promise<{
    account: TestAccount;
    created: Deposit;
    credited: Deposit;
    returned: Deposit;
  }> {
    const account = await createTestAccount(pool);
    const credited = await createTestDeposit(pool, account);
    await processDeposit(credited, { outcome: 'approved' });
    const returned = await createTestDeposit(pool, account);
    await processDeposit(returned, { outcome: 'rejected', reason: 'sanctions screening hit' });
    const created = await createTestDeposit(pool, account);
    return { account, created, credited, returned };
  }

  function ids(response: { json: Record<string, unknown> }): unknown[] {
    return (response.json.data as { id: string }[]).map((d) => d.id);
  }

  it.each(['created', 'credited', 'returned'] as const)('filters by status=%s', async (status) => {
    const deposits = await accountWithStatuses();
    const response = await getJson(
      app,
      `/accounts/${deposits.account.typeId}/deposits?status=${status}`,
      DEPOSITS,
    );
    expect(response.statusCode).toBe(200);
    expect(ids(response)).toEqual([toTypeId('dep', deposits[status].id)]);
    expect(response.json.next_before).toBeNull();
  });

  it('answers an empty list for a valid status no deposit is in', async () => {
    const { account } = await accountWithStatuses();
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/deposits?status=under_review`,
      DEPOSITS,
    );
    expect(response.json).toEqual({ data: [], next_before: null });
  });

  it('filters by network_transaction_id, an exact match', async () => {
    const { account, credited } = await accountWithStatuses();
    const url = `/accounts/${account.typeId}/deposits?network_transaction_id=`;
    const match = await getJson(
      app,
      url + encodeURIComponent(credited.networkTransactionId),
      DEPOSITS,
    );
    expect(ids(match)).toEqual([toTypeId('dep', credited.id)]);
    // A prefix of it matches nothing: exact, not a search.
    const prefix = await getJson(
      app,
      url + encodeURIComponent(credited.networkTransactionId.slice(0, -1)),
      DEPOSITS,
    );
    expect(prefix.json).toEqual({ data: [], next_before: null });
  });

  it("doesn't find another account's deposit by its network_transaction_id", async () => {
    const { credited } = await accountWithStatuses();
    const other = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${other.typeId}/deposits?network_transaction_id=${encodeURIComponent(credited.networkTransactionId)}`,
      DEPOSITS,
    );
    expect(response.json).toEqual({ data: [], next_before: null });
  });

  it('combines both filters', async () => {
    const { account, credited } = await accountWithStatuses();
    const ntx = encodeURIComponent(credited.networkTransactionId);
    const url = `/accounts/${account.typeId}/deposits?network_transaction_id=${ntx}&status=`;
    expect(ids(await getJson(app, `${url}credited`, DEPOSITS))).toEqual([
      toTypeId('dep', credited.id),
    ]);
    expect(ids(await getJson(app, `${url}returned`, DEPOSITS))).toEqual([]);
  });

  // limit and before apply within the filtered list; next_before is the last deposit of the
  // filtered page, so following it skips the deposits the filter leaves out.
  it('pages through a filtered list with its own cursors', async () => {
    const account = await createTestAccount(pool);
    const credited: Deposit[] = [];
    for (let i = 0; i < 5; i++) {
      const deposit = await createTestDeposit(pool, account);
      await processDeposit(deposit, { outcome: 'approved' });
      credited.push(deposit);
      await createTestDeposit(pool, account); // left created, between them
    }
    const newestFirst = credited.map((d) => toTypeId('dep', d.id)).reverse();

    const seen: unknown[] = [];
    const cursors: unknown[] = [];
    let url = `/accounts/${account.typeId}/deposits?status=credited&limit=2`;
    for (;;) {
      const page = await getJson(app, url, DEPOSITS);
      seen.push(...ids(page));
      const next = page.json.next_before;
      cursors.push(next);
      if (typeof next !== 'string') break;
      url = `/accounts/${account.typeId}/deposits?status=credited&limit=2&before=${next}`;
    }
    expect(seen).toEqual(newestFirst);
    expect(cursors).toEqual([newestFirst[1], newestFirst[3], null]);
  });

  it.each([
    ['status=unmatched', 'status', STATUS_MESSAGE],
    ['status=nope', 'status', STATUS_MESSAGE],
    ['status=', 'status', STATUS_MESSAGE],
    ['status=created&status=credited', 'status', STATUS_MESSAGE],
    [
      'network_transaction_id=',
      'network_transaction_id',
      'Too small: expected string to have >=1 characters',
    ],
    [
      `network_transaction_id=${'x'.repeat(256)}`,
      'network_transaction_id',
      'Too big: expected string to have <=255 characters',
    ],
    [
      'network_transaction_id=a&network_transaction_id=b',
      'network_transaction_id',
      'Invalid input: expected string, received array',
    ],
  ])('answers 400 for %s', async (query, path, message) => {
    const account = await createTestAccount(pool);
    const response = await getJson(app, `/accounts/${account.typeId}/deposits?${query}`, DEPOSITS);
    expect(response.statusCode).toBe(400);
    expect(response.json.errors).toEqual([{ path, message }]);
  });

  // 255 characters counted as Zod counts them, in code points: 255 emoji are valid.
  it('accepts a network_transaction_id of 255 characters, counted in code points', async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/deposits?network_transaction_id=${encodeURIComponent('😀'.repeat(255))}`,
      DEPOSITS,
    );
    expect(response.statusCode).toBe(200);
  });

  // Slice 4 decision 68: every problem at once, in the contract's parameter order (limit,
  // before, include, status, network_transaction_id), whatever order the query sends them in.
  it('reports every problem at once, in the contract’s order', async () => {
    const response = await getJson(
      app,
      '/accounts/nope/deposits?network_transaction_id=&status=nope&include=x&before=nope&limit=0',
      DEPOSITS,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json.errors).toEqual([
      { path: 'account_id', message: 'must be a acct_ ID' },
      { path: 'limit', message: 'Too small: expected number to be >=1' },
      { path: 'before', message: 'must be a dep_ ID' },
      { path: 'include', message: 'Invalid input: expected "history"' },
      { path: 'status', message: STATUS_MESSAGE },
      {
        path: 'network_transaction_id',
        message: 'Too small: expected string to have >=1 characters',
      },
    ]);
  });
});

describe('GET /accounts/:id/deposits/:deposit_id', () => {
  // Byte for byte, in this order (Go's must match): type first, no provider_account_id.
  it('returns the deposit', async () => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account, 1250n);
    const response = await getJson(app, depositUrl(account, deposit), DEPOSIT);
    expect(response.statusCode).toBe(200);
    expect(response.body).toBe(
      JSON.stringify({
        type: 'deposit',
        id: toTypeId('dep', deposit.id),
        account_id: account.typeId,
        creditor_iban: account.iban,
        amount_minor: '1250',
        currency: 'EUR',
        network: 'SEPA',
        network_transaction_id: deposit.networkTransactionId,
        status: 'created',
        failure_reason: null,
        notification_count: 1,
        created_at: deposit.createdAt.toISOString(),
        updated_at: deposit.updatedAt.toISOString(),
      }),
    );
  });

  it('is the same representation as in the list', async () => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account);
    const single = await getJson(app, depositUrl(account, deposit, '?include=history'), DEPOSIT);
    const list = await getJson(
      app,
      `/accounts/${account.typeId}/deposits?include=history`,
      DEPOSITS,
    );
    expect((list.json.data as unknown[])[0]).toEqual(single.json);
  });

  it('includes its status history with include=history', async () => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account);
    await withTransaction(pool, (tx) => book(tx, deposit.id));
    const response = await getJson(app, depositUrl(account, deposit, '?include=history'), DEPOSIT);
    expect(response.json.history).toEqual([
      { from_status: null, to_status: 'created', reason: null, at: expect.any(String) },
      { from_status: 'created', to_status: 'received', reason: null, at: expect.any(String) },
    ]);
  });

  // Slice 4 decisions 69 and 76: one neutral text for both failure statuses, whatever the
  // internal reason; null for every other status.
  it.each([
    ['created', undefined, null],
    ['approved', { decision: { outcome: 'approved' }, post: false }, null],
    ['credited', { decision: { outcome: 'approved' }, post: true }, null],
    [
      'rejected',
      { decision: { outcome: 'rejected', reason: 'sanctions screening hit' }, post: false },
      DEPOSIT_FAILURE_REASON,
    ],
    [
      'returned',
      { decision: { outcome: 'rejected', reason: 'sanctions screening hit' }, post: true },
      DEPOSIT_FAILURE_REASON,
    ],
  ] as const)('gives a %s deposit the failure_reason %s', async (status, steps, failureReason) => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account);
    if (steps !== undefined) await processDeposit(deposit, steps.decision, steps.post);
    const response = await getJson(app, depositUrl(account, deposit), DEPOSIT);
    expect(response.json).toMatchObject({ status, failure_reason: failureReason });
    expect(response.body).not.toContain('sanctions');
  });

  it('uses "returned to the sender" as the neutral text', () => {
    expect(DEPOSIT_FAILURE_REASON).toBe('returned to the sender');
  });

  // The single read has no list, so the list's limit and before are unknown parameters to it.
  it.each(['limit=0', 'before=nope', 'limit=lots&before=nope'])('ignores %s', async (query) => {
    const account = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, account);
    const response = await getJson(app, depositUrl(account, deposit, `?${query}`), DEPOSIT);
    expect(response.statusCode).toBe(200);
  });

  // Slice 4 decision 64: the account in the path is a scope, decided by the one query.
  it('answers 404 deposit-not-found for another account’s deposit', async () => {
    const owner = await createTestAccount(pool);
    const other = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, owner);
    const response = await getJson(app, depositUrl(other, deposit), DEPOSIT);
    expect(response.statusCode).toBe(404);
    expect(response.json).toEqual(DEPOSIT_NOT_FOUND);
  });

  it('answers 404 deposit-not-found for a real deposit under an unknown account', async () => {
    const owner = await createTestAccount(pool);
    const deposit = await createTestDeposit(pool, owner);
    const unknown = toTypeId('acct', newUuid('acct'));
    const response = await getJson(
      app,
      `/accounts/${unknown}/deposits/${toTypeId('dep', deposit.id)}`,
      DEPOSIT,
    );
    expect(response.statusCode).toBe(404);
    expect(response.json).toEqual(DEPOSIT_NOT_FOUND);
  });

  it('answers 404 deposit-not-found for an unknown deposit', async () => {
    const account = await createTestAccount(pool);
    const response = await getJson(
      app,
      `/accounts/${account.typeId}/deposits/${toTypeId('dep', newUuid('dep'))}`,
      DEPOSIT,
    );
    expect(response.statusCode).toBe(404);
    expect(response.json).toEqual(DEPOSIT_NOT_FOUND);
  });

  // Slice 4 decision 68: both IDs and the query at once, in the contract's order.
  it('reports both invalid IDs and an invalid include at once', async () => {
    const response = await getJson(
      app,
      `/accounts/nope/deposits/${toTypeId('wd', newUuid('wd'))}?include=everything`,
      DEPOSIT,
    );
    expect(response.statusCode).toBe(400);
    expect(response.json).toEqual({
      type: 'urn:ledger-demo:problem:invalid-request',
      title: 'Invalid request',
      status: 400,
      errors: [
        { path: 'account_id', message: 'must be a acct_ ID' },
        { path: 'deposit_id', message: 'must be a dep_ ID' },
        { path: 'include', message: 'Invalid input: expected "history"' },
      ],
    });
  });
});
