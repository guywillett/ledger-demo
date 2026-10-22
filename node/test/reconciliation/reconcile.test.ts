import { spawnSync } from 'node:child_process';
import { afterAll, describe, expect, it } from 'vitest';
import { decide, startReview } from '../../src/compliance/transitions.ts';
import type { Decision } from '../../src/compliance/decision.ts';
import { recordNotification } from '../../src/ingestion/notifications.ts';
import { type Payment } from '../../src/payments/payments.ts';
import { move } from '../../src/ledger/move.ts';
import { book, postDecision } from '../../src/ledger/posting.ts';
import { execute, markSubmitted, release } from '../../src/ledger/withdrawals.ts';
import { markPublished } from '../../src/outbox/outbox.ts';
import { reconcile, type ReconcileReport } from '../../src/reconciliation/reconcile.ts';
import { withTransaction, type Tx } from '../../src/shared/db.ts';
import { newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import {
  createTestAccount,
  createTestPayment,
  createTestPool,
  createTestWithdrawal,
  fundTestAccount,
  newNetworkTransactionId,
  outboxRowsFor,
  type TestAccount,
} from '../helpers.ts';

// reconcile() against the test database, limited to each test's own accounts (the test
// database as a whole holds deliberate inconsistencies left by other tests). Corruption happens
// inside a transaction that is always rolled back, and reconcile() runs in that transaction, so
// it sees the corruption and nothing persists.

const pool = createTestPool();
afterAll(() => pool.end());

const NEVER_STUCK = { stuckAfterMs: 24 * 3600 * 1000 };

class RolledBack<T> extends Error {
  readonly value: T;

  constructor(value: T) {
    super('rolled back on purpose');
    this.value = value;
  }
}

async function inRolledBackTransaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T> {
  try {
    await withTransaction(pool, async (tx) => {
      throw new RolledBack(await fn(tx));
    });
  } catch (err) {
    if (err instanceof RolledBack) return err.value as T;
    throw err;
  }
  throw new Error('unreachable');
}

async function processPayment(payment: Payment, decision: Decision): Promise<void> {
  await withTransaction(pool, (tx) => book(tx, payment.id));
  await withTransaction(pool, (tx) => startReview(tx, payment.id));
  await withTransaction(pool, (tx) => decide(tx, payment.id, decision));
  await withTransaction(pool, (tx) => postDecision(tx, payment.id));
}

// An account with one payment in each kind of final and non-final state.
async function busyAccount(): Promise<{ account: TestAccount; booked: Payment }> {
  const account = await createTestAccount(pool);
  await processPayment(await createTestPayment(pool, account, 100n), { outcome: 'approved' });
  await processPayment(await createTestPayment(pool, account, 200n), {
    outcome: 'rejected',
    reason: 'test',
  });
  const booked = await createTestPayment(pool, account, 300n);
  await withTransaction(pool, (tx) => book(tx, booked.id));
  await createTestPayment(pool, account, 400n); // still created
  return { account, booked };
}

function check(db: Tx | typeof pool, account: TestAccount): Promise<ReconcileReport> {
  return reconcile(db, { accountIds: [account.id], ...NEVER_STUCK });
}

describe('reconcile', () => {
  it('finds the books consistent for a normal account', async () => {
    const { account } = await busyAccount();
    const report = await check(pool, account);
    expect(report).toMatchObject({
      accountsChecked: 1,
      paymentsChecked: 4,
      differences: [],
      mismatches: [],
      consistent: true,
    });
  });

  it('detects a balance changed outside move()', async () => {
    const { account } = await busyAccount();
    const report = await inRolledBackTransaction(async (tx) => {
      await tx.query(
        'UPDATE account_balances SET available = available + 1 WHERE account_id = $1',
        [account.id],
      );
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    expect(report.differences).toEqual([
      {
        accountId: account.id,
        balance: { pendingIn: 300n, available: 101n, held: 0n },
        fromStatuses: { pendingIn: 300n, available: 100n, held: 0n },
        fromMovements: { pendingIn: 300n, available: 100n, held: 0n },
      },
    ]);
    expect(report.mismatches).toEqual([]);
  });

  it('detects an account without a balance row', async () => {
    const { account } = await busyAccount();
    const report = await inRolledBackTransaction(async (tx) => {
      await tx.query('DELETE FROM account_balances WHERE account_id = $1', [account.id]);
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    expect(report.accountsChecked).toBe(1);
    expect(report.differences).toEqual([
      {
        accountId: account.id,
        balance: null,
        fromStatuses: { pendingIn: 300n, available: 100n, held: 0n },
        fromMovements: { pendingIn: 300n, available: 100n, held: 0n },
      },
    ]);
  });

  it('detects a status changed without its movement', async () => {
    const { account, booked } = await busyAccount();
    const report = await inRolledBackTransaction(async (tx) => {
      await tx.query("UPDATE incoming_payments SET status = 'credited' WHERE id = $1", [booked.id]);
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    // Payments now say 300 is available; balance and movements still have it pending.
    expect(report.differences[0]?.fromStatuses).toEqual({
      pendingIn: 0n,
      available: 400n,
      held: 0n,
    });
    expect(report.mismatches).toEqual([
      expect.objectContaining({
        paymentId: booked.id,
        status: 'credited',
        steps: ['book'],
        expectedSteps: ['book', 'credit'],
      }),
    ]);
  });

  it('detects a movement for a payment that does not exist', async () => {
    const { account } = await busyAccount();
    const ghost = newUuid('pay');
    const report = await inRolledBackTransaction(async (tx) => {
      // As a buggy writer would: a movement and its balance change, for no payment.
      await tx.query(
        `INSERT INTO balance_movements (id, account_id, from_bucket, to_bucket, amount_minor,
                                        reference_type, reference_id, reference_step)
         VALUES ($1, $2, 'external', 'pending_in', 50, 'payment', $3, 'book')`,
        [newUuid('mov'), account.id, ghost],
      );
      await tx.query(
        'UPDATE account_balances SET pending_in = pending_in + 50 WHERE account_id = $1',
        [account.id],
      );
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    // Balance and movements agree; only the payments disagree.
    expect(report.differences[0]).toMatchObject({
      balance: { pendingIn: 350n },
      fromMovements: { pendingIn: 350n },
      fromStatuses: { pendingIn: 300n },
    });
    expect(report.mismatches).toEqual([
      expect.objectContaining({ paymentId: ghost, status: null, steps: ['book'] }),
    ]);
  });

  it('detects a movement posted to another account', async () => {
    const { account, booked } = await busyAccount();
    const other = await createTestAccount(pool);
    const report = await inRolledBackTransaction(async (tx) => {
      await tx.query(
        `INSERT INTO balance_movements (id, account_id, from_bucket, to_bucket, amount_minor,
                                        reference_type, reference_id, reference_step)
         VALUES ($1, $2, 'pending_in', 'available', 300, 'payment', $3, 'credit')`,
        [newUuid('mov'), other.id, booked.id],
      );
      return check(tx, account);
    });
    expect(report.mismatches).toEqual([
      expect.objectContaining({ paymentId: booked.id, otherAccount: true }),
    ]);
  });

  it('detects movements for the wrong amount, even when the account sums agree', async () => {
    const account = await createTestAccount(pool);
    const first = await createTestPayment(pool, account, 100n);
    const second = await createTestPayment(pool, account, 200n);
    const report = await inRolledBackTransaction(async (tx) => {
      // As a buggy writer would: each booked 10 off, in opposite directions, so the account's
      // pending_in (300) agrees with its payments and its movements.
      for (const [payment, amount] of [
        [first, 110],
        [second, 190],
      ] as const) {
        await tx.query(
          `INSERT INTO balance_movements (id, account_id, from_bucket, to_bucket, amount_minor,
                                          reference_type, reference_id, reference_step)
           VALUES ($1, $2, 'external', 'pending_in', $3, 'payment', $4, 'book')`,
          [newUuid('mov'), account.id, amount, payment.id],
        );
        await tx.query("UPDATE incoming_payments SET status = 'received' WHERE id = $1", [
          payment.id,
        ]);
      }
      await tx.query('UPDATE account_balances SET pending_in = 300 WHERE account_id = $1', [
        account.id,
      ]);
      return check(tx, account);
    });
    expect(report.differences).toEqual([]);
    expect(report.consistent).toBe(false);
    // In any order: the query doesn't sort.
    expect(report.mismatches).toHaveLength(2);
    expect(report.mismatches).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ paymentId: first.id, steps: ['book'], wrongAmount: true }),
        expect.objectContaining({ paymentId: second.id, steps: ['book'], wrongAmount: true }),
      ]),
    );
  });

  // balance_movements can't be corrupted after the fact: its triggers reject UPDATE and DELETE
  // (test/ledger/move.test.ts), so the journal check needs no "edited movement" test.

  it('reports payments stuck in a non-final status, and never final ones', async () => {
    const { account, booked } = await busyAccount();
    const report = await reconcile(pool, { accountIds: [account.id], stuckAfterMs: 0 });
    const stuck = new Map(report.stuck.map((s) => [s.paymentId, s.status]));
    expect(stuck.get(booked.id)).toBe('received');
    expect([...stuck.values()].sort()).toEqual(['created', 'received']);
    // Stuck payments don't make the books inconsistent.
    expect(report.consistent).toBe(true);

    expect((await check(pool, account)).stuck).toEqual([]);
  });

  // Nothing relays in these tests, so every payment's event is still unpublished unless a test
  // changes that.
  it('says which side stalled for a payment stuck in created', async () => {
    const account = await createTestAccount(pool);
    const unpublished = await createTestPayment(pool, account, 100n);
    const published = await createTestPayment(pool, account, 200n);
    const missing = await createTestPayment(pool, account, 300n);
    const booked = await createTestPayment(pool, account, 400n);
    await withTransaction(pool, (tx) => book(tx, booked.id));

    const report = await inRolledBackTransaction(async (tx) => {
      const [event] = await outboxRowsFor(pool, published.id);
      if (event === undefined) throw new Error('no outbox row');
      expect(await markPublished(tx, event.id)).toBe('applied');
      await tx.query('DELETE FROM outbox WHERE entity_id = $1', [missing.id]);
      return reconcile(tx, { accountIds: [account.id], stuckAfterMs: 0 });
    });

    const events = new Map(report.stuck.map((s) => [s.paymentId, s.event]));
    expect(events).toEqual(
      new Map([
        [unpublished.id, 'unpublished'],
        [published.id, 'published'],
        [missing.id, 'missing'],
        // Only for created: a booked payment's event has evidently been delivered.
        [booked.id, null],
      ]),
    );
    // A missing event is a bug to report, but the books still agree.
    expect(report.consistent).toBe(true);
  });

  it('reports unpublished events, counting only those older than the threshold as stuck', async () => {
    const account = await createTestAccount(pool);
    const first = await createTestPayment(pool, account);
    const second = await createTestPayment(pool, account);

    const stuck = await reconcile(pool, { accountIds: [account.id], stuckAfterMs: 0 });
    expect(stuck.outbox).toMatchObject({ stuckUnpublished: 2, maxUnpublishedAttempts: 0 });
    expect(stuck.outbox.oldestUnpublishedAgeSeconds).toBeGreaterThanOrEqual(0);

    // Unpublished, but not for long: reported, not stuck.
    const recent = await check(pool, account);
    expect(recent.outbox.stuckUnpublished).toBe(0);
    expect(recent.outbox.oldestUnpublishedAgeSeconds).not.toBeNull();

    const afterAttempts = await inRolledBackTransaction(async (tx) => {
      await tx.query('UPDATE outbox SET attempts = 7 WHERE entity_id = $1', [second.id]);
      return reconcile(tx, { accountIds: [account.id], stuckAfterMs: 0 });
    });
    expect(afterAttempts.outbox.maxUnpublishedAttempts).toBe(7);

    const allPublished = await inRolledBackTransaction(async (tx) => {
      await tx.query('UPDATE outbox SET published_at = now() WHERE entity_id = ANY($1)', [
        [first.id, second.id],
      ]);
      return reconcile(tx, { accountIds: [account.id], stuckAfterMs: 0 });
    });
    expect(allPublished.outbox).toEqual({
      stuckUnpublished: 0,
      oldestUnpublishedAgeSeconds: null,
      maxUnpublishedAttempts: 0,
    });
    // Outbox problems don't make the books inconsistent either.
    expect(stuck.consistent).toBe(true);
  });

  it('counts duplicate notifications', async () => {
    const account = await createTestAccount(pool);
    const notification = {
      creditorIban: account.iban,
      providerAccountId: null,
      amountMinor: 700n,
      currency: 'EUR' as const,
      network: 'SEPA' as const,
      networkTransactionId: newNetworkTransactionId(),
    };
    for (let i = 0; i < 3; i++) {
      await withTransaction(pool, (tx) => recordNotification(tx, notification));
    }
    expect((await check(pool, account)).deliveries.duplicateNotifications).toBe(2n);
  });
});

// An account funded with 1000, with one withdrawal in each status: held 100, submitted 200,
// executed 300, failed 50 (released). Available 1000 - 100 - 200 - 300 = 400; held 300.
async function withdrawingAccount(): Promise<{
  account: TestAccount;
  ids: { held: Uuid<'wd'>; submitted: Uuid<'wd'>; executed: Uuid<'wd'>; failed: Uuid<'wd'> };
}> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, 1000n);
  const held = await createTestWithdrawal(pool, account, 100n);
  const submitted = await createTestWithdrawal(pool, account, 200n);
  const executed = await createTestWithdrawal(pool, account, 300n);
  const failed = await createTestWithdrawal(pool, account, 50n);
  await withTransaction(pool, (tx) => markSubmitted(tx, submitted.id, 'tr_1'));
  await withTransaction(pool, (tx) => markSubmitted(tx, executed.id, 'tr_2'));
  await withTransaction(pool, (tx) => execute(tx, executed.id));
  await withTransaction(pool, (tx) => release(tx, failed.id, 'refused'));
  return {
    account,
    ids: { held: held.id, submitted: submitted.id, executed: executed.id, failed: failed.id },
  };
}

describe('reconcile: withdrawals', () => {
  it('finds the books consistent with a withdrawal in every status', async () => {
    const { account } = await withdrawingAccount();
    const report = await check(pool, account);
    expect(report).toMatchObject({
      accountsChecked: 1,
      paymentsChecked: 1,
      withdrawalsChecked: 4,
      differences: [],
      mismatches: [],
      withdrawalMismatches: [],
      consistent: true,
    });
    const balance = await pool.query<{ available: bigint; held: bigint }>(
      'SELECT available, held FROM account_balances WHERE account_id = $1',
      [account.id],
    );
    expect(balance.rows).toEqual([{ available: 400n, held: 300n }]);
  });

  it('detects a withdrawal whose status changed without its movement', async () => {
    const { account, ids } = await withdrawingAccount();
    const report = await inRolledBackTransaction(async (tx) => {
      await tx.query(`UPDATE withdrawals SET status = 'executed' WHERE id = $1`, [ids.held]);
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    expect(report.withdrawalMismatches).toEqual([
      {
        withdrawalId: ids.held,
        accountId: account.id,
        status: 'executed',
        steps: ['hold'],
        expectedSteps: ['execute', 'hold'],
        otherAccount: false,
        wrongAmount: false,
      },
    ]);
    // The statuses now say 100 less is held (and spent), but the balance and movements still
    // hold it.
    expect(report.differences).toEqual([
      expect.objectContaining({
        balance: { pendingIn: 0n, available: 400n, held: 300n },
        fromStatuses: { pendingIn: 0n, available: 400n, held: 200n },
        fromMovements: { pendingIn: 0n, available: 400n, held: 300n },
      }),
    ]);
  });

  it('detects a held balance changed outside move()', async () => {
    const { account } = await withdrawingAccount();
    const report = await inRolledBackTransaction(async (tx) => {
      await tx.query('UPDATE account_balances SET held = held + 1 WHERE account_id = $1', [
        account.id,
      ]);
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    expect(report.withdrawalMismatches).toEqual([]);
    expect(report.differences).toEqual([
      {
        accountId: account.id,
        balance: { pendingIn: 0n, available: 400n, held: 301n },
        fromStatuses: { pendingIn: 0n, available: 400n, held: 300n },
        fromMovements: { pendingIn: 0n, available: 400n, held: 300n },
      },
    ]);
  });

  it('detects movements for a withdrawal that does not exist', async () => {
    const { account } = await withdrawingAccount();
    const ghost = newUuid('wd');
    const report = await inRolledBackTransaction(async (tx) => {
      await move(tx, {
        accountId: account.id,
        from: 'available',
        to: 'held',
        amountMinor: 10n,
        referenceType: 'withdrawal',
        referenceId: ghost,
        referenceStep: 'hold',
      });
      return check(tx, account);
    });
    expect(report.consistent).toBe(false);
    expect(report.withdrawalMismatches).toEqual([
      expect.objectContaining({ withdrawalId: ghost, status: null, steps: ['hold'] }),
    ]);
  });

  it('reports withdrawals stuck in held or submitted, and never settled ones', async () => {
    const { account, ids } = await withdrawingAccount();
    const report = await reconcile(pool, { accountIds: [account.id], stuckAfterMs: 0 });
    const stuck = new Map(report.stuckWithdrawals.map((s) => [s.withdrawalId, s.status]));
    expect(stuck).toEqual(
      new Map([
        [ids.held, 'held'],
        [ids.submitted, 'submitted'],
      ]),
    );
    // Stuck withdrawals don't make the books inconsistent.
    expect(report.consistent).toBe(true);
    expect((await check(pool, account)).stuckWithdrawals).toEqual([]);
  });
});

describe('scripts/reconcile.ts', () => {
  function run(args: string[]): { status: number | null; stdout: string; stderr: string } {
    const result = spawnSync(process.execPath, ['scripts/reconcile.ts', ...args], {
      env: process.env, // the test DATABASE_URL (vitest config)
      encoding: 'utf8',
    });
    return { status: result.status, stdout: result.stdout, stderr: result.stderr };
  }

  // The child's stderr as the assertion message, so a crash shows why.
  function exitCode(result: { status: number | null; stderr: string }) {
    return expect(result.status, result.stderr);
  }

  it('exits 0 and prints JSON for a consistent account', async () => {
    const { account } = await busyAccount();
    const result = run(['--json', '--account', account.typeId]);
    exitCode(result).toBe(0);
    const report = JSON.parse(result.stdout) as Record<string, unknown>;
    expect(report).toMatchObject({ consistent: true, accountsChecked: 1, paymentsChecked: 4 });
  });

  it('exits 1 when the books disagree, naming the account', async () => {
    // A persistent corruption, in an account of its own (the test database is reset as a whole).
    const { account } = await busyAccount();
    await pool.query('UPDATE account_balances SET pending_in = 0 WHERE account_id = $1', [
      account.id,
    ]);
    const result = run(['--account', account.typeId]);
    exitCode(result).toBe(1);
    expect(result.stdout).toContain('THE BOOKS DISAGREE');
    expect(result.stdout).toContain(account.typeId);
  });

  it('exits 2 for stuck payments only with --fail-on-stuck', async () => {
    const { account } = await busyAccount();
    exitCode(run(['--account', account.typeId, '--stuck-after', '0'])).toBe(0);
    exitCode(run(['--account', account.typeId, '--stuck-after', '0', '--fail-on-stuck'])).toBe(2);
  });

  it('exits 2 with --fail-on-stuck for a stuck withdrawal, and lists it', async () => {
    const account = await createTestAccount(pool);
    await fundTestAccount(pool, account, 1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 100n);
    const result = run(['--account', account.typeId, '--stuck-after', '0', '--fail-on-stuck']);
    exitCode(result).toBe(2);
    expect(result.stdout).toContain('1 payments, 1 withdrawals: the books agree.');
    expect(result.stdout).toContain('Stuck withdrawals (held or submitted for more than 0 min): 1');
    expect(result.stdout).toContain(toTypeId('wd', withdrawal.id));
  });

  it('exits 2 with --fail-on-stuck for an unpublished event alone, saying which side stalled', async () => {
    // Every payment final, but their events never published (nothing relays in these tests).
    const account = await createTestAccount(pool);
    await processPayment(await createTestPayment(pool, account), { outcome: 'approved' });
    const done = run(['--account', account.typeId, '--stuck-after', '0', '--fail-on-stuck']);
    exitCode(done).toBe(2);
    expect(done.stdout).toContain('Stuck (non-final for more than 0 min): 0');
    expect(done.stdout).toContain('Outbox: unpublished for more than 0 min: 1');

    const waiting = await createTestPayment(pool, account);
    const result = run(['--account', account.typeId, '--stuck-after', '0']);
    exitCode(result).toBe(0);
    expect(result.stdout).toMatch(
      new RegExp(
        `${toTypeId('pay', waiting.id)}\\s+created\\s+\\d+ min\\s+unpublished \\(relay\\)`,
      ),
    );
  });

  it.each([[['--account', 'pay_bad']], [['--stuck-after', 'soon']], [['--unknown']]])(
    'exits 3 for bad arguments: %j',
    (args) => {
      const result = run(args);
      exitCode(result).toBe(3);
      expect(result.stdout).toBe('');
    },
  );

  it('accepts the account as a TypeID only', () => {
    const id: Uuid<'acct'> = newUuid('acct');
    exitCode(run(['--account', id])).toBe(3);
    exitCode(run(['--account', toTypeId('acct', id)])).toBe(0);
  });
});
