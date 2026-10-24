import { randomUUID } from 'node:crypto';
import { afterAll, describe, expect, it } from 'vitest';
import { move } from '../../src/ledger/move.ts';
import { markSubmitted } from '../../src/ledger/withdrawals.ts';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { getWithdrawal, type Withdrawal } from '../../src/withdrawals/withdrawals.ts';
import { expectMatchesContract } from '../contract.ts';
import {
  balancesOf,
  capturingLogger,
  createTestAccount,
  createTestApp,
  createTestWithdrawal,
  fundTestAccount,
} from '../helpers.ts';

// POST /provider/transfer-updates, against withdrawals set up through the ledger functions.
// Every answer to a parseable update is 204 with no body, whatever it did, so each test asserts
// the recorded effects: the transfer_updates row, balances, movements, the log line.

const { logger, lines } = capturingLogger();
const { app, pool, close } = await createTestApp({ logger });
afterAll(close);

type Update = Record<string, unknown>;

async function postUpdate(payload: Update): Promise<number> {
  const response = await app.inject({
    method: 'POST',
    url: '/provider/transfer-updates',
    headers: { 'content-type': 'application/json' },
    payload: JSON.stringify(payload),
  });
  expectMatchesContract('POST', '/provider/transfer-updates', response);
  return response.statusCode;
}

function update(w: Withdrawal, overrides: Update = {}): Update {
  return {
    provider_event_id: `pev_${randomUUID()}`,
    transfer_id: 'tr_1',
    reference: toTypeId('wd', w.id),
    status: 'executed',
    occurred_at: '2026-10-24T09:00:00.000Z',
    ...overrides,
  };
}

// A withdrawal of 400 from an account funded with 1000: held, or submitted as tr_1.
async function withdrawal(status: 'held' | 'submitted' = 'submitted'): Promise<Withdrawal> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, 1000n);
  const w = await createTestWithdrawal(pool, account, 400n);
  if (status === 'submitted') {
    await withTransaction(pool, (tx) => markSubmitted(tx, w.id, 'tr_1'));
  }
  return w;
}

type UpdateRow = { outcome: string; notification_count: number; withdrawal_id: string | null };

async function rowOf(providerEventId: unknown): Promise<UpdateRow | undefined> {
  const result = await pool.query<UpdateRow>(
    `SELECT outcome, notification_count, withdrawal_id FROM transfer_updates
     WHERE provider_event_id = $1`,
    [providerEventId],
  );
  return result.rows[0];
}

async function stepsOf(id: Uuid<'wd'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    `SELECT reference_step FROM balance_movements
     WHERE reference_type = 'withdrawal' AND reference_id = $1 ORDER BY created_at`,
    [id],
  );
  return result.rows.map((r) => r.reference_step);
}

function logOf(providerEventId: unknown): Record<string, unknown> | undefined {
  return lines().find(
    (l) => l.provider_event_id === providerEventId && l.msg !== 'request completed',
  );
}

// Accounts whose updates all went well, for the final reconciliation.
const settledAccounts: Uuid<'acct'>[] = [];

describe('POST /provider/transfer-updates', () => {
  it('executes a submitted withdrawal: 204, applied, held funds gone, transfer ID kept', async () => {
    const w = await withdrawal();
    settledAccounts.push(w.accountId);
    const u = update(w);
    expect(await postUpdate(u)).toBe(204);

    expect(await rowOf(u.provider_event_id)).toEqual({
      outcome: 'applied',
      notification_count: 1,
      withdrawal_id: w.id,
    });
    expect(await getWithdrawal(pool, w.id)).toMatchObject({
      status: 'executed',
      providerTransferId: 'tr_1',
    });
    expect(await balancesOf(pool, w.accountId)).toEqual({
      pendingIn: 0n,
      available: 600n,
      held: 0n,
    });
    expect(await stepsOf(w.id)).toEqual(['hold', 'execute']);
    expect(logOf(u.provider_event_id)).toMatchObject({
      level: 'info',
      msg: 'transfer update applied',
      withdrawal_id: toTypeId('wd', w.id),
      transfer_id: 'tr_1',
      status: 'executed',
    });
  });

  it('releases a failed one: funds back in available, the reason stored', async () => {
    const w = await withdrawal();
    settledAccounts.push(w.accountId);
    const u = update(w, { status: 'failed', reason: 'beneficiary account closed' });
    expect(await postUpdate(u)).toBe(204);

    expect((await rowOf(u.provider_event_id))?.outcome).toBe('applied');
    expect(await getWithdrawal(pool, w.id)).toMatchObject({
      status: 'failed',
      failureReason: 'beneficiary account closed',
    });
    expect(await balancesOf(pool, w.accountId)).toEqual({
      pendingIn: 0n,
      available: 1000n,
      held: 0n,
    });
    expect(await stepsOf(w.id)).toEqual(['hold', 'release']);
  });

  it('applies an update that overtakes the submitter (still held); the late markSubmitted skips', async () => {
    const w = await withdrawal('held');
    settledAccounts.push(w.accountId);
    const u = update(w);
    expect(await postUpdate(u)).toBe(204);
    expect((await rowOf(u.provider_event_id))?.outcome).toBe('applied');
    expect(await withTransaction(pool, (tx) => markSubmitted(tx, w.id, 'tr_1'))).toBe('skipped');
    expect(await getWithdrawal(pool, w.id)).toMatchObject({
      status: 'executed',
      providerTransferId: 'tr_1',
    });
  });

  it('counts the same event resent, settling nothing twice', async () => {
    const w = await withdrawal();
    settledAccounts.push(w.accountId);
    const u = update(w);
    expect(await postUpdate(u)).toBe(204);
    expect(await postUpdate(u)).toBe(204);

    expect(await rowOf(u.provider_event_id)).toMatchObject({
      outcome: 'applied',
      notification_count: 2,
    });
    expect(await stepsOf(w.id)).toEqual(['hold', 'execute']);
    expect(
      lines()
        .filter((l) => l.provider_event_id === u.provider_event_id)
        .map((l) => l.msg),
    ).toContain('duplicate transfer update');
  });

  it('records a second event with the same result as a duplicate', async () => {
    const w = await withdrawal();
    settledAccounts.push(w.accountId);
    await postUpdate(update(w));
    const second = update(w);
    expect(await postUpdate(second)).toBe(204);
    expect(await rowOf(second.provider_event_id)).toMatchObject({
      outcome: 'duplicate',
      notification_count: 1,
    });
    expect(await stepsOf(w.id)).toEqual(['hold', 'execute']);
  });

  it('changes nothing when the provider reuses an event ID with other details, and alerts', async () => {
    const w = await withdrawal();
    settledAccounts.push(w.accountId);
    const u = update(w);
    await postUpdate(u);
    expect(await postUpdate({ ...u, transfer_id: 'tr_other' })).toBe(204);

    expect(await rowOf(u.provider_event_id)).toMatchObject({
      outcome: 'applied',
      notification_count: 1,
    });
    expect(
      lines().find(
        (l) =>
          l.provider_event_id === u.provider_event_id &&
          l.transfer_id === 'tr_other' &&
          l.msg !== 'request completed',
      ),
    ).toMatchObject({ level: 'error', alert: true });
  });

  it('records failed after executed as a conflict, moving nothing, and alerts', async () => {
    const w = await withdrawal();
    await postUpdate(update(w));
    const contradiction = update(w, { status: 'failed', reason: 'returned' });
    expect(await postUpdate(contradiction)).toBe(204);

    expect((await rowOf(contradiction.provider_event_id))?.outcome).toBe('conflict');
    expect((await getWithdrawal(pool, w.id))?.status).toBe('executed');
    expect(await stepsOf(w.id)).toEqual(['hold', 'execute']);
    expect(logOf(contradiction.provider_event_id)).toMatchObject({
      level: 'error',
      alert: true,
      stored_status: 'executed',
    });
    // Reconciliation lists it for a person; the books still agree.
    const report = await reconcile(pool, { accountIds: [w.accountId], stuckAfterMs: 0 });
    expect(report.transferUpdateProblems).toEqual([
      expect.objectContaining({
        providerEventId: contradiction.provider_event_id,
        outcome: 'conflict',
        withdrawalId: w.id,
      }),
    ]);
    expect(report.consistent).toBe(true);
  });

  it('records a transfer-ID mismatch as a conflict, moving nothing', async () => {
    const w = await withdrawal();
    const u = update(w, { transfer_id: 'tr_other' });
    expect(await postUpdate(u)).toBe(204);
    expect((await rowOf(u.provider_event_id))?.outcome).toBe('conflict');
    expect(await getWithdrawal(pool, w.id)).toMatchObject({
      status: 'submitted',
      providerTransferId: 'tr_1',
    });
    expect(await stepsOf(w.id)).toEqual(['hold']);
    expect(logOf(u.provider_event_id)).toMatchObject({
      level: 'error',
      alert: true,
      stored_transfer_id: 'tr_1',
    });
  });

  it.each([
    ['an unknown withdrawal', () => toTypeId('wd', newUuid('wd'))],
    ['a reference that is no wd_ TypeID', () => `foreign-${randomUUID()}`],
  ])('records an update for %s as unmatched, and alerts', async (_case, reference) => {
    const u = {
      provider_event_id: `pev_${randomUUID()}`,
      transfer_id: 'tr_9',
      reference: reference(),
      status: 'executed',
      occurred_at: '2026-10-24T09:00:00.000Z',
    };
    expect(await postUpdate(u)).toBe(204);
    expect(await rowOf(u.provider_event_id)).toEqual({
      outcome: 'unmatched',
      notification_count: 1,
      withdrawal_id: null,
    });
    expect(logOf(u.provider_event_id)).toMatchObject({ level: 'error', alert: true });
    // In a whole-database reconciliation (an unmatched update belongs to no account).
    const report = await reconcile(pool, { stuckAfterMs: 24 * 3600 * 1000 });
    expect(report.transferUpdateProblems.map((p) => p.providerEventId)).toContain(
      u.provider_event_id,
    );
  });

  it.each([
    ['failed without a reason', { status: 'failed' }],
    ['executed with a reason', { reason: 'why' }],
    ['an unknown status', { status: 'returned' }],
    ['no transfer_id', { transfer_id: undefined }],
    ['an invalid occurred_at', { occurred_at: 'yesterday' }],
  ])('answers 400 for %s, recording nothing', async (_case, overrides) => {
    const w = await withdrawal();
    const u = update(w, overrides);
    expect(await postUpdate(JSON.parse(JSON.stringify(u)) as Update)).toBe(400);
    expect(await rowOf(u.provider_event_id)).toBeUndefined();
    expect((await getWithdrawal(pool, w.id))?.status).toBe('submitted');
  });

  it('answers 400 (not 415) for a non-JSON content type: a webhook has one 4xx', async () => {
    const response = await app.inject({
      method: 'POST',
      url: '/provider/transfer-updates',
      headers: { 'content-type': 'text/plain' },
      payload: 'executed',
    });
    expectMatchesContract('POST', '/provider/transfer-updates', response);
    expect(response.statusCode).toBe(400);
  });

  it('answers 500 for an invariant violation, rolling everything back, and alerts', async () => {
    // An execute movement already exists for a withdrawal still submitted: the guards disagree.
    const w = await withdrawal();
    await withTransaction(pool, (tx) =>
      move(tx, {
        accountId: w.accountId,
        from: 'held',
        to: 'external',
        amountMinor: w.amountMinor,
        referenceType: 'withdrawal',
        referenceId: w.id,
        referenceStep: 'execute',
      }),
    );
    const before = await balancesOf(pool, w.accountId);
    const u = update(w);
    expect(await postUpdate(u)).toBe(500);

    expect(await rowOf(u.provider_event_id)).toBeUndefined();
    expect((await getWithdrawal(pool, w.id))?.status).toBe('submitted');
    expect(await balancesOf(pool, w.accountId)).toEqual(before);
    expect(
      lines().find((l) => l.provider_event_id === u.provider_event_id && l.level === 'fatal'),
    ).toMatchObject({ alert: true });
  });

  it('leaves the books consistent for every account whose updates applied', async () => {
    const report = await reconcile(pool, { accountIds: settledAccounts, stuckAfterMs: 0 });
    expect(report.differences).toEqual([]);
    expect(report.withdrawalMismatches).toEqual([]);
    expect(report.stuckWithdrawals).toEqual([]);
    expect(report.transferUpdateProblems).toEqual([]);
    expect(report.consistent).toBe(true);
  });
});
