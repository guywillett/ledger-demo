import { randomUUID } from 'node:crypto';
import type { AddressInfo } from 'node:net';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { reconcile } from '../../src/reconciliation/reconcile.ts';
import { fromTypeId, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { getWithdrawal } from '../../src/withdrawals/withdrawals.ts';
import {
  balancesOf,
  connectTestNats,
  createTestAccount,
  createTestApp,
  fundTestAccount,
  postWithdrawal,
  withdrawalBody,
} from '../helpers.ts';
import { cleanStart, kill, startChild, waitFor, type Child } from './children.ts';

// Withdrawals end to end: provider-sim, the relay and the submitter as child processes, against
// the test database and the test NATS. Withdrawals are requested through the API app, which
// stores withdrawal.held in the outbox; the relay publishes it, the submitter sends it to
// provider-sim, and provider-sim reports the result back to the app's webhook (the app listens
// on a real port for that). provider-sim and the relay run for the whole file; submitters are
// started (and killed) per test.

// Not compose's provider-sim (host port 4000): a test instance of its own.
const SIM_PORT = '4100';
const PROVIDER_TIMEOUT_MS = '3000';

const testApp = await createTestApp();
const { app, pool } = testApp;
await app.listen({ host: '127.0.0.1', port: 0 });
const webhookUrl = `http://127.0.0.1:${(app.server.address() as AddressInfo).port}/provider/transfer-updates`;
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

// Requests a withdrawal of 400 through the API from a freshly funded account (1000).
async function requestWithdrawal(
  remittance: string,
): Promise<{ id: Uuid<'wd'>; accountId: Uuid<'acct'> }> {
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
  return { id: fromTypeId('wd', String(response.json.id)), accountId: account.id };
}

async function statusOf(id: Uuid<'wd'>): Promise<string | undefined> {
  return (await getWithdrawal(pool, id))?.status;
}

async function stepsOf(id: Uuid<'wd'>): Promise<string[]> {
  const result = await pool.query<{ reference_step: string }>(
    `SELECT reference_step FROM balance_movements
     WHERE reference_type = 'withdrawal' AND reference_id = $1 ORDER BY created_at`,
    [id],
  );
  return result.rows.map((r) => r.reference_step);
}

// provider-sim's "transfer created" lines for this withdrawal (its TypeID is the reference).
function transfersCreated(id: Uuid<'wd'>): number {
  const reference = toTypeId('wd', id);
  return (
    sim?.lines().filter((l) => l.msg === 'transfer created' && l.reference === reference).length ??
    0
  );
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
    const { id, accountId } = await requestWithdrawal('Rent October');

    await waitFor(async () => (await statusOf(id)) === 'executed', 'executed');
    expect((await getWithdrawal(pool, id))?.providerTransferId).toMatch(/^tr_/);
    expect(transfersCreated(id)).toBe(1);
    expect(await stepsOf(id)).toEqual(['hold', 'execute']);
    expect(await balancesOf(pool, accountId)).toEqual({ pendingIn: 0n, available: 600n, held: 0n });

    submitter.process.kill('SIGTERM');
    expect(await submitter.exited).toBe(0);
  });

  it('fails a SIM FAIL withdrawal, releasing its funds', async () => {
    await startSubmitter();
    const { id, accountId } = await requestWithdrawal('SIM FAIL');

    await waitFor(async () => (await statusOf(id)) === 'failed', 'failed');
    expect((await getWithdrawal(pool, id))?.failureReason).toBe('rejected by the beneficiary bank');
    expect(await stepsOf(id)).toEqual(['hold', 'release']);
    expect(await balancesOf(pool, accountId)).toEqual({
      pendingIn: 0n,
      available: 1000n,
      held: 0n,
    });
  });

  it('applies a result that arrives before the submission is answered (SIM EARLY)', async () => {
    await startSubmitter();
    const { id } = await requestWithdrawal('SIM EARLY');

    await waitFor(async () => (await statusOf(id)) === 'executed', 'executed');
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
    const { id } = await requestWithdrawal('SIM DUPLICATE');

    await waitFor(async () => {
      const result = await pool.query<{ notification_count: number }>(
        'SELECT notification_count FROM transfer_updates WHERE withdrawal_id = $1',
        [id],
      );
      return result.rows[0]?.notification_count === 2;
    }, 'the second delivery');
    expect(await statusOf(id)).toBe('executed');
    expect(await stepsOf(id)).toEqual(['hold', 'execute']);
  });

  it('settles once after the submitter is killed mid-call and restarted: one transfer at the provider', async () => {
    // SIM SLOW: the provider creates the transfer, then takes 10 s to answer.
    const first = await startSubmitter();
    const { id } = await requestWithdrawal('SIM SLOW');

    // The call is in flight: the provider has created the transfer and is still answering.
    await waitFor(() => transfersCreated(id) === 1, 'the transfer at the provider');
    await kill(first); // no answer recorded, no ack, no nak

    // The provider's result (scheduled when it created the transfer) arrives while the submitter
    // is down, and executes the still-held withdrawal (decision 27). The restarted submitter gets
    // the redelivery after ack_wait (30 s), finds it settled, and makes no second call.
    await startSubmitter();
    await waitFor(async () => (await statusOf(id)) === 'executed', 'executed');
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
    expect(transfersCreated(id)).toBe(1);
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
