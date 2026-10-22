import { spawnSync } from 'node:child_process';
import { afterAll, describe, expect, it } from 'vitest';
import { decide, startReview, type Decision } from '../../src/compliance/transitions.ts';
import { recordNotification, type Payment } from '../../src/ingestion/payments.ts';
import { book, postDecision } from '../../src/ledger/posting.ts';
import { reconcile, type ReconcileReport } from '../../src/reconciliation/reconcile.ts';
import { withTransaction, type Tx } from '../../src/shared/db.ts';
import { newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import {
  createTestAccount,
  createTestPayment,
  createTestPool,
  newNetworkTransactionId,
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
        balance: { pendingIn: 300n, available: 101n },
        fromPayments: { pendingIn: 300n, available: 100n },
        fromMovements: { pendingIn: 300n, available: 100n },
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
        fromPayments: { pendingIn: 300n, available: 100n },
        fromMovements: { pendingIn: 300n, available: 100n },
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
    expect(report.differences[0]?.fromPayments).toEqual({ pendingIn: 0n, available: 400n });
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
      fromPayments: { pendingIn: 300n },
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
    for (let i = 0; i < 3; i++) await recordNotification(pool, notification);
    expect((await check(pool, account)).deliveries.duplicateNotifications).toBe(2n);
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
