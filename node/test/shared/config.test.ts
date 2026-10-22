import { describe, expect, it } from 'vitest';
import { loadApiConfig } from '../../src/api/config.ts';
import { loadRelayConfig } from '../../src/relay/config.ts';
import { loadSharedConfig } from '../../src/shared/config.ts';
import { loadWorkerConfig } from '../../src/worker/config.ts';

const DATABASE_URL = 'postgres://u:p@localhost:5432/db';
const NATS_URL = 'nats://localhost:4222';

describe('loadSharedConfig', () => {
  it('parses a valid environment and defaults LOG_LEVEL', () => {
    const config = loadSharedConfig({ DATABASE_URL });
    expect(config).toEqual({ databaseUrl: DATABASE_URL, logLevel: 'info' });
    expect(Object.isFrozen(config)).toBe(true);
  });

  it('reports every invalid variable at once', () => {
    expect(() => loadSharedConfig({ LOG_LEVEL: 'loud' })).toThrow(/DATABASE_URL[\s\S]*LOG_LEVEL/);
  });
});

describe('NATS_URL', () => {
  it('is not required by the API', () => {
    expect(loadApiConfig({ DATABASE_URL })).toMatchObject({ databaseUrl: DATABASE_URL });
  });

  it('is required by the worker and the relay', () => {
    expect(() => loadWorkerConfig({ DATABASE_URL })).toThrow(/NATS_URL/);
    expect(() => loadRelayConfig({ DATABASE_URL })).toThrow(/NATS_URL/);
    expect(loadWorkerConfig({ DATABASE_URL, NATS_URL }).natsUrl).toBe(NATS_URL);
  });
});

describe('loadRelayConfig', () => {
  it('defaults to batches of 10 under a 30 s lease', () => {
    expect(loadRelayConfig({ DATABASE_URL, NATS_URL })).toMatchObject({
      natsUrl: NATS_URL,
      batchSize: 10,
      leaseMs: 30_000,
    });
  });

  // Beyond 60 s, a crashed relay's re-publish could fall outside the stream's 120 s duplicate
  // window.
  it('refuses a lease longer than 60 s', () => {
    expect(() => loadRelayConfig({ DATABASE_URL, NATS_URL, RELAY_LEASE_MS: '60001' })).toThrow(
      /RELAY_LEASE_MS/,
    );
    expect(loadRelayConfig({ DATABASE_URL, NATS_URL, RELAY_LEASE_MS: '60000' }).leaseMs).toBe(
      60_000,
    );
  });

  // 20 × 2000 ms publish timeout = 40 s of worst-case publishing, + 5 s for database work.
  it('refuses a lease shorter than a batch of timed-out publishes plus the margin', () => {
    expect(() =>
      loadRelayConfig({ DATABASE_URL, NATS_URL, RELAY_BATCH_SIZE: '20', RELAY_LEASE_MS: '44999' }),
    ).toThrow(/RELAY_LEASE_MS must be at least RELAY_BATCH_SIZE/);
    expect(
      loadRelayConfig({ DATABASE_URL, NATS_URL, RELAY_BATCH_SIZE: '20', RELAY_LEASE_MS: '45000' })
        .leaseMs,
    ).toBe(45_000);
  });
});
