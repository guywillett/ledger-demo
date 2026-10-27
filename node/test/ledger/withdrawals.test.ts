import { randomUUID } from 'node:crypto';
import { afterAll, describe, expect, it } from 'vitest';
import { OverdraftError, type MovementReference } from '../../src/ledger/move.ts';
import {
  createWithdrawal,
  execute,
  InsufficientFundsError,
  markSubmitted,
  release,
} from '../../src/ledger/withdrawals.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid, type Uuid } from '../../src/shared/ids.ts';
import {
  TransferIdMismatchError,
  withdrawalStatusHistory,
  type WithdrawalTransition,
} from '../../src/withdrawals/status.ts';
import { getWithdrawal } from '../../src/withdrawals/withdrawals.ts';
import {
  balancesOf,
  createTestAccount,
  createTestPool,
  createTestWithdrawal,
  fundTestAccount,
  newTestIban,
  type TestAccount,
} from '../helpers.ts';

// The withdrawal lifecycle against the test database: hold, submit, execute, release. Each test
// uses its own account, funded through a real deposit (fundTestAccount).

const pool = createTestPool();
afterAll(() => pool.end());

async function fundedAccount(amountMinor = 1000n): Promise<TestAccount> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, amountMinor);
  return account;
}

async function movementSteps(withdrawalId: Uuid<'wd'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    `SELECT reference_step FROM balance_movements
     WHERE reference_type = 'withdrawal' AND reference_id = $1 ORDER BY created_at`,
    [withdrawalId],
  );
  return result.rows.map((r) => r.reference_step);
}

async function statusOf(withdrawalId: Uuid<'wd'>): Promise<string | undefined> {
  return (await getWithdrawal(pool, withdrawalId))?.status;
}

async function historyOf(withdrawalId: Uuid<'wd'>): Promise<[string | null, string][]> {
  const history = await withdrawalStatusHistory(pool, withdrawalId);
  return history.map((change) => [change.from, change.to]);
}

async function withdrawalCount(account: TestAccount): Promise<number> {
  const result = await pool.query<{ n: number }>(
    'SELECT count(*)::int AS n FROM withdrawals WHERE account_id = $1',
    [account.id],
  );
  return result.rows[0]?.n ?? 0;
}

async function submit(
  withdrawalId: Uuid<'wd'>,
  transferId = `tr_${randomUUID()}`,
): Promise<'applied' | 'skipped'> {
  return withTransaction(pool, (tx) => markSubmitted(tx, withdrawalId, transferId));
}

describe('hold', () => {
  it('moves the amount from available to held, with the withdrawal and its first history row', async () => {
    const account = await fundedAccount(1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 400n);

    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 600n,
      held: 400n,
    });
    expect(await movementSteps(withdrawal.id)).toEqual(['hold']);
    expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({
      accountId: account.id,
      amountMinor: 400n,
      currency: 'EUR',
      creditorName: 'Jane Doe',
      status: 'held',
      failureReason: null,
      providerTransferId: null,
    });
    expect(await historyOf(withdrawal.id)).toEqual([[null, 'held']]);
  });

  it('throws InsufficientFundsError and, after the rollback, nothing is stored', async () => {
    const account = await fundedAccount(1000n);
    const error = await createTestWithdrawal(pool, account, 1001n).catch((err: unknown) => err);

    expect(error).toBeInstanceOf(InsufficientFundsError);
    // A business outcome, not the OverdraftError the deposit-worker treats as a bug; the cause
    // keeps the database's refusal.
    expect(error).not.toBeInstanceOf(OverdraftError);
    expect((error as InsufficientFundsError).cause).toBeInstanceOf(OverdraftError);
    expect(error).toMatchObject({ accountId: account.id, amountMinor: 1001n });

    expect(await withdrawalCount(account)).toBe(0);
    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 1000n,
      held: 0n,
    });
    const movements = await pool.query(
      `SELECT 1 FROM balance_movements WHERE account_id = $1 AND reference_type = 'withdrawal'`,
      [account.id],
    );
    expect(movements.rowCount).toBe(0);
  });

  it('lets exactly one of two concurrent withdrawals that together exceed available commit', async () => {
    const account = await fundedAccount(1000n);
    // Two transactions on two connections. Both inserts succeed (different keys); the holds then
    // update the same balance row, so the second waits for the first's row lock. Once the first
    // commits, the second's update sees available = 300 and the CHECK refuses it.
    const results = await Promise.allSettled([
      createTestWithdrawal(pool, account, 700n),
      createTestWithdrawal(pool, account, 700n),
    ]);

    const committed = results.filter((r) => r.status === 'fulfilled');
    const refused = results.filter((r) => r.status === 'rejected');
    expect(committed).toHaveLength(1);
    expect(refused).toHaveLength(1);
    expect(refused[0]?.reason).toBeInstanceOf(InsufficientFundsError);

    expect(await withdrawalCount(account)).toBe(1);
    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 300n,
      held: 700n,
    });
  });

  it('inserts nothing for a key the account has already used, so nothing is held twice', async () => {
    const account = await fundedAccount(1000n);
    const key = randomUUID();
    const first = await createTestWithdrawal(pool, account, 100n, key);

    const repeat = await withTransaction(pool, (tx) =>
      createWithdrawal(tx, {
        accountId: account.id,
        idempotencyKey: key,
        amountMinor: 100n,
        currency: 'EUR',
        creditorIban: newTestIban(),
        creditorName: 'Jane Doe',
        remittanceInfo: null,
      }),
    );
    expect(repeat).toBeUndefined();
    expect(await withdrawalCount(account)).toBe(1);
    expect(await movementSteps(first.id)).toEqual(['hold']);
    expect(await balancesOf(pool, account.id)).toMatchObject({ available: 900n, held: 100n });

    // The same key on another account is another withdrawal.
    const other = await fundedAccount(1000n);
    await createTestWithdrawal(pool, other, 100n, key);
    expect(await withdrawalCount(other)).toBe(1);
  });
});

describe('withdrawals table', () => {
  it("stores no remittance information as NULL only: '' is refused", async () => {
    const account = await fundedAccount(1000n);
    const create = withTransaction(pool, (tx) =>
      createWithdrawal(tx, {
        accountId: account.id,
        idempotencyKey: randomUUID(),
        amountMinor: 100n,
        currency: 'EUR',
        creditorIban: newTestIban(),
        creditorName: 'Jane Doe',
        remittanceInfo: '',
      }),
    );
    // Both remittance_info CHECKs refuse '' (the length and the character set, which needs at
    // least one character); which one Postgres reports first doesn't matter.
    await expect(create).rejects.toMatchObject({
      code: '23514',
      constraint: expect.stringMatching(/^withdrawals_remittance_info_(charset_)?check$/),
    });
    expect(await withdrawalCount(account)).toBe(0);
  });

  // Decision 32: the SEPA basic character set only, rejected rather than transliterated.
  it.each([
    ['an accent in the name', { creditorName: 'Zoë Müller' }, 'creditor_name'],
    ["'&' in the name", { creditorName: 'Smith & Sons' }, 'creditor_name'],
    ['a name of only spaces', { creditorName: '   ' }, 'creditor_name'],
    ['an accent in the remittance information', { remittanceInfo: 'Café rent' }, 'remittance_info'],
  ] as const)('refuses %s', async (_case, fields, column) => {
    const account = await fundedAccount(1000n);
    const create = withTransaction(pool, (tx) =>
      createWithdrawal(tx, {
        accountId: account.id,
        idempotencyKey: randomUUID(),
        amountMinor: 100n,
        currency: 'EUR',
        creditorIban: newTestIban(),
        creditorName: 'Jane Doe',
        remittanceInfo: null,
        ...fields,
      }),
    );
    await expect(create).rejects.toMatchObject({
      code: '23514',
      constraint: `withdrawals_${column}_charset_check`,
    });
    expect(await withdrawalCount(account)).toBe(0);
  });

  it("accepts every character of the set: letters, digits and / - ? : ( ) . , ' +", async () => {
    const account = await fundedAccount(1000n);
    const withdrawal = await withTransaction(pool, (tx) =>
      createWithdrawal(tx, {
        accountId: account.id,
        idempotencyKey: randomUUID(),
        amountMinor: 100n,
        currency: 'EUR',
        creditorIban: newTestIban(),
        creditorName: "O'Brien-Smith (UK) Ltd.",
        remittanceInfo: 'Inv 2026/10, ref: A+B? 99',
      }),
    );
    expect(withdrawal?.creditorName).toBe("O'Brien-Smith (UK) Ltd.");
  });
});

describe('markSubmitted', () => {
  it('moves held to submitted, storing the provider transfer ID, without moving money', async () => {
    const account = await fundedAccount(1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 400n);

    expect(
      await withTransaction(pool, (tx) => markSubmitted(tx, withdrawal.id, 'tr_provider_1')),
    ).toBe('applied');
    expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({
      status: 'submitted',
      providerTransferId: 'tr_provider_1',
    });
    expect(await movementSteps(withdrawal.id)).toEqual(['hold']);
    expect(await balancesOf(pool, account.id)).toMatchObject({ available: 600n, held: 400n });

    // A repeat (the submitter retrying, same transfer) skips.
    expect(await submit(withdrawal.id, 'tr_provider_1')).toBe('skipped');
    expect((await getWithdrawal(pool, withdrawal.id))?.providerTransferId).toBe('tr_provider_1');
  });
});

describe('execute', () => {
  it.each([['held'], ['submitted']] as const)(
    'moves %s to executed: the held funds leave, once',
    async (origin) => {
      const account = await fundedAccount(1000n);
      const withdrawal = await createTestWithdrawal(pool, account, 400n);
      if (origin === 'submitted') await submit(withdrawal.id);

      expect(await withTransaction(pool, (tx) => execute(tx, withdrawal.id))).toBe('applied');
      expect(await statusOf(withdrawal.id)).toBe('executed');
      expect(await balancesOf(pool, account.id)).toEqual({
        pendingIn: 0n,
        available: 600n,
        held: 0n,
      });
      expect(await movementSteps(withdrawal.id)).toEqual(['hold', 'execute']);
      // The history records the status it actually came from.
      expect((await historyOf(withdrawal.id)).at(-1)).toEqual([origin, 'executed']);

      expect(await withTransaction(pool, (tx) => execute(tx, withdrawal.id))).toBe('skipped');
      expect(await movementSteps(withdrawal.id)).toEqual(['hold', 'execute']);
    },
  );
});

describe('release', () => {
  it.each([['held'], ['submitted']] as const)(
    'moves %s to failed with the reason: the funds go back to available',
    async (origin) => {
      const account = await fundedAccount(1000n);
      const withdrawal = await createTestWithdrawal(pool, account, 400n);
      if (origin === 'submitted') await submit(withdrawal.id);

      expect(
        await withTransaction(pool, (tx) => release(tx, withdrawal.id, 'destination closed')),
      ).toBe('applied');
      expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({
        status: 'failed',
        failureReason: 'destination closed',
      });
      expect(await balancesOf(pool, account.id)).toEqual({
        pendingIn: 0n,
        available: 1000n,
        held: 0n,
      });
      expect(await movementSteps(withdrawal.id)).toEqual(['hold', 'release']);
      const history = await withdrawalStatusHistory(pool, withdrawal.id);
      expect(history.at(-1)).toMatchObject({
        from: origin,
        to: 'failed',
        reason: 'destination closed',
      });

      expect(await withTransaction(pool, (tx) => release(tx, withdrawal.id, 'again'))).toBe(
        'skipped',
      );
    },
  );
});

describe('final statuses', () => {
  it('never change: execute after release, and release after execute, skip and move nothing', async () => {
    const account = await fundedAccount(1000n);
    const released = await createTestWithdrawal(pool, account, 100n);
    const executed = await createTestWithdrawal(pool, account, 200n);
    await withTransaction(pool, (tx) => release(tx, released.id, 'refused'));
    await withTransaction(pool, (tx) => execute(tx, executed.id));
    const before = await balancesOf(pool, account.id);

    expect(await withTransaction(pool, (tx) => execute(tx, released.id))).toBe('skipped');
    expect(await withTransaction(pool, (tx) => release(tx, executed.id, 'late'))).toBe('skipped');
    expect(await submit(released.id)).toBe('skipped');

    expect(await statusOf(released.id)).toBe('failed');
    expect(await statusOf(executed.id)).toBe('executed');
    expect(await movementSteps(released.id)).toEqual(['hold', 'release']);
    expect(await movementSteps(executed.id)).toEqual(['hold', 'execute']);
    expect(await balancesOf(pool, account.id)).toEqual(before);
  });

  it('lets the provider’s update win the race: executed from held, then markSubmitted skips', async () => {
    // Decision 27: the webhook settles the withdrawal before the submitter has processed the
    // response to its own request.
    const account = await fundedAccount(1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 400n);

    expect(await withTransaction(pool, (tx) => execute(tx, withdrawal.id, 'tr_from_update'))).toBe(
      'applied',
    );
    // The submitter's response for the same transfer arrives late.
    expect(await submit(withdrawal.id, 'tr_from_update')).toBe('skipped');

    // The update's transfer ID is stored: the skipped submission couldn't record it.
    expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({
      status: 'executed',
      providerTransferId: 'tr_from_update',
    });
    expect(await historyOf(withdrawal.id)).toEqual([
      [null, 'held'],
      ['held', 'executed'],
    ]);
  });
});

describe('provider transfer IDs', () => {
  // Everything about a withdrawal that a refused transition must leave as it was.
  async function snapshot(withdrawalId: Uuid<'wd'>, account: TestAccount): Promise<unknown> {
    return {
      withdrawal: await getWithdrawal(pool, withdrawalId),
      history: await historyOf(withdrawalId),
      movements: await movementSteps(withdrawalId),
      balances: await balancesOf(pool, account.id),
    };
  }

  async function submitted(
    transferId: string,
  ): Promise<{ account: TestAccount; withdrawalId: Uuid<'wd'> }> {
    const account = await fundedAccount(1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 400n);
    await withTransaction(pool, (tx) => markSubmitted(tx, withdrawal.id, transferId));
    return { account, withdrawalId: withdrawal.id };
  }

  it.each([
    ['execute', (id: Uuid<'wd'>) => withTransaction(pool, (tx) => execute(tx, id, 'tr_B'))],
    [
      'release',
      (id: Uuid<'wd'>) => withTransaction(pool, (tx) => release(tx, id, 'refused', 'tr_B')),
    ],
  ] as const)(
    'a %s reporting another ID than the submission throws, changing nothing',
    async (_name, settle) => {
      const { account, withdrawalId } = await submitted('tr_A');
      const before = await snapshot(withdrawalId, account);

      const error = await settle(withdrawalId).catch((err: unknown) => err);
      expect(error).toBeInstanceOf(TransferIdMismatchError);
      expect(error).toMatchObject({
        withdrawalId,
        storedTransferId: 'tr_A',
        reportedTransferId: 'tr_B',
      });
      expect(await snapshot(withdrawalId, account)).toEqual(before);
    },
  );

  it('a settlement with the same ID, or none, applies', async () => {
    const same = await submitted('tr_A');
    expect(await withTransaction(pool, (tx) => execute(tx, same.withdrawalId, 'tr_A'))).toBe(
      'applied',
    );
    const none = await submitted('tr_A');
    expect(await withTransaction(pool, (tx) => release(tx, none.withdrawalId, 'refused'))).toBe(
      'applied',
    );
    expect((await getWithdrawal(pool, none.withdrawalId))?.providerTransferId).toBe('tr_A');
  });

  it('a repeated update for a final withdrawal skips with the same ID, and throws with another', async () => {
    const account = await fundedAccount(1000n);
    const withdrawal = await createTestWithdrawal(pool, account, 400n);
    await withTransaction(pool, (tx) => execute(tx, withdrawal.id, 'tr_A'));
    const before = await snapshot(withdrawal.id, account);

    expect(await withTransaction(pool, (tx) => execute(tx, withdrawal.id, 'tr_A'))).toBe('skipped');
    await expect(
      withTransaction(pool, (tx) => execute(tx, withdrawal.id, 'tr_B')),
    ).rejects.toBeInstanceOf(TransferIdMismatchError);
    expect(await snapshot(withdrawal.id, account)).toEqual(before);
  });

  it('a submission after the update stored another ID throws; after one that stored none, it skips', async () => {
    const account = await fundedAccount(1000n);
    const reported = await createTestWithdrawal(pool, account, 100n);
    await withTransaction(pool, (tx) => execute(tx, reported.id, 'tr_A'));
    await expect(
      withTransaction(pool, (tx) => markSubmitted(tx, reported.id, 'tr_B')),
    ).rejects.toBeInstanceOf(TransferIdMismatchError);
    expect(await withTransaction(pool, (tx) => markSubmitted(tx, reported.id, 'tr_A'))).toBe(
      'skipped',
    );

    const unreported = await createTestWithdrawal(pool, account, 100n);
    await withTransaction(pool, (tx) => release(tx, unreported.id, 'refused'));
    expect(await withTransaction(pool, (tx) => markSubmitted(tx, unreported.id, 'tr_B'))).toBe(
      'skipped',
    );
    expect((await getWithdrawal(pool, unreported.id))?.providerTransferId).toBeNull();
  });
});

describe('types', () => {
  it('make illegal transitions and mismatched movement references compile errors', () => {
    const legal: WithdrawalTransition[] = [
      { from: ['held'], to: 'submitted', providerTransferId: 'tr_1' },
      { from: ['held', 'submitted'], to: 'executed' },
      { from: ['held', 'submitted'], to: 'failed', reason: 'refused' },
    ];
    // @ts-expect-error a final status never changes
    const backwards: WithdrawalTransition = { from: ['executed'], to: 'held' };
    // @ts-expect-error failed needs a reason
    const noReason: WithdrawalTransition = { from: ['held'], to: 'failed' };
    // @ts-expect-error submitted can only be reached from held
    const resubmit: WithdrawalTransition = {
      from: ['submitted'],
      to: 'submitted',
      providerTransferId: 'x',
    };

    const deposit = newUuid('dep');
    const withdrawal = newUuid('wd');
    // @ts-expect-error a deposit has no hold step
    const depositHold: MovementReference = {
      referenceType: 'deposit',
      referenceId: deposit,
      referenceStep: 'hold',
    };
    // @ts-expect-error a withdrawal has no credit step
    const withdrawalCredit: MovementReference = {
      referenceType: 'withdrawal',
      referenceId: withdrawal,
      referenceStep: 'credit',
    };
    // @ts-expect-error a withdrawal reference needs a withdrawal's UUID
    const wrongId: MovementReference = {
      referenceType: 'withdrawal',
      referenceId: deposit,
      referenceStep: 'hold',
    };

    // Runtime is irrelevant here; tsc (npm test runs it first) is the assertion.
    expect([
      legal,
      backwards,
      noReason,
      resubmit,
      depositHold,
      withdrawalCredit,
      wrongId,
    ]).toHaveLength(7);
  });
});
