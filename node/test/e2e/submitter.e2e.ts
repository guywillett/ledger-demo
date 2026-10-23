import { randomUUID } from 'node:crypto';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { fromTypeId, toTypeId, type Uuid } from '../../src/shared/ids.ts';
import { getWithdrawal } from '../../src/withdrawals/withdrawals.ts';
import {
  connectTestNats,
  createTestAccount,
  createTestApp,
  fundTestAccount,
  postWithdrawal,
  withdrawalBody,
} from '../helpers.ts';
import { cleanStart, kill, startChild, waitFor, type Child } from './children.ts';

// provider-sim, the relay and the submitter as child processes, against the test database and
// the test NATS: withdrawals are requested through the API app, which stores withdrawal.held in
// the outbox; the relay publishes it, and the submitter sends it to provider-sim. provider-sim
// and the relay run for the whole file; submitters are started (and killed) per test.

// Not compose's provider-sim (host port 4000): a test instance of its own.
const SIM_PORT = '4100';
const PROVIDER_TIMEOUT_MS = '3000';

const testApp = await createTestApp();
const { app, pool } = testApp;
const nc = await connectTestNats();

let sim: Child | undefined;
let relay: Child | undefined;
const submitters: Child[] = [];

async function startSubmitter(): Promise<Child> {
  const submitter = await startChild('src/submitter/main.ts', 'submitter started', {
    PROVIDER_URL: `http://127.0.0.1:${SIM_PORT}`,
    PROVIDER_TIMEOUT_MS,
  });
  submitters.push(submitter);
  return submitter;
}

// Requests a withdrawal through the API from a freshly funded account.
async function requestWithdrawal(remittance: string): Promise<Uuid<'wd'>> {
  const account = await createTestAccount(pool);
  await fundTestAccount(pool, account, 1000n);
  const response = await postWithdrawal(
    app,
    account.typeId,
    withdrawalBody({ remittance_info: remittance }),
    randomUUID(),
  );
  expect(response.statusCode).toBe(201);
  return fromTypeId('wd', String(response.json.id));
}

async function statusOf(id: Uuid<'wd'>): Promise<string | undefined> {
  return (await getWithdrawal(pool, id))?.status;
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

describe('submitter process', () => {
  it('submits a withdrawal requested through the API, and shuts down cleanly on SIGTERM', async () => {
    const submitter = await startSubmitter();
    const id = await requestWithdrawal('Rent October');

    await waitFor(async () => (await statusOf(id)) === 'submitted', 'submitted');
    expect((await getWithdrawal(pool, id))?.providerTransferId).toMatch(/^tr_/);
    expect(transfersCreated(id)).toBe(1);

    submitter.process.kill('SIGTERM');
    expect(await submitter.exited).toBe(0);
  });

  it('submits once after the submitter is killed mid-call and restarted: one transfer at the provider', async () => {
    // SIM SLOW: the provider creates the transfer, then takes 10 s to answer.
    const first = await startSubmitter();
    const id = await requestWithdrawal('SIM SLOW');

    // The call is in flight: the provider has created the transfer and is still answering.
    await waitFor(() => transfersCreated(id) === 1, 'the transfer at the provider');
    await kill(first); // no answer recorded, no ack, no nak
    expect(await statusOf(id)).toBe('held');

    // JetStream redelivers once ack_wait (30 s) has passed. The new call carries the same key,
    // and the provider answers with the transfer it already has.
    await startSubmitter();
    await waitFor(async () => (await statusOf(id)) === 'submitted', 'submitted', 60_000);
    expect(transfersCreated(id)).toBe(1);
  }, 90_000);
});
