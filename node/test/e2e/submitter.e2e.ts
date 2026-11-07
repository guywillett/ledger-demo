import { randomUUID } from 'node:crypto';
import type { AddressInfo } from 'node:net';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { fromTypeId, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { getWithdrawal } from '../../src/withdrawals/withdrawals.ts';
import {
  apiBalances,
  apiWithdrawal,
  connectTestNats,
  createTestAccount,
  createTestApp,
  fundTestAccount,
  postWithdrawal,
  withdrawalBody,
  type TestAccount,
} from '../helpers.ts';
import { cleanStart, kill, startChild, waitFor, type Child } from './children.ts';

// Withdrawals end to end: provider-sim, the relay and the submitter as child processes, against
// the test database and the test NATS. Withdrawals are requested through the API app, which
// stores withdrawal.held in the outbox; the relay publishes it, the submitter sends it to
// provider-sim, and provider-sim reports the result back to the app's webhook (the app listens
// on a real port for that). provider-sim and the relay run for the whole file; submitters are
// started (and killed) per test. The withdrawal and the balance are read through the client API,
// what provider-sim saw through its test-only endpoint; what neither shows (the movements, the
// delivery log, transfer-update rows, provider_transfer_id) from the database.

// Not compose's provider-sim (host port 4000): a test instance of its own.
const SIM_PORT = '4100';
const PROVIDER_TIMEOUT_MS = '3000';

const testApp = await createTestApp();
const { app, pool } = testApp;
await app.listen({ host: '127.0.0.1', port: 0 });
const appPort = (app.server.address() as AddressInfo).port;
const webhookUrl = `http://127.0.0.1:${appPort}/provider/transfer-updates`;
const nc = await connectTestNats();

let sim: Child | undefined;
let relay: Child | undefined;
const submitters: Child[] = [];
const accountsUsed: Uuid<'acct'>[] = [];

async function startSubmitter(): Promise<Child> {
  const submitter = await startChild('src/submitter/main.ts', 'submitter started', {
    PROVIDER_URL: `http://127.0.0.1:${SIM_PORT}`,
    PROVIDER_TIMEOUT_MS,
  });
  submitters.push(submitter);
  return submitter;
}

type Requested = { id: Uuid<'wd'>; account: TestAccount };

// Requests a withdrawal of 400 through the API from a freshly funded account (1000).
async function requestWithdrawal(remittance: string): Promise<Requested> {
  const account = await createTestAccount(pool);
  accountsUsed.push(account.id);
  await fundTestAccount(pool, account, 1000n);
  const response = await postWithdrawal(
    app,
    account.typeId,
    withdrawalBody({ remittance_info: remittance }),
    randomUUID(),
  );
  expect(response.statusCode).toBe(201);
  return { id: fromTypeId('wd', String(response.json.id)), account };
}

// The withdrawal as the client API shows it.
async function shown(w: Requested): Promise<Record<string, unknown>> {
  return apiWithdrawal(app, w.account.typeId, toTypeId('wd', w.id));
}

async function statusOf(w: Requested): Promise<unknown> {
  return (await shown(w)).status;
}

async function stepsOf(id: Uuid<'wd'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    `SELECT reference_step FROM balance_movements
     WHERE reference_type = 'withdrawal' AND reference_id = $1 ORDER BY created_at`,
    [id],
  );
  return result.rows.map((r) => r.reference_step);
}

type SimSeen = {
  requests: number;
  transfer: { transfer_id: string; outcome: string } | null;
  refused: boolean;
  errored: boolean;
};

// What provider-sim has seen for this withdrawal (its TypeID is the reference), from its
// test-only endpoint (slice 4 decision 73); undefined if no request has carried it.
async function simSeen(id: Uuid<'wd'>): Promise<SimSeen | undefined> {
  const response = await fetch(
    `http://127.0.0.1:${SIM_PORT}/_test/transfers?reference=${toTypeId('wd', id)}`,
  );
  if (response.status === 404) return undefined;
  if (!response.ok) throw new Error(`provider-sim answered ${response.status}`);
  return (await response.json()) as SimSeen;
}

beforeAll(async () => {
  await cleanStart(pool, nc);
  sim = await startChild('src/provider-sim/main.ts', 'provider-sim started', {
    PORT: SIM_PORT,
    SIM_WEBHOOK_URL: webhookUrl,
    SIM_EXECUTION_DELAY_MS: '1000',
    // The slow answer comes well after the submitter's 3 s timeout.
    SIM_SLOW_DELAY_MS: '10000',
  });
  relay = await startChild('src/relay/main.ts', 'relay started', {});
});

afterEach(async () => {
  for (const submitter of submitters.splice(0)) await kill(submitter);
});

afterAll(async () => {
  for (const child of [relay, sim]) if (child !== undefined) await kill(child);
  await nc.drain();
  await testApp.close();
});

describe('withdrawals end to end', () => {
  it('executes a withdrawal requested through the API, and the submitter shuts down cleanly', async () => {
    const submitter = await startSubmitter();
    const w = await requestWithdrawal('Rent October');

    await waitFor(async () => (await statusOf(w)) === 'executed', 'executed');
    // One call, and the transfer it created is the one stored (provider_transfer_id isn't in the
    // client API, decision 69).
    const seen = await simSeen(w.id);
    expect(seen).toMatchObject({ requests: 1, transfer: { outcome: 'executed' } });
    expect((await getWithdrawal(pool, w.id))?.providerTransferId).toBe(seen?.transfer?.transfer_id);
    expect(await stepsOf(w.id)).toEqual(['hold', 'execute']);
    expect(await apiBalances(app, w.account.typeId)).toEqual({
      pendingIn: 0n,
      available: 600n,
      held: 0n,
    });

    submitter.process.kill('SIGTERM');
    expect(await submitter.exited).toBe(0);
  });

  it('fails a SIM FAIL withdrawal, releasing its funds', async () => {
    await startSubmitter();
    const w = await requestWithdrawal('SIM FAIL');

    await waitFor(async () => (await statusOf(w)) === 'failed', 'failed');
    expect((await shown(w)).failure_reason).toBe('rejected by the beneficiary bank');
    expect(await stepsOf(w.id)).toEqual(['hold', 'release']);
    expect(await apiBalances(app, w.account.typeId)).toEqual({
      pendingIn: 0n,
      available: 1000n,
      held: 0n,
    });
  });

  it('fails a withdrawal the provider refuses at submission (SIM REFUSE), releasing its funds', async () => {
    await startSubmitter();
    const w = await requestWithdrawal('SIM REFUSE');

    // No transfer and no webhook: the submitter's own refusal outcome releases the funds.
    await waitFor(async () => (await statusOf(w)) === 'failed', 'failed');
    expect((await shown(w)).failure_reason).toBe('creditor account closed');
    expect(await simSeen(w.id)).toMatchObject({ requests: 1, refused: true, transfer: null });
    expect(await stepsOf(w.id)).toEqual(['hold', 'release']);
    expect(await apiBalances(app, w.account.typeId)).toEqual({
      pendingIn: 0n,
      available: 1000n,
      held: 0n,
    });
  });

  it('retries a 503 with the same key and executes it (SIM ERROR)', async () => {
    const submitter = await startSubmitter();
    const w = await requestWithdrawal('SIM ERROR');
    const { id } = w;

    // The first submission is answered 503 (unknown outcome, nak); the retry, about 1 s later,
    // is accepted, and the provider's result executes it.
    await waitFor(async () => (await statusOf(w)) === 'executed', 'executed');
    expect(await simSeen(id)).toMatchObject({
      requests: 2,
      errored: true,
      transfer: { outcome: 'executed' },
    });
    expect(await stepsOf(id)).toEqual(['hold', 'execute']);
    // The submitter's side of it: its own log (which outcome it recorded is internal).
    expect(
      submitter
        .lines()
        .some(
          (l) =>
            l.msg === 'provider outcome unknown; will retry' &&
            l.withdrawal_id === toTypeId('wd', id),
        ),
    ).toBe(true);
  });

  it('applies a result that arrives before the submission is answered (SIM EARLY)', async () => {
    await startSubmitter();
    const w = await requestWithdrawal('SIM EARLY');
    const { id } = w;

    await waitFor(async () => (await statusOf(w)) === 'executed', 'executed');
    // The submitter's late markSubmitted skipped: its delivery is recorded as a duplicate.
    await waitFor(async () => {
      const deliveries = await pool.query<{ outcome: string }>(
        `SELECT d.outcome FROM event_deliveries d JOIN outbox o ON o.id = d.event_id
         WHERE d.consumer = 'submitter' AND o.entity_id = $1`,
        [id],
      );
      return deliveries.rows.some((d) => d.outcome === 'duplicate');
    }, "the submitter's duplicate outcome");
    expect(await stepsOf(id)).toEqual(['hold', 'execute']);
  });

  it('settles once when the result is delivered twice (SIM DUPLICATE)', async () => {
    await startSubmitter();
    const w = await requestWithdrawal('SIM DUPLICATE');
    const { id } = w;

    await waitFor(async () => {
      const result = await pool.query<{ notification_count: number }>(
        'SELECT notification_count FROM transfer_updates WHERE withdrawal_id = $1',
        [id],
      );
      return result.rows[0]?.notification_count === 2;
    }, 'the second delivery');
    expect(await statusOf(w)).toBe('executed');
    expect(await stepsOf(id)).toEqual(['hold', 'execute']);
  });

  it('settles once after the submitter is killed mid-call and restarted: one transfer at the provider', async () => {
    // SIM SLOW: the provider creates the transfer, then takes 10 s to answer.
    const first = await startSubmitter();
    const w = await requestWithdrawal('SIM SLOW');
    const { id } = w;

    // The call is in flight: the provider has created the transfer and is still answering.
    await waitFor(
      async () => ((await simSeen(id))?.transfer ?? null) !== null,
      'the transfer at the provider',
    );
    await kill(first); // no answer recorded, no ack, no nak

    // The provider's result (scheduled when it created the transfer) arrives while the submitter
    // is down, and executes the still-held withdrawal (decision 27). The restarted submitter gets
    // the redelivery after ack_wait (30 s), finds it settled, and makes no second call.
    await startSubmitter();
    await waitFor(async () => (await statusOf(w)) === 'executed', 'executed');
    await waitFor(
      async () => {
        const deliveries = await pool.query<{ n: number }>(
          `SELECT count(*)::int AS n FROM event_deliveries d JOIN outbox o ON o.id = d.event_id
         WHERE d.consumer = 'submitter' AND o.entity_id = $1 AND d.outcome = 'duplicate'`,
          [id],
        );
        return (deliveries.rows[0]?.n ?? 0) > 0;
      },
      'the redelivery',
      60_000,
    );
    // One call to the provider: the killed one. A second would count as a request.
    expect((await simSeen(id))?.requests).toBe(1);
    expect(await stepsOf(id)).toEqual(['hold', 'execute']);
  }, 90_000);

  it('leaves the books consistent for every account it used', async () => {
    const report = await reconcile(pool, { accountIds: accountsUsed, stuckAfterMs: 0 });
    expect(report.differences).toEqual([]);
    expect(report.withdrawalMismatches).toEqual([]);
    expect(report.stuckWithdrawals).toEqual([]);
    expect(report.transferUpdateProblems).toEqual([]);
    expect(report.consistent).toBe(true);
  });
});
