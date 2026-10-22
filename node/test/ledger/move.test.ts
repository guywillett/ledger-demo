import { afterAll, describe, expect, it } from 'vitest';
import {
  move,
  MovementConflictError,
  OverdraftError,
  type Movement,
} from '../../src/ledger/move.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid } from '../../src/shared/ids.ts';
import { balancesOf, createTestAccount, createTestPool, type TestAccount } from '../helpers.ts';

// move() against the test database. Each test uses its own account, so balances start at zero.

const pool = createTestPool();
afterAll(() => pool.end());

function booking(account: TestAccount, overrides: Partial<Movement> = {}): Movement {
  return {
    accountId: account.id,
    from: 'external',
    to: 'pending_in',
    amountMinor: 1250n,
    referenceType: 'payment',
    referenceId: newUuid('pay'),
    referenceStep: 'book',
    ...overrides,
  };
}

async function movementCount(m: Movement): Promise<number> {
  const result = await pool.query<{ n: number }>(
    `SELECT COUNT(*)::int AS n FROM balance_movements
     WHERE reference_type = $1 AND reference_id = $2 AND reference_step = $3`,
    [m.referenceType, m.referenceId, m.referenceStep],
  );
  return result.rows[0]?.n ?? 0;
}

describe('move', () => {
  it('moves money between the right buckets', async () => {
    const account = await createTestAccount(pool);
    const book = booking(account);
    expect(await withTransaction(pool, (tx) => move(tx, book))).toBe('applied');
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 1250n, available: 0n });

    const credit = {
      ...book,
      from: 'pending_in',
      to: 'available',
      referenceStep: 'credit',
    } as const;
    expect(await withTransaction(pool, (tx) => move(tx, credit))).toBe('applied');
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 0n, available: 1250n });
  });

  it('moves amounts beyond 2^53 exactly', async () => {
    const account = await createTestAccount(pool);
    // 2^53 + 1: the smallest integer a JavaScript number can't hold.
    const amountMinor = 9007199254740993n;
    await withTransaction(pool, (tx) => move(tx, booking(account, { amountMinor })));
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: amountMinor, available: 0n });
  });

  it('is a no-op when called twice with the same reference', async () => {
    const account = await createTestAccount(pool);
    const m = booking(account);
    expect(await withTransaction(pool, (tx) => move(tx, m))).toBe('applied');
    expect(await withTransaction(pool, (tx) => move(tx, m))).toBe('duplicate');
    // Also within one transaction.
    expect(await withTransaction(pool, (tx) => move(tx, m))).toBe('duplicate');

    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 1250n, available: 0n });
    expect(await movementCount(m)).toBe(1);
  });

  it('refuses a reused reference with different details, changing nothing', async () => {
    const account = await createTestAccount(pool);
    const other = await createTestAccount(pool);
    const m = booking(account);
    await withTransaction(pool, (tx) => move(tx, m));

    for (const reused of [
      { ...m, amountMinor: 999n },
      { ...m, accountId: other.id },
      { ...m, to: 'available' as const },
    ]) {
      await expect(withTransaction(pool, (tx) => move(tx, reused))).rejects.toThrow(
        MovementConflictError,
      );
    }
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 1250n, available: 0n });
    expect(await balancesOf(pool, other.id)).toEqual({ pendingIn: 0n, available: 0n });
  });

  it('applies exactly once when called concurrently with the same reference', async () => {
    const account = await createTestAccount(pool);
    const m = booking(account);
    const outcomes = await Promise.all(
      Array.from({ length: 10 }, () => withTransaction(pool, (tx) => move(tx, m))),
    );

    expect(outcomes.filter((o) => o === 'applied')).toHaveLength(1);
    expect(outcomes.filter((o) => o === 'duplicate')).toHaveLength(9);
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 1250n, available: 0n });
    expect(await movementCount(m)).toBe(1);
  });

  it('cannot overdraw a bucket: the CHECK rejects it, reported as an OverdraftError, and the movement is rolled back', async () => {
    const account = await createTestAccount(pool);
    const m = booking(account, { from: 'pending_in', to: 'available', referenceStep: 'credit' });
    const attempt = withTransaction(pool, (tx) => move(tx, m));
    await expect(attempt).rejects.toThrow(OverdraftError);
    await expect(attempt).rejects.toMatchObject({
      bucket: 'pending_in',
      accountId: account.id,
      amountMinor: 1250n,
      cause: { code: '23514' }, // check_violation, kept as the cause
    });
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 0n, available: 0n });
    expect(await movementCount(m)).toBe(0);
  });

  it('throws if the account has no balance row, recording nothing', async () => {
    // The trigger creates the row with the account, so delete it to get here (reconciliation
    // reports such accounts too).
    const account = await createTestAccount(pool);
    await pool.query('DELETE FROM account_balances WHERE account_id = $1', [account.id]);
    const m: Movement = {
      accountId: account.id,
      from: 'external',
      to: 'pending_in',
      amountMinor: 1250n,
      referenceType: 'payment',
      referenceId: newUuid('pay'),
      referenceStep: 'book',
    };
    await expect(withTransaction(pool, (tx) => move(tx, m))).rejects.toThrow(/no balance row/);
    expect(await movementCount(m)).toBe(0);
  });

  it('rejects movements to the same bucket or with a non-positive amount', async () => {
    const account = await createTestAccount(pool);
    await expect(
      withTransaction(pool, (tx) => move(tx, booking(account, { to: 'external' }))),
    ).rejects.toThrow(/to itself/);
    await expect(
      withTransaction(pool, (tx) => move(tx, booking(account, { amountMinor: 0n }))),
    ).rejects.toThrow(/positive/);
  });

  it('keeps balance_movements append-only', async () => {
    const account = await createTestAccount(pool);
    const m = booking(account);
    await withTransaction(pool, (tx) => move(tx, m));
    await expect(
      pool.query('UPDATE balance_movements SET amount_minor = 1 WHERE reference_id = $1', [
        m.referenceId,
      ]),
    ).rejects.toThrow(/append-only: UPDATE/);
    await expect(
      pool.query('DELETE FROM balance_movements WHERE reference_id = $1', [m.referenceId]),
    ).rejects.toThrow(/append-only: DELETE/);
  });
});
