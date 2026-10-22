import { spawn, type ChildProcess } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { jetstreamManager } from '@nats-io/jetstream';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import type { Uuid } from '../../src/shared/ids.ts';
import { fromTypeId } from '../../src/shared/ids.ts';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { CONSUMERS, STREAMS } from '../../src/shared/nats.ts';
import {
  balancesOf,
  createTestAccount,
  createTestApp,
  notificationBody,
  postPayment,
  statusOf,
  type TestAccount,
} from '../helpers.ts';

// The real worker as a child process (node src/worker/main.ts), against the test database and
// the test NATS: payments are posted through the API app, which publishes payment.created,
// and the worker consumes them through the durable `worker` consumer.

const testApp = await createTestApp();
const { app, pool, nc } = testApp;

type Worker = {
  readonly process: ChildProcess;
  readonly lines: () => Record<string, unknown>[];
  readonly exited: Promise<number | null>;
};
const workers: Worker[] = [];

function startWorker(env: Record<string, string>): Promise<Worker> {
  const child = spawn(process.execPath, ['src/worker/main.ts'], {
    // process.env already holds the test DATABASE_URL and NATS_URL (vitest config).
    env: { ...process.env, LOG_LEVEL: 'info', APPROVE_RATE: '1', ...env },
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  let output = '';
  child.stdout.on('data', (chunk: Buffer) => {
    output += chunk.toString();
  });
  const lines = (): Record<string, unknown>[] =>
    output
      .split('\n')
      .filter((line) => line.startsWith('{'))
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  const exited = new Promise<number | null>((resolve) => child.once('exit', resolve));
  const worker = { process: child, lines, exited };
  workers.push(worker);
  return waitFor(() => lines().some((l) => l.msg === 'worker started'), 'worker start').then(
    () => worker,
  );
}

async function waitFor(
  condition: () => boolean | Promise<boolean>,
  what: string,
  timeoutMs = 30_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(100);
  }
}

// Every account the tests post to, for the final reconciliation.
const accountsUsed: Uuid<'acct'>[] = [];

async function post(account: TestAccount): Promise<Uuid<'pay'>> {
  if (!accountsUsed.includes(account.id)) accountsUsed.push(account.id);
  const response = await postPayment(app, notificationBody({ creditor_iban: account.iban }));
  expect(response.statusCode).toBe(201);
  return fromTypeId('pay', String(response.json.id));
}

async function movementSteps(paymentId: Uuid<'pay'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    "SELECT reference_step FROM balance_movements WHERE reference_type = 'payment' AND reference_id = $1 ORDER BY created_at",
    [paymentId],
  );
  return result.rows.map((r) => r.reference_step);
}

// Deliveries of this payment's events (the event carries the payment ID; the delivery log
// has the event ID, so match through the published events).
async function deliveriesFor(
  paymentId: Uuid<'pay'>,
): Promise<{ outcome: string; delivery_attempt: number }[]> {
  const eventIds = testApp.published
    .filter((p) => p.event.payment_id !== undefined)
    .filter((p) => fromTypeId('pay', String(p.event.payment_id)) === paymentId)
    .map((p) => fromTypeId('evt', String(p.event.event_id)));
  const result = await pool.query<{ outcome: string; delivery_attempt: number }>(
    'SELECT outcome, delivery_attempt FROM event_deliveries WHERE event_id = ANY($1) ORDER BY id',
    [eventIds],
  );
  return result.rows;
}

// The name nats-test runs with (docker-compose.yml). Checked before purging, so a test NATS
// URL pointing at the development server by mistake can't empty its stream.
const TEST_NATS_SERVER_NAME = 'ledger-demo-test';

async function consumerDeliveries(): Promise<number> {
  const jsm = await jetstreamManager(nc);
  const info = await jsm.consumers.info(STREAMS.payments, CONSUMERS.worker);
  return info.delivered.consumer_seq;
}

beforeAll(async () => {
  // Start from an empty test stream: events published by unit tests would otherwise be the
  // worker's first backlog. Only the test NATS is touched.
  const serverName = nc.info?.server_name;
  if (serverName !== TEST_NATS_SERVER_NAME) {
    throw new Error(
      `Refusing to purge ${STREAMS.payments} on NATS server "${String(serverName)}": ` +
        `the e2e tests only run against ${TEST_NATS_SERVER_NAME} (nats-test).`,
    );
  }
  const jsm = await jetstreamManager(nc);
  await jsm.streams.purge(STREAMS.payments);
});

afterEach(async () => {
  for (const worker of workers.splice(0)) {
    if (worker.process.exitCode === null && worker.process.signalCode === null) {
      worker.process.kill('SIGKILL');
      await worker.exited;
    }
  }
});

afterAll(testApp.close);

describe('worker process', () => {
  it('processes a posted payment end to end, and shuts down cleanly on SIGTERM', async () => {
    const worker = await startWorker({ REVIEW_DELAY_MS: '300' });
    const account = await createTestAccount(pool);
    const paymentId = await post(account);

    await waitFor(async () => (await statusOf(pool, paymentId)) === 'credited', 'credited');
    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await movementSteps(paymentId)).toEqual(['book', 'credit']);
    expect(await deliveriesFor(paymentId)).toEqual([{ outcome: 'applied', delivery_attempt: 1 }]);

    worker.process.kill('SIGTERM');
    expect(await worker.exited).toBe(0);
    expect(worker.lines().some((l) => l.msg === 'shutting down')).toBe(true);
  });

  it('completes a payment once after the worker is killed mid-review and restarted', async () => {
    // A long review, so the kill lands inside it.
    const first = await startWorker({ REVIEW_DELAY_MS: '5000' });
    const account = await createTestAccount(pool);
    const paymentId = await post(account);

    await waitFor(async () => (await statusOf(pool, paymentId)) === 'under_review', 'review');
    first.process.kill('SIGKILL'); // no chance to ack, nak or record an outcome
    await first.exited;
    expect(await statusOf(pool, paymentId)).toBe('under_review');
    expect(await deliveriesFor(paymentId)).toEqual([
      { outcome: 'in_progress', delivery_attempt: 1 },
    ]);

    // JetStream redelivers once ack_wait (10 s) has passed since the last working() signal.
    await startWorker({ REVIEW_DELAY_MS: '5000' });
    await waitFor(async () => (await statusOf(pool, paymentId)) === 'credited', 'credited');

    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 0n, available: 1250n });
    expect(await movementSteps(paymentId)).toEqual(['book', 'credit']);
    expect(await deliveriesFor(paymentId)).toEqual([
      { outcome: 'in_progress', delivery_attempt: 1 },
      { outcome: 'applied', delivery_attempt: 2 },
    ]);
  });

  it('shares the consumer between two workers, each message going to one of them', async () => {
    const env = { REVIEW_DELAY_MS: '300', WORKER_CONCURRENCY: '2' };
    const [a, b] = await Promise.all([startWorker(env), startWorker(env)]);
    const account = await createTestAccount(pool);
    const paymentIds: Uuid<'pay'>[] = [];
    for (let i = 0; i < 6; i++) paymentIds.push(await post(account));

    await waitFor(async () => {
      const statuses = await Promise.all(paymentIds.map((id) => statusOf(pool, id)));
      return statuses.every((s) => s === 'credited');
    }, 'all credited');

    expect(await balancesOf(pool, account.id)).toEqual({ pendingIn: 0n, available: 6n * 1250n });
    for (const id of paymentIds) {
      expect(await movementSteps(id)).toEqual(['book', 'credit']);
      expect(await deliveriesFor(id)).toEqual([{ outcome: 'applied', delivery_attempt: 1 }]);
    }
    const processed = (w: Worker | undefined): number =>
      w?.lines().filter((l) => l.msg === 'payment processed').length ?? 0;
    expect(processed(a) + processed(b)).toBe(6);
    expect(processed(a)).toBeGreaterThan(0);
    expect(processed(b)).toBeGreaterThan(0);
  });

  it('holds only messages it is working on, so a long review never causes a redelivery', async () => {
    // Three slots and a review longer than ack_wait (10 s), with more payments than slots.
    // The extra payments must wait on the server, not in a client buffer where their ack_wait
    // would run out unsignalled. (A prefetching consume() with max_messages 3 holds two more
    // messages beyond the three in flight; with one slot it prefetches none, which is why this
    // needs at least two.)
    await startWorker({ REVIEW_DELAY_MS: '12000', WORKER_CONCURRENCY: '3' });
    const account = await createTestAccount(pool);
    // JetStream counts every delivery, redeliveries included, in the consumer's sequence: the
    // most direct evidence of a redelivery (a redelivered copy may only be handled, and get its
    // delivery row, after the payments look finished).
    const deliveriesBefore = await consumerDeliveries();
    const payments: Uuid<'pay'>[] = [];
    for (let i = 0; i < 5; i++) payments.push(await post(account));

    await waitFor(
      async () => {
        const statuses = await Promise.all(payments.map((id) => statusOf(pool, id)));
        return statuses.every((s) => s === 'credited');
      },
      'all credited',
      45_000,
    );
    expect((await consumerDeliveries()) - deliveriesBefore).toBe(5);
    for (const id of payments) {
      expect(await deliveriesFor(id)).toEqual([{ outcome: 'applied', delivery_attempt: 1 }]);
    }
  });

  // After everything above (a kill -9 mid-review, two workers sharing the consumer, reviews
  // longer than ack_wait), the books must still agree: the "Done when" reconciliation check.
  it('leaves the books consistent for every account it used', async () => {
    const report = await reconcile(pool, { accountIds: accountsUsed, stuckAfterMs: 0 });
    expect(report.accountsChecked).toBe(accountsUsed.length);
    expect(report.differences).toEqual([]);
    expect(report.mismatches).toEqual([]);
    // Every payment finished: none left in a non-final status.
    expect(report.stuck).toEqual([]);
    expect(report.consistent).toBe(true);
  });
});
