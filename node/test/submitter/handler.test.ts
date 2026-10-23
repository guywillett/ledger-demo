import { afterAll, describe, expect, it } from 'vitest';
import type { DeliveredMessage } from '../../src/consumer/messages.ts';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { move } from '../../src/ledger/move.ts';
import { execute } from '../../src/ledger/withdrawals.ts';
import { withTransaction } from '../../src/shared/db.ts';
import { newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { handleSubmission, type SubmitterDeps } from '../../src/submitter/handler.ts';
import type { SubmitOutcome } from '../../src/submitter/provider.ts';
import { withdrawalHeldEvent } from '../../src/withdrawals/events.ts';
import { getWithdrawal, type Withdrawal } from '../../src/withdrawals/withdrawals.ts';
import {
  balancesOf,
  capturingLogger,
  createTestAccount,
  createTestPool,
  createTestWithdrawal,
  failingDeliveryFinish,
  fundTestAccount,
} from '../helpers.ts';

// The submitter's handler against the test database, with fake messages and a fake provider
// client: a message here is what JetStream would deliver, and "redelivery" is calling the
// handler again.

const pool = createTestPool();
afterAll(() => pool.end());

type FakeProvider = {
  readonly provider: SubmitterDeps['provider'];
  // The withdrawals it was asked to submit, in order (the client derives key and body from them).
  readonly calls: Withdrawal[];
};

// Answers with `outcomes` in turn (the last one repeats).
function fakeProvider(...outcomes: SubmitOutcome[]): FakeProvider {
  const calls: Withdrawal[] = [];
  return {
    calls,
    provider: {
      submitTransfer: async (w) => {
        calls.push(w);
        return outcomes[Math.min(calls.length - 1, outcomes.length - 1)] ?? fail();
      },
    },
  };
}

function fail(): never {
  throw new Error('no outcome scripted');
}

function deps(provider: FakeProvider, overrides: Partial<SubmitterDeps> = {}): SubmitterDeps {
  return {
    pool,
    logger: capturingLogger().logger,
    provider: provider.provider,
    retryBaseDelayMs: 1000,
    retryMaxDelayMs: 60_000,
    ...overrides,
  };
}

type FakeMessage = DeliveredMessage & { readonly calls: string[]; readonly naks: number[] };

function message(body: string, deliveryAttempt = 1): FakeMessage {
  const calls: string[] = [];
  const naks: number[] = [];
  return {
    body,
    natsMsgId: 'wd.held',
    deliveryAttempt,
    calls,
    naks,
    ack: () => calls.push('ack'),
    term: () => calls.push('term'),
    nak: (delayMs) => {
      calls.push('nak');
      naks.push(delayMs);
    },
    working: () => calls.push('working'),
  };
}

// A held withdrawal of 400 from an account funded with 1000, and its event as published.
async function heldWithdrawal(): Promise<{
  withdrawal: Withdrawal;
  body: string;
  eventId: Uuid<'evt'>;
}> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, 1000n);
  const withdrawal = await createTestWithdrawal(pool, account, 400n);
  const eventId = newUuid('evt');
  const body = JSON.stringify(withdrawalHeldEvent(withdrawal, toTypeId('evt', eventId)));
  return { withdrawal, body, eventId };
}

async function deliveriesOf(
  eventId: Uuid<'evt'>,
): Promise<{ consumer: string; outcome: string }[]> {
  const result = await pool.query<{ consumer: string; outcome: string }>(
    'SELECT consumer, outcome FROM event_deliveries WHERE event_id = $1 ORDER BY id',
    [eventId],
  );
  return result.rows;
}

const ACCEPTED: SubmitOutcome = { kind: 'accepted', transferId: 'tr_1' };

describe('handleSubmission', () => {
  it('submits a held withdrawal: submitted with the transfer ID, built from the stored withdrawal', async () => {
    const { withdrawal, body, eventId } = await heldWithdrawal();
    const provider = fakeProvider(ACCEPTED);
    const msg = message(body);

    expect(await handleSubmission(deps(provider), msg)).toBe('applied');
    // working() before the call resets ack_wait, so the call starts with the full lease.
    expect(msg.calls).toEqual(['working', 'ack']);
    expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({
      status: 'submitted',
      providerTransferId: 'tr_1',
    });
    // The client got the stored withdrawal (it sends its TypeID as the key, its fields as the
    // body; test/submitter/provider.test.ts checks the wire).
    expect(provider.calls).toEqual([withdrawal]);
    expect(await deliveriesOf(eventId)).toEqual([{ consumer: 'submitter', outcome: 'applied' }]);
  });

  it('records nothing on a redelivery after the submission was recorded (a crash before the ack)', async () => {
    const { withdrawal, body, eventId } = await heldWithdrawal();
    const provider = fakeProvider(ACCEPTED);
    expect(await handleSubmission(deps(provider), message(body))).toBe('applied');

    // Redelivered as if the ack was lost: the withdrawal is already submitted, so no call.
    const again = message(body, 2);
    expect(await handleSubmission(deps(provider), again)).toBe('duplicate');
    expect(again.calls).toEqual(['ack']);
    expect(provider.calls).toHaveLength(1);
    expect(await deliveriesOf(eventId)).toEqual([
      { consumer: 'submitter', outcome: 'applied' },
      { consumer: 'submitter', outcome: 'duplicate' },
    ]);
    expect((await getWithdrawal(pool, withdrawal.id))?.providerTransferId).toBe('tr_1');
  });

  it('treats an acceptance for a withdrawal settled meanwhile as a duplicate (markSubmitted skips)', async () => {
    const { withdrawal, body } = await heldWithdrawal();
    // The provider's update settles it while the call is in flight (decision 27).
    const provider: FakeProvider = {
      calls: [],
      provider: {
        submitTransfer: async (w) => {
          provider.calls.push(w);
          await withTransaction(pool, (tx) => execute(tx, w.id, 'tr_1'));
          return ACCEPTED;
        },
      },
    };
    expect(await handleSubmission(deps(provider), message(body))).toBe('duplicate');
    expect((await getWithdrawal(pool, withdrawal.id))?.status).toBe('executed');
  });

  it('makes no call for a withdrawal no longer held: duplicate', async () => {
    const { withdrawal, body } = await heldWithdrawal();
    await withTransaction(pool, (tx) => execute(tx, withdrawal.id, 'tr_early'));
    const provider = fakeProvider(ACCEPTED);
    const msg = message(body);
    expect(await handleSubmission(deps(provider), msg)).toBe('duplicate');
    expect(msg.calls).toEqual(['ack']);
    expect(provider.calls).toEqual([]);
  });

  it('releases the funds on a refusal: failed with the reason', async () => {
    const { withdrawal, body } = await heldWithdrawal();
    const provider = fakeProvider({ kind: 'refused', reason: 'creditor account closed' });
    const msg = message(body);
    expect(await handleSubmission(deps(provider), msg)).toBe('applied');
    expect(msg.calls).toEqual(['working', 'ack']);
    expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({
      status: 'failed',
      failureReason: 'creditor account closed',
    });
    expect(await balancesOf(pool, withdrawal.accountId)).toEqual({
      pendingIn: 0n,
      available: 1000n,
      held: 0n,
    });
  });

  it('naks an unknown outcome with a growing delay, leaves it held, and the redelivery submits it', async () => {
    const { withdrawal, body, eventId } = await heldWithdrawal();
    const provider = fakeProvider(
      { kind: 'unknown', cause: 'timeout' },
      { kind: 'unknown', cause: 'provider answered 503' },
      ACCEPTED,
    );

    const first = message(body, 1);
    const second = message(body, 2);
    expect(await handleSubmission(deps(provider), first)).toBe('failed');
    expect(await handleSubmission(deps(provider), second)).toBe('failed');
    expect([...first.naks, ...second.naks]).toEqual([1000, 2000]);
    expect(await getWithdrawal(pool, withdrawal.id)).toMatchObject({ status: 'held' });
    expect(await balancesOf(pool, withdrawal.accountId)).toMatchObject({ held: 400n });

    const third = message(body, 3);
    expect(await handleSubmission(deps(provider), third)).toBe('applied');
    expect((await getWithdrawal(pool, withdrawal.id))?.status).toBe('submitted');
    // The same withdrawal (so the same key) every time; never a new one.
    expect(new Set(provider.calls.map((w) => w.id))).toEqual(new Set([withdrawal.id]));
    expect((await deliveriesOf(eventId)).map((d) => d.outcome)).toEqual([
      'failed',
      'failed',
      'applied',
    ]);
  });

  it.each([
    ['invalid (400)', { kind: 'invalid', fields: ['creditor_name'], bodyBytes: 120 }],
    ['key_conflict (409)', { kind: 'key_conflict' }],
  ] as const)(
    'treats %s as our bug: fatal alert, term, still held, nothing moved',
    async (_case, outcome) => {
      const { withdrawal, body } = await heldWithdrawal();
      const { logger, lines } = capturingLogger();
      const msg = message(body);
      expect(
        await handleSubmission(deps(fakeProvider(outcome as SubmitOutcome), { logger }), msg),
      ).toBe('failed');
      expect(msg.calls).toEqual(['working', 'term']);
      expect((await getWithdrawal(pool, withdrawal.id))?.status).toBe('held');
      expect(await balancesOf(pool, withdrawal.accountId)).toEqual({
        pendingIn: 0n,
        available: 600n,
        held: 400n,
      });
      expect(lines().find((l) => l.level === 'fatal')).toMatchObject({ alert: true });
      // Reconciliation lists it as stuck, for a person to resolve.
      const report = await reconcile(pool, {
        accountIds: [withdrawal.accountId],
        stuckAfterMs: 0,
      });
      expect(report.stuckWithdrawals.map((s) => s.withdrawalId)).toEqual([withdrawal.id]);
      expect(report.consistent).toBe(true);
    },
  );

  it('terminates a bug outcome even when its outcome cannot be recorded', async () => {
    // Signal vs record (consumer/messages.ts): a stop is signalled first, so a failed write to
    // the delivery log can't leave the message to be redelivered and the alert repeated.
    const { withdrawal, body, eventId } = await heldWithdrawal();
    const msg = message(body);
    const outcome = await handleSubmission(
      deps(fakeProvider({ kind: 'key_conflict' }), { pool: failingDeliveryFinish(pool) }),
      msg,
    );
    expect(outcome).toBe('failed');
    expect(msg.calls).toEqual(['working', 'term']);
    expect((await getWithdrawal(pool, withdrawal.id))?.status).toBe('held');
    // The trace left behind: a delivery still in_progress, which reconciliation reports.
    expect(await deliveriesOf(eventId)).toEqual([
      { consumer: 'submitter', outcome: 'in_progress' },
    ]);
  });

  it('treats a transfer-ID mismatch from markSubmitted as an invariant violation', async () => {
    const { withdrawal, body } = await heldWithdrawal();
    const { logger, lines } = capturingLogger();
    // The provider's update stored tr_A (and executed it), then this submission gets tr_B. The
    // pre-call read must still see `held`, so settle it during the call.
    const provider: FakeProvider = {
      calls: [],
      provider: {
        submitTransfer: async (w) => {
          provider.calls.push(w);
          await withTransaction(pool, (tx) => execute(tx, w.id, 'tr_A'));
          return { kind: 'accepted', transferId: 'tr_B' };
        },
      },
    };
    const msg = message(body);
    expect(await handleSubmission(deps(provider, { logger }), msg)).toBe('failed');
    expect(msg.calls).toEqual(['working', 'term']);
    // Both transfer IDs are logged with the error (pino's err serializer copies its fields).
    expect(lines().find((l) => l.level === 'fatal')).toMatchObject({
      alert: true,
      msg: 'invariant violated; terminating the message',
      err: expect.objectContaining({ storedTransferId: 'tr_A', reportedTransferId: 'tr_B' }),
    });
    expect((await getWithdrawal(pool, withdrawal.id))?.providerTransferId).toBe('tr_A');
  });

  it('terminates a ledger invariant violation with an alert, as the worker does', async () => {
    // A release movement already exists for a withdrawal still held: the two guards disagree,
    // so release() throws LedgerInconsistencyError. Retrying can't fix it.
    const { withdrawal, body } = await heldWithdrawal();
    await withTransaction(pool, (tx) =>
      move(tx, {
        accountId: withdrawal.accountId,
        from: 'held',
        to: 'available',
        amountMinor: withdrawal.amountMinor,
        referenceType: 'withdrawal',
        referenceId: withdrawal.id,
        referenceStep: 'release',
      }),
    );
    const before = await balancesOf(pool, withdrawal.accountId);
    const { logger, lines } = capturingLogger();
    const msg = message(body);

    const outcome = await handleSubmission(
      deps(fakeProvider({ kind: 'refused', reason: 'creditor account closed' }), { logger }),
      msg,
    );
    expect(outcome).toBe('failed');
    expect(msg.calls).toEqual(['working', 'term']);
    expect(lines().find((l) => l.level === 'fatal')).toMatchObject({
      alert: true,
      msg: 'invariant violated; terminating the message',
      err: expect.objectContaining({ type: 'LedgerInconsistencyError' }),
    });
    // The transition rolled back with the failed movement.
    expect((await getWithdrawal(pool, withdrawal.id))?.status).toBe('held');
    expect(await balancesOf(pool, withdrawal.accountId)).toEqual(before);
  });

  // A crash between the call and the transaction looks the same: accepted, nothing recorded.
  it('naks a transient failure after an accepted call; the redelivery sends the same request and records it', async () => {
    const { withdrawal, body } = await heldWithdrawal();
    const provider = fakeProvider(ACCEPTED);
    // A pool whose transactions fail, as when the database goes away after the call.
    const broken = {
      connect: async () => {
        throw new Error('database unavailable');
      },
      query: pool.query.bind(pool),
    } as unknown as SubmitterDeps['pool'];
    const msg = message(body);
    expect(await handleSubmission(deps(provider, { pool: broken }), msg)).toBe('failed');
    expect(msg.calls).toEqual(['working', 'nak']);
    expect((await getWithdrawal(pool, withdrawal.id))?.status).toBe('held');

    expect(await handleSubmission(deps(provider), message(body, 2))).toBe('applied');
    expect((await getWithdrawal(pool, withdrawal.id))?.providerTransferId).toBe('tr_1');
    // Accepted twice, with the identical withdrawal (so the same key and body) both times.
    expect(provider.calls).toHaveLength(2);
    expect(provider.calls[1]).toEqual(provider.calls[0]);
  });

  it.each([
    ['not JSON', 'not json'],
    ['not a withdrawal.held event', JSON.stringify({ event_type: 'payment.created' })],
  ])('terminates an invalid event (%s)', async (_case, body) => {
    const msg = message(body);
    expect(await handleSubmission(deps(fakeProvider(ACCEPTED)), msg)).toBe('invalid');
    expect(msg.calls).toEqual(['term']);
  });

  it('terminates an event for an unknown withdrawal', async () => {
    const { withdrawal } = await heldWithdrawal();
    const ghost = { ...withdrawal, id: newUuid('wd') };
    const eventId = newUuid('evt');
    const body = JSON.stringify(withdrawalHeldEvent(ghost, toTypeId('evt', eventId)));
    const provider = fakeProvider(ACCEPTED);
    const msg = message(body);
    expect(await handleSubmission(deps(provider), msg)).toBe('invalid');
    expect(msg.calls).toEqual(['term']);
    expect(provider.calls).toEqual([]);
    expect(await deliveriesOf(eventId)).toEqual([{ consumer: 'submitter', outcome: 'invalid' }]);
  });

  it('logs no name, IBAN or remittance text', async () => {
    const { body } = await heldWithdrawal();
    const { logger, lines } = capturingLogger();
    await handleSubmission(deps(fakeProvider(ACCEPTED), { logger }), message(body));
    const everything = JSON.stringify(lines());
    expect(everything).not.toContain('Jane Doe');
    expect(everything).not.toContain('test withdrawal');
    expect(lines().some((l) => l.msg === 'withdrawal submitted')).toBe(true);
  });
});
