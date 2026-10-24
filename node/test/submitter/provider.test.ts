import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { setTimeout as sleep } from 'node:timers/promises';
import { afterAll, describe, expect, it } from 'vitest';
import { buildSimApp } from '../../src/provider-sim/app.ts';
import { parseIban, type Iban } from '../../src/shared/iban.ts';
import { newUuid, toTypeId } from '../../src/shared/ids.ts';
import { providerClient } from '../../src/submitter/provider.ts';
import type { Withdrawal } from '../../src/withdrawals/withdrawals.ts';
import { capturingLogger, FOREIGN_IBAN } from '../helpers.ts';

// The provider client over real HTTP, against provider-sim listening on an ephemeral port: every
// answer maps to its outcome, and an unknown outcome resolves on the next call with the same key.

const SLOW_DELAY_MS = 600;

const { logger, lines } = capturingLogger();
const sim = buildSimApp({
  logger,
  now: () => new Date(),
  random: () => 0.5,
  // No webhook: results are only logged.
  deliver: undefined,
  sleep: (ms, signal) => sleep(ms, undefined, { signal }).catch(() => undefined),
  executionDelayMs: 50,
  slowDelayMs: SLOW_DELAY_MS,
  webhookMaxAttempts: 1,
  rates: { refuse: 0, error: 0, slow: 0, fail: 0, duplicate: 0, early: 0 },
});
await sim.app.listen({ host: '127.0.0.1', port: 0 });
const baseUrl = `http://127.0.0.1:${(sim.app.server.address() as AddressInfo).port}`;

afterAll(async () => {
  sim.stop();
  await sim.app.close();
});

const client = providerClient({ baseUrl, timeoutMs: 2000 });

function iban(value: string): Iban {
  const result = parseIban(value);
  if (!result.ok) throw new Error(result.reason);
  return result.iban;
}

// A withdrawal as stored (not in a database: the client only reads it).
function withdrawal(overrides: Partial<Withdrawal> = {}): Withdrawal {
  return {
    id: newUuid('wd'),
    accountId: newUuid('acct'),
    idempotencyKey: 'client-key',
    amountMinor: 2500n,
    currency: 'EUR',
    creditorIban: iban(FOREIGN_IBAN),
    creditorName: 'Jane Doe',
    remittanceInfo: 'Rent October',
    status: 'held',
    failureReason: null,
    providerTransferId: null,
    createdAt: new Date(),
    updatedAt: new Date(),
    ...overrides,
  };
}

function transfersCreated(reference: string): number {
  return lines().filter((l) => l.msg === 'transfer created' && l.reference === reference).length;
}

describe('providerClient.submitTransfer', () => {
  it('maps 201, then 200 for a repeat of the same key, to accepted with the same transfer ID', async () => {
    const w = withdrawal();
    const first = await client.submitTransfer(w);
    const again = await client.submitTransfer(w);
    expect(first).toEqual({ kind: 'accepted', transferId: expect.stringMatching(/^tr_/) });
    expect(again).toEqual(first);
  });

  it('maps a 422 to refused, with the reason', async () => {
    expect(await client.submitTransfer(withdrawal({ remittanceInfo: 'SIM REFUSE' }))).toEqual({
      kind: 'refused',
      reason: 'creditor account closed',
    });
  });

  it('maps a 400 to invalid, with only a summary of the body: field paths and size', async () => {
    // Not something a stored withdrawal can hold (the database refuses it): a bug.
    const outcome = await client.submitTransfer(withdrawal({ creditorName: 'Zoë Müller' }));
    expect(outcome).toEqual({
      kind: 'invalid',
      fields: ['creditor_name'],
      bodyBytes: expect.any(Number) as number,
    });
    // Nothing from the request (or anything else in the body) is carried along.
    expect(JSON.stringify(outcome)).not.toContain('Zo');
  });

  it('keeps request values a provider echoes in a 400 out of the outcome', async () => {
    // A provider (not provider-sim) that repeats the IBAN in its error message, and in a path.
    const echoing = createServer((_req, res) => {
      res.writeHead(400, { 'content-type': 'application/json' });
      res.end(
        JSON.stringify({
          errors: [
            { path: 'creditor_iban', message: `${FOREIGN_IBAN} is not reachable` },
            { path: `iban ${FOREIGN_IBAN}`, message: 'unknown' },
          ],
        }),
      );
    });
    await new Promise<void>((resolve) => echoing.listen(0, '127.0.0.1', resolve));
    try {
      const port = (echoing.address() as AddressInfo).port;
      const outcome = await providerClient({
        baseUrl: `http://127.0.0.1:${port}`,
        timeoutMs: 2000,
      }).submitTransfer(withdrawal());
      expect(outcome).toMatchObject({ kind: 'invalid', fields: ['creditor_iban'] });
      expect(JSON.stringify(outcome)).not.toContain(FOREIGN_IBAN);
    } finally {
      await new Promise((resolve) => echoing.close(resolve));
    }
  });

  it('maps a 409 (our key with another body) to key_conflict', async () => {
    const w = withdrawal();
    await client.submitTransfer(w);
    expect(await client.submitTransfer({ ...w, amountMinor: 2501n })).toEqual({
      kind: 'key_conflict',
    });
  });

  it('maps a 503 to unknown; the retry with the same key is accepted', async () => {
    const w = withdrawal({ remittanceInfo: 'SIM ERROR' });
    expect(await client.submitTransfer(w)).toEqual({
      kind: 'unknown',
      cause: 'provider answered 503',
    });
    expect((await client.submitTransfer(w)).kind).toBe('accepted');
  });

  it('maps a connection failure to unknown', async () => {
    const nowhere = providerClient({ baseUrl: 'http://127.0.0.1:1', timeoutMs: 1000 });
    expect((await nowhere.submitTransfer(withdrawal())).kind).toBe('unknown');
  });

  // The unknown-outcome proof: the provider created the transfer but answered after our timeout.
  // Retrying with the same key finds that transfer instead of creating a second one.
  it('maps a timeout to unknown, and the next call with the same key gets the same, single transfer', async () => {
    const impatient = providerClient({ baseUrl, timeoutMs: 100 });
    const w = withdrawal({ remittanceInfo: 'SIM SLOW' });

    const timedOut = await impatient.submitTransfer(w);
    expect(timedOut.kind).toBe('unknown');
    // The provider did create it, though we never heard.
    expect(transfersCreated(toTypeId('wd', w.id))).toBe(1);

    const retried = await impatient.submitTransfer(w);
    expect(retried).toEqual({ kind: 'accepted', transferId: expect.stringMatching(/^tr_/) });
    // Let the slow first answer arrive; still one transfer for this withdrawal.
    await sleep(SLOW_DELAY_MS);
    expect(transfersCreated(toTypeId('wd', w.id))).toBe(1);
  });
});
