import { afterAll, describe, expect, it } from 'vitest';
import { decide, startReview } from '../../src/compliance/transitions.ts';
import { move } from '../../src/ledger/move.ts';
import {
  book,
  credit,
  LedgerInconsistencyError,
  postDecision,
  returnToSender,
} from '../../src/ledger/posting.ts';
import { withTransaction, type Tx } from '../../src/shared/db.ts';
import { newUuid, type Uuid } from '../../src/shared/ids.ts';
import { transitionStatus } from '../../src/shared/payment-status.ts';
import {
  balancesOf,
  createTestAccount,
  createTestPayment,
  createTestPool,
  statusOf,
} from '../helpers.ts';

// The payment lifecycle through the posting (ledger/) and compliance transitions, against the
// test database. Each test has its own account and payment.

const pool = createTestPool();
afterAll(() => pool.end());

async function history(
  paymentId: Uuid<'pay'>,
): Promise<{ from_status: string | null; to_status: string; reason: string | null }[]> {
  const result = await pool.query<{
    from_status: string | null;
    to_status: string;
    reason: string | null;
  }>(
    'SELECT from_status, to_status, reason FROM payment_status_history WHERE payment_id = $1 ORDER BY id',
    [paymentId],
  );
  return result.rows;
}

async function newPayment(): Promise<{ accountId: Uuid<'acct'>; paymentId: Uuid<'pay'> }> {
  const account = await createTestAccount(pool);
  const payment = await createTestPayment(pool, account);
  return { accountId: account.id, paymentId: payment.id };
}

describe('payment lifecycle', () => {
  it('approved: booked into pending_in, then credited to available', async () => {
    const { accountId, paymentId } = await newPayment();

    expect(await withTransaction(pool, (tx) => book(tx, paymentId))).toBe('applied');
    expect(await statusOf(pool, paymentId)).toBe('received');
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 1250n, available: 0n });

    expect(await withTransaction(pool, (tx) => startReview(tx, paymentId))).toBe('applied');
    const approved = { outcome: 'approved' } as const;
    expect(await withTransaction(pool, (tx) => decide(tx, paymentId, approved))).toBe('applied');
    // Decisions move no money.
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 1250n, available: 0n });

    expect(await withTransaction(pool, (tx) => postDecision(tx, paymentId))).toBe('credited');
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 0n, available: 1250n });

    expect(await history(paymentId)).toEqual([
      { from_status: null, to_status: 'created', reason: null },
      { from_status: 'created', to_status: 'received', reason: null },
      { from_status: 'received', to_status: 'under_review', reason: null },
      { from_status: 'under_review', to_status: 'approved', reason: null },
      { from_status: 'approved', to_status: 'credited', reason: null },
    ]);
  });

  it('rejected: booked, then returned to sender, with the reason recorded', async () => {
    const { accountId, paymentId } = await newPayment();
    await withTransaction(pool, (tx) => book(tx, paymentId));
    await withTransaction(pool, (tx) => startReview(tx, paymentId));
    const rejected = { outcome: 'rejected', reason: 'sanctions screening hit' } as const;
    expect(await withTransaction(pool, (tx) => decide(tx, paymentId, rejected))).toBe('applied');

    expect(await withTransaction(pool, (tx) => postDecision(tx, paymentId))).toBe('returned');
    expect(await statusOf(pool, paymentId)).toBe('returned');
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 0n, available: 0n });
    expect((await history(paymentId))[3]).toEqual({
      from_status: 'under_review',
      to_status: 'rejected',
      reason: 'sanctions screening hit',
    });
  });

  it('skips every transition when repeated, changing nothing', async () => {
    const { accountId, paymentId } = await newPayment();
    const approved = { outcome: 'approved' } as const;
    const steps = [
      () => withTransaction(pool, (tx) => book(tx, paymentId)),
      () => withTransaction(pool, (tx) => startReview(tx, paymentId)),
      () => withTransaction(pool, (tx) => decide(tx, paymentId, approved)),
      () => withTransaction(pool, (tx) => credit(tx, paymentId)),
    ];
    for (const step of steps) {
      expect(await step()).toBe('applied');
      expect(await step()).toBe('skipped');
    }
    // Every step once more, after the payment is credited.
    for (const step of steps) expect(await step()).toBe('skipped');
    expect(await withTransaction(pool, (tx) => postDecision(tx, paymentId))).toBe('skipped');

    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await history(paymentId)).toHaveLength(5);
  });

  it('skips transitions out of order', async () => {
    const { accountId, paymentId } = await newPayment();
    const approved = { outcome: 'approved' } as const;
    expect(await withTransaction(pool, (tx) => startReview(tx, paymentId))).toBe('skipped');
    expect(await withTransaction(pool, (tx) => decide(tx, paymentId, approved))).toBe('skipped');
    expect(await withTransaction(pool, (tx) => credit(tx, paymentId))).toBe('skipped');
    expect(await withTransaction(pool, (tx) => returnToSender(tx, paymentId))).toBe('skipped');
    expect(await withTransaction(pool, (tx) => postDecision(tx, paymentId))).toBe('skipped');

    expect(await statusOf(pool, paymentId)).toBe('created');
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 0n, available: 0n });
    expect(await history(paymentId)).toHaveLength(1);
  });

  // Compile-time only: tsc fails if a line marked @ts-expect-error does NOT error, so this
  // proves illegal transitions can't be written. The function is never called.
  it('rejects illegal transitions at compile time', () => {
    const paymentId = newUuid('pay');
    const illegal = (tx: Tx): Promise<unknown> => {
      // @ts-expect-error: credited -> created is not a legal transition
      return transitionStatus(tx, { paymentId, from: 'credited', to: 'created' });
    };
    const skipsAStep = (tx: Tx): Promise<unknown> => {
      // @ts-expect-error: created -> under_review skips booking
      return transitionStatus(tx, { paymentId, from: 'created', to: 'under_review' });
    };
    expect([illegal, skipsAStep]).toHaveLength(2);
  });

  it('never re-makes a recorded decision', async () => {
    const { paymentId } = await newPayment();
    await withTransaction(pool, (tx) => book(tx, paymentId));
    await withTransaction(pool, (tx) => startReview(tx, paymentId));
    await withTransaction(pool, (tx) => decide(tx, paymentId, { outcome: 'approved' }));

    const reject = { outcome: 'rejected', reason: 'second opinion' } as const;
    expect(await withTransaction(pool, (tx) => decide(tx, paymentId, reject))).toBe('skipped');
    expect(await statusOf(pool, paymentId)).toBe('approved');
  });

  it('books exactly once under concurrency', async () => {
    const { accountId, paymentId } = await newPayment();
    const outcomes = await Promise.all(
      Array.from({ length: 10 }, () => withTransaction(pool, (tx) => book(tx, paymentId))),
    );

    expect(outcomes.filter((o) => o === 'applied')).toHaveLength(1);
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 1250n, available: 0n });
    expect(await history(paymentId)).toHaveLength(2);
  });

  it('rolls back a transition whose movement already exists, and reports it', async () => {
    const { accountId, paymentId } = await newPayment();
    // The ledger already holds this payment's booking, but the payment is still `created`:
    // the two guards disagree.
    await withTransaction(pool, (tx) =>
      move(tx, {
        accountId,
        from: 'external',
        to: 'pending_in',
        amountMinor: 1250n,
        referenceType: 'payment',
        referenceId: paymentId,
        referenceStep: 'book',
      }),
    );

    await expect(withTransaction(pool, (tx) => book(tx, paymentId))).rejects.toThrow(
      LedgerInconsistencyError,
    );
    // The status change was rolled back with it: still created, no history row added, and
    // pending_in holds only the one booking.
    expect(await statusOf(pool, paymentId)).toBe('created');
    expect(await history(paymentId)).toHaveLength(1);
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 1250n, available: 0n });
  });

  it('keeps payment_status_history append-only', async () => {
    const { paymentId } = await newPayment();
    await expect(
      pool.query("UPDATE payment_status_history SET reason = 'edited' WHERE payment_id = $1", [
        paymentId,
      ]),
    ).rejects.toThrow(/append-only: UPDATE/);
    await expect(
      pool.query('DELETE FROM payment_status_history WHERE payment_id = $1', [paymentId]),
    ).rejects.toThrow(/append-only: DELETE/);
  });
});
