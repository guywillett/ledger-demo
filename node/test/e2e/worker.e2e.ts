import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import type { Uuid } from '../../src/shared/ids.ts';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { CONSUMERS, STREAMS } from '../../src/shared/nats.ts';
import {
  balancesOf,
  connectTestNats,
  createTestAccount,
  createTestApp,
  notificationBody,
  depositByNetworkId,
  postIncomingPayment,
  statusOf,
  type TestAccount,
} from '../helpers.ts';
import { cleanStart, kill, startChild, waitFor, type Child } from './children.ts';

// The real relay and worker as child processes (node src/relay/main.ts, node src/worker/main.ts),
// against the test database and the test NATS, as in production: deposits are posted through
// the API app, which stores deposit.created in the outbox; the relay publishes it, and the
// worker consumes it through the durable `worker` consumer. One relay runs for the whole file;
// workers are started (and killed) per test.

const testApp = await createTestApp();
const { app, pool } = testApp;
const nc = await connectTestNats();

const workers: Child[] = [];
let relay: Child | undefined;

function startWorker(env: Record<string, string>): Promise<Child> {
  return startChild('src/worker/main.ts', 'worker started', { APPROVE_RATE: '1', ...env }).then(
    (worker) => {
      workers.push(worker);
      return worker;
    },
  );
}

// Every account the tests post to, for the final reconciliation.
const accountsUsed: Uuid<'acct'>[] = [];

async function post(account: TestAccount): Promise<Uuid<'dep'>> {
  if (!accountsUsed.includes(account.id)) accountsUsed.push(account.id);
  const body = notificationBody({ creditor_iban: account.iban });
  expect((await postIncomingPayment(app, body)).statusCode).toBe(204);
  // The webhook answers 204 with no body: the deposit is read from the database.
  const deposit = await depositByNetworkId(pool, body.network_transaction_id);
  if (deposit === undefined) throw new Error('deposit not stored');
  return deposit.id;
}

async function movementSteps(depositId: Uuid<'dep'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    `SELECT reference_step FROM balance_movements
     WHERE reference_type = 'deposit' AND reference_id = $1 ORDER BY created_at`,
    [depositId],
  );
  return result.rows.map((r) => r.reference_step);
}

// Deliveries of this deposit's events: the delivery log has the event ID, and the outbox row
// with that ID says which deposit the event is about.
async function deliveriesFor(
  depositId: Uuid<'dep'>,
): Promise<{ outcome: string; delivery_attempt: number }[]> {
  const result = await pool.query<{ outcome: string; delivery_attempt: number }>(
    `SELECT d.outcome, d.delivery_attempt
     FROM event_deliveries d JOIN outbox o ON o.id = d.event_id
     WHERE o.entity_type = 'deposit' AND o.entity_id = $1
     ORDER BY d.id`,
    [depositId],
  );
  return result.rows;
}

async function consumerDeliveries(): Promise<number> {
  const jsm = await jetstreamManager(nc);
  const info = await jsm.consumers.info(STREAMS.deposits, CONSUMERS.worker);
  return info.delivered.consumer_seq;
}

beforeAll(async () => {
  await cleanStart(pool, nc);
  relay = await startChild('src/relay/main.ts', 'relay started', {});
});

afterEach(async () => {
  for (const worker of workers.splice(0)) await kill(worker);
});

afterAll(async () => {
  if (relay !== undefined) await kill(relay);
  await nc.drain();
  await testApp.close();
});

describe('worker process', () => {
  it('processes a posted deposit end to end, and the worker shuts down cleanly on SIGTERM', async () => {
    const worker = await startWorker({ REVIEW_DELAY_MS: '300' });
    const account = await createTestAccount(pool);
    const depositId = await post(account);

    await waitFor(async () => (await statusOf(pool, depositId)) === 'credited', 'credited');
    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 1250n,
      held: 0n,
    });
    expect(await movementSteps(depositId)).toEqual(['book', 'credit']);
    expect(await deliveriesFor(depositId)).toEqual([{ outcome: 'applied', delivery_attempt: 1 }]);

    worker.process.kill('SIGTERM');
    expect(await worker.exited).toBe(0);
    expect(worker.lines().some((l) => l.msg === 'shutting down')).toBe(true);
  });

  it('completes a deposit once after the worker is killed mid-review and restarted', async () => {
    // A long review, so the kill lands inside it.
    const first = await startWorker({ REVIEW_DELAY_MS: '5000' });
    const account = await createTestAccount(pool);
    const depositId = await post(account);

    await waitFor(async () => (await statusOf(pool, depositId)) === 'under_review', 'review');
    first.process.kill('SIGKILL'); // no chance to ack, nak or record an outcome
    await first.exited;
    expect(await statusOf(pool, depositId)).toBe('under_review');
    expect(await deliveriesFor(depositId)).toEqual([
      { outcome: 'in_progress', delivery_attempt: 1 },
    ]);

    // JetStream redelivers once ack_wait (10 s) has passed since the last working() signal.
    await startWorker({ REVIEW_DELAY_MS: '5000' });
    await waitFor(async () => (await statusOf(pool, depositId)) === 'credited', 'credited');

    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 1250n,
      held: 0n,
    });
    expect(await movementSteps(depositId)).toEqual(['book', 'credit']);
    expect(await deliveriesFor(depositId)).toEqual([
      { outcome: 'in_progress', delivery_attempt: 1 },
      { outcome: 'applied', delivery_attempt: 2 },
    ]);
  });

  it('shares the consumer between two workers, each message going to one of them', async () => {
    const env = { REVIEW_DELAY_MS: '300', WORKER_CONCURRENCY: '2' };
    const [a, b] = await Promise.all([startWorker(env), startWorker(env)]);
    const account = await createTestAccount(pool);
    const depositIds: Uuid<'dep'>[] = [];
    for (let i = 0; i < 6; i++) depositIds.push(await post(account));

    await waitFor(async () => {
      const statuses = await Promise.all(depositIds.map((id) => statusOf(pool, id)));
      return statuses.every((s) => s === 'credited');
    }, 'all credited');

    expect(await balancesOf(pool, account.id)).toEqual({
      pendingIn: 0n,
      available: 6n * 1250n,
      held: 0n,
    });
    for (const id of depositIds) {
      expect(await movementSteps(id)).toEqual(['book', 'credit']);
      expect(await deliveriesFor(id)).toEqual([{ outcome: 'applied', delivery_attempt: 1 }]);
    }
    const processed = (w: Child | undefined): number =>
      w?.lines().filter((l) => l.msg === 'deposit processed').length ?? 0;
    expect(processed(a) + processed(b)).toBe(6);
    expect(processed(a)).toBeGreaterThan(0);
    expect(processed(b)).toBeGreaterThan(0);
  });

  it('holds only messages it is working on, so a long review never causes a redelivery', async () => {
    // Three slots and a review longer than ack_wait (10 s), with more deposits than slots.
    // The extra deposits must wait on the server, not in a client buffer where their ack_wait
    // would run out unsignalled. (A prefetching consume() with max_messages 3 holds two more
    // messages beyond the three in flight; with one slot it prefetches none, which is why this
    // needs at least two.)
    await startWorker({ REVIEW_DELAY_MS: '12000', WORKER_CONCURRENCY: '3' });
    const account = await createTestAccount(pool);
    // JetStream counts every delivery, redeliveries included, in the consumer's sequence: the
    // most direct evidence of a redelivery (a redelivered copy may only be handled, and get its
    // delivery row, after the deposits look finished).
    const deliveriesBefore = await consumerDeliveries();
    const deposits: Uuid<'dep'>[] = [];
    for (let i = 0; i < 5; i++) deposits.push(await post(account));

    await waitFor(
      async () => {
        const statuses = await Promise.all(deposits.map((id) => statusOf(pool, id)));
        return statuses.every((s) => s === 'credited');
      },
      'all credited',
      45_000,
    );
    expect((await consumerDeliveries()) - deliveriesBefore).toBe(5);
    for (const id of deposits) {
      expect(await deliveriesFor(id)).toEqual([{ outcome: 'applied', delivery_attempt: 1 }]);
    }
  });

  it('the relay published each event once and shuts down cleanly on SIGTERM', async () => {
    const published = relay?.lines().filter((l) => l.msg === 'event published') ?? [];
    expect(published.length).toBeGreaterThan(0);
    expect(published.some((l) => l.stream_duplicate === true)).toBe(false);

    relay?.process.kill('SIGTERM');
    expect(await relay?.exited).toBe(0);
    expect(relay?.lines().some((l) => l.msg === 'shutting down')).toBe(true);
  });

  // After everything above (a kill -9 mid-review, two workers sharing the consumer, reviews
  // longer than ack_wait), the books must still agree: the "Done when" reconciliation check.
  it('leaves the books consistent for every account it used', async () => {
    const report = await reconcile(pool, { accountIds: accountsUsed, stuckAfterMs: 0 });
    expect(report.accountsChecked).toBe(accountsUsed.length);
    expect(report.differences).toEqual([]);
    expect(report.depositMismatches).toEqual([]);
    // Every deposit finished: none left in a non-final status, and every event published.
    expect(report.stuckDeposits).toEqual([]);
    expect(report.outbox.stuckUnpublished).toBe(0);
    expect(report.consistent).toBe(true);
  });
});
