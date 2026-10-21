import { afterAll, describe, expect, it } from 'vitest';
import { decideRandomly } from '../../src/compliance/decision.ts';
import { decide, startReview, type Decision } from '../../src/compliance/transitions.ts';
import { paymentCreatedEvent } from '../../src/ingestion/events.ts';
import type { Payment } from '../../src/ingestion/payments.ts';
import { move } from '../../src/ledger/move.ts';
import { book } from '../../src/ledger/posting.ts';
import { createPool, withTransaction } from '../../src/shared/db.ts';
import { newUuid, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { createLogger } from '../../src/shared/logger.ts';
import { startDelivery } from '../../src/worker/deliveries.ts';
import {
  handleDelivery,
  type DeliveredMessage,
  type HandlerDeps,
} from '../../src/worker/handler.ts';
import {
  balancesOf,
  capturingLogger,
  createTestAccount,
  createTestPayment,
  createTestPool,
  statusOf,
} from '../helpers.ts';

// The worker's handler against the test database, with fake messages. No NATS: a message here
// is what JetStream would deliver, and "redelivery" is calling the handler again.

const pool = createTestPool();
afterAll(() => pool.end());

const APPROVE: Decision = { outcome: 'approved' };
const REJECT: Decision = { outcome: 'rejected', reason: 'test rejection' };

function deps(overrides: Partial<HandlerDeps> = {}): HandlerDeps {
  return {
    pool,
    logger: createLogger({ service: 'test', level: 'silent' }),
    decide: () => APPROVE,
    reviewDelayMs: 0,
    workingIntervalMs: 10,
    retryDelayMs: 5000,
    ...overrides,
  };
}

type FakeMessage = DeliveredMessage & { readonly calls: string[] };

function message(body: string, natsMsgId = 'msg-1', deliveryAttempt = 1): FakeMessage {
  const calls: string[] = [];
  return {
    body,
    natsMsgId,
    deliveryAttempt,
    calls,
    ack: () => calls.push('ack'),
    term: () => calls.push('term'),
    nak: () => calls.push('nak'),
    working: () => calls.push('working'),
  };
}

// The event exactly as the API publishes it, as JSON.
function eventFor(payment: Payment): { body: string; eventId: Uuid<'evt'> } {
  const eventId = newUuid('evt');
  const event = paymentCreatedEvent(payment, toTypeId('evt', eventId));
  return { body: JSON.stringify(event), eventId };
}

async function newPayment(): Promise<Payment> {
  return createTestPayment(pool, await createTestAccount(pool));
}

async function deliveriesOf(
  eventId: Uuid<'evt'>,
): Promise<{ outcome: string; nats_msg_id: string | null; delivery_attempt: number }[]> {
  const result = await pool.query<{
    outcome: string;
    nats_msg_id: string | null;
    delivery_attempt: number;
  }>(
    'SELECT outcome, nats_msg_id, delivery_attempt FROM event_deliveries WHERE event_id = $1 ORDER BY id',
    [eventId],
  );
  return result.rows;
}

async function movementSteps(paymentId: Uuid<'pay'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    "SELECT reference_step FROM balance_movements WHERE reference_type = 'payment' AND reference_id = $1 ORDER BY created_at",
    [paymentId],
  );
  return result.rows.map((r) => r.reference_step);
}

async function historyOf(paymentId: Uuid<'pay'>): Promise<string[]> {
  const result = await pool.query<{ to_status: string }>(
    'SELECT to_status FROM payment_status_history WHERE payment_id = $1 ORDER BY id',
    [paymentId],
  );
  return result.rows.map((r) => r.to_status);
}

describe('handleDelivery', () => {
  it('books, reviews, decides and credits an approved payment, then acks', async () => {
    const payment = await newPayment();
    const { body, eventId } = eventFor(payment);
    const { logger, lines } = capturingLogger();
    const msg = message(body, `${toTypeId('pay', payment.id)}.created`);

    expect(await handleDelivery(deps({ logger }), msg)).toBe('applied');
    expect(msg.calls).toEqual(['ack']);
    expect(await statusOf(pool, payment.id)).toBe('credited');
    expect(await balancesOf(pool, payment.accountId)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await movementSteps(payment.id)).toEqual(['book', 'credit']);
    expect(await deliveriesOf(eventId)).toEqual([
      { outcome: 'applied', nats_msg_id: msg.natsMsgId, delivery_attempt: 1 },
    ]);
    expect(lines().find((l) => l.msg === 'payment processed')).toMatchObject({
      event_id: toTypeId('evt', eventId),
      payment_id: toTypeId('pay', payment.id),
      booked: 'applied',
      decided: 'approved',
      posted: 'credited',
    });
  });

  it('returns a rejected payment to the sender', async () => {
    const payment = await newPayment();
    const msg = message(eventFor(payment).body);
    expect(await handleDelivery(deps({ decide: () => REJECT }), msg)).toBe('applied');
    expect(await statusOf(pool, payment.id)).toBe('returned');
    expect(await balancesOf(pool, payment.accountId)).toEqual({ pendingIn: 0n, available: 0n });
    expect(await movementSteps(payment.id)).toEqual(['book', 'return']);
  });

  it('applies the same event once when delivered twice with different message IDs', async () => {
    const payment = await newPayment();
    const { body, eventId } = eventFor(payment);

    expect(await handleDelivery(deps(), message(body, 'first'))).toBe('applied');
    const second = message(body, 'second');
    expect(await handleDelivery(deps(), second)).toBe('duplicate');
    expect(second.calls).toEqual(['ack']);

    expect(await deliveriesOf(eventId)).toEqual([
      { outcome: 'applied', nats_msg_id: 'first', delivery_attempt: 1 },
      { outcome: 'duplicate', nats_msg_id: 'second', delivery_attempt: 1 },
    ]);
    expect(await balancesOf(pool, payment.accountId)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await movementSteps(payment.id)).toEqual(['book', 'credit']);
  });

  it('applies the same event once when two consumers process it concurrently', async () => {
    const payment = await newPayment();
    const { body } = eventFor(payment);
    // A short review so the two runs overlap.
    const overlapping = deps({ reviewDelayMs: 30 });
    await Promise.all([
      handleDelivery(overlapping, message(body, 'a')),
      handleDelivery(overlapping, message(body, 'b')),
    ]);

    // Each did whatever steps it got to first; the money moved once.
    expect(await balancesOf(pool, payment.accountId)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await movementSteps(payment.id)).toEqual(['book', 'credit']);
    expect(await historyOf(payment.id)).toEqual([
      'created',
      'received',
      'under_review',
      'approved',
      'credited',
    ]);
  });

  // A crash after Tx N leaves exactly the state of Tx 1..N committed plus a delivery row left
  // in_progress (each Tx commits on its own). These tests create that state directly, then
  // run the handler as the redelivery.
  it('completes after a crash between booking and the decision, crediting pending_in once', async () => {
    const payment = await newPayment();
    const { body, eventId } = eventFor(payment);
    await startDelivery(pool, { eventId, natsMsgId: 'crashed', deliveryAttempt: 1 });
    await withTransaction(pool, (tx) => book(tx, payment.id)); // Tx 1, then the crash

    expect(await handleDelivery(deps(), message(body, 'crashed', 2))).toBe('applied');
    expect(await balancesOf(pool, payment.accountId)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await movementSteps(payment.id)).toEqual(['book', 'credit']);
    expect(await deliveriesOf(eventId)).toEqual([
      { outcome: 'in_progress', nats_msg_id: 'crashed', delivery_attempt: 1 },
      { outcome: 'applied', nats_msg_id: 'crashed', delivery_attempt: 2 },
    ]);
  });

  it('posts the recorded decision after a crash between deciding and posting, never re-deciding', async () => {
    const payment = await newPayment();
    const { body, eventId } = eventFor(payment);
    await startDelivery(pool, { eventId, natsMsgId: 'crashed', deliveryAttempt: 1 });
    await withTransaction(pool, (tx) => book(tx, payment.id)); // Tx 1
    await withTransaction(pool, (tx) => startReview(tx, payment.id)); // Tx 2
    await withTransaction(pool, (tx) => decide(tx, payment.id, APPROVE)); // Tx 3, then the crash

    // This time the dice say reject: the recorded approval must stand.
    expect(await handleDelivery(deps({ decide: () => REJECT }), message(body, 'crashed', 2))).toBe(
      'applied',
    );
    expect(await statusOf(pool, payment.id)).toBe('credited');
    expect(await historyOf(payment.id)).not.toContain('rejected');
    expect(await movementSteps(payment.id)).toEqual(['book', 'credit']);
  });

  it('terminates an event that is not valid JSON, logging the delivery without an event_id', async () => {
    const msg = message('{not json', 'garbage');
    expect(await handleDelivery(deps(), msg)).toBe('invalid');
    expect(msg.calls).toEqual(['term']);
    const result = await pool.query<{ outcome: string; event_id: string | null }>(
      "SELECT outcome, event_id FROM event_deliveries WHERE nats_msg_id = 'garbage' ORDER BY id DESC LIMIT 1",
    );
    expect(result.rows[0]).toEqual({ outcome: 'invalid', event_id: null });
  });

  it('terminates an event that fails validation, keeping its event_id in the log', async () => {
    const payment = await newPayment();
    const { body, eventId } = eventFor(payment);
    const broken = JSON.stringify({ ...JSON.parse(body), amount_minor: 1250 });
    const msg = message(broken);
    expect(await handleDelivery(deps(), msg)).toBe('invalid');
    expect(msg.calls).toEqual(['term']);
    expect(await deliveriesOf(eventId)).toEqual([
      { outcome: 'invalid', nats_msg_id: 'msg-1', delivery_attempt: 1 },
    ]);
    expect(await statusOf(pool, payment.id)).toBe('created');
  });

  it('terminates an event for a payment that does not exist', async () => {
    const payment = await newPayment();
    const ghost = { ...payment, id: newUuid('pay') };
    const { body, eventId } = eventFor(ghost);
    const msg = message(body);
    expect(await handleDelivery(deps(), msg)).toBe('invalid');
    expect(msg.calls).toEqual(['term']);
    expect(await deliveriesOf(eventId)).toEqual([
      { outcome: 'invalid', nats_msg_id: 'msg-1', delivery_attempt: 1 },
    ]);
  });

  it('naks after an unexpected failure, and the redelivery completes from where it stopped', async () => {
    const payment = await newPayment();
    const { body, eventId } = eventFor(payment);
    const failing = deps({
      decide: () => {
        throw new Error('compliance service unavailable');
      },
    });
    const first = message(body, 'm', 1);
    expect(await handleDelivery(failing, first)).toBe('failed');
    expect(first.calls).toEqual(['nak']);
    // Tx 1 and 2 committed before the failure.
    expect(await statusOf(pool, payment.id)).toBe('under_review');

    expect(await handleDelivery(deps(), message(body, 'm', 2))).toBe('applied');
    expect(await statusOf(pool, payment.id)).toBe('credited');
    expect(await movementSteps(payment.id)).toEqual(['book', 'credit']);
    expect((await deliveriesOf(eventId)).map((d) => d.outcome)).toEqual(['failed', 'applied']);
  });

  it('naks without throwing when the delivery cannot even be recorded', async () => {
    const unreachable = createPool({
      databaseUrl: 'postgres://ledger:ledger@localhost:1/ledger_test',
      logger: createLogger({ service: 'test', level: 'silent' }),
    });
    try {
      const msg = message('{}');
      expect(await handleDelivery(deps({ pool: unreachable }), msg)).toBe('failed');
      expect(msg.calls).toEqual(['nak']);
    } finally {
      await unreachable.end();
    }
  });

  it('signals working() during the review so the message is not redelivered meanwhile', async () => {
    const payment = await newPayment();
    const msg = message(eventFor(payment).body);
    await handleDelivery(deps({ reviewDelayMs: 50, workingIntervalMs: 20 }), msg);
    // 50 ms in stretches of at most 20 ms: three signals, then the ack.
    expect(msg.calls).toEqual(['working', 'working', 'working', 'ack']);
  });
});

describe('handleDelivery: review delay and poison messages', () => {
  it('skips the review delay for a duplicate and for an unknown payment', async () => {
    const slow = deps({ reviewDelayMs: 300, workingIntervalMs: 100 });
    const payment = await newPayment();
    const { body } = eventFor(payment);
    expect(await handleDelivery(deps(), message(body, 'first'))).toBe('applied');

    const duplicate = message(body, 'second');
    const started = Date.now();
    expect(await handleDelivery(slow, duplicate)).toBe('duplicate');
    expect(duplicate.calls).toEqual(['ack']); // no working(): it didn't wait
    expect(Date.now() - started).toBeLessThan(250);

    const ghost = message(eventFor({ ...payment, id: newUuid('pay') }).body);
    expect(await handleDelivery(slow, ghost)).toBe('invalid');
    expect(ghost.calls).toEqual(['term']);
  });

  it('terminates instead of retrying when an invariant is violated, logging it as fatal', async () => {
    const payment = await newPayment();
    // The ledger already holds the booking but the payment is still created: book() will
    // throw LedgerInconsistencyError, which no retry can fix.
    await withTransaction(pool, (tx) =>
      move(tx, {
        accountId: payment.accountId,
        from: 'external',
        to: 'pending_in',
        amountMinor: payment.amountMinor,
        referenceType: 'payment',
        referenceId: payment.id,
        referenceStep: 'book',
      }),
    );
    const { body, eventId } = eventFor(payment);
    const { logger, lines } = capturingLogger();
    const msg = message(body);

    expect(await handleDelivery(deps({ logger }), msg)).toBe('failed');
    expect(msg.calls).toEqual(['term']);
    expect(await statusOf(pool, payment.id)).toBe('created');
    expect((await deliveriesOf(eventId)).map((d) => d.outcome)).toEqual(['failed']);
    expect(lines().filter((l) => l.level === 'fatal')).toEqual([
      expect.objectContaining({ alert: true, msg: 'invariant violated; terminating the message' }),
    ]);
  });
});

describe('handleDelivery: overdraft', () => {
  it('terminates on an OverdraftError rather than retrying it', async () => {
    const payment = await newPayment();
    const { body } = eventFor(payment);
    // Get the payment booked and approved, then corrupt the balance so crediting it would
    // overdraw pending_in: only a bug could get here, so it must not be retried.
    await withTransaction(pool, (tx) => book(tx, payment.id));
    await withTransaction(pool, (tx) => startReview(tx, payment.id));
    await withTransaction(pool, (tx) => decide(tx, payment.id, APPROVE));
    await pool.query('UPDATE account_balances SET pending_in = 0 WHERE account_id = $1', [
      payment.accountId,
    ]);
    const { logger, lines } = capturingLogger();
    const msg = message(body);

    expect(await handleDelivery(deps({ logger }), msg)).toBe('failed');
    expect(msg.calls).toEqual(['term']);
    expect(await statusOf(pool, payment.id)).toBe('approved');
    expect(lines().filter((l) => l.level === 'fatal')).toEqual([
      expect.objectContaining({
        alert: true,
        err: expect.objectContaining({ type: 'OverdraftError' }),
      }),
    ]);
  });
});

describe('decideRandomly', () => {
  it('approves below the rate and rejects at or above it, with a reason', () => {
    expect(decideRandomly(0.8, () => 0.79)).toEqual({ outcome: 'approved' });
    expect(decideRandomly(0.8, () => 0.8)).toMatchObject({ outcome: 'rejected' });
    expect(decideRandomly(0.8, () => 0.8)).toHaveProperty('reason');
  });
});
