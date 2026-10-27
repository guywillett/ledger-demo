import { z } from 'zod';
import { loadSharedConfig, natsEnvSchema, parseEnv, type SharedConfig } from '../shared/config.ts';
import { PUBLISH_TIMEOUT_MS } from '../shared/nats.ts';

// On top of the publish timeouts, a batch spends time the lease must also cover: the mark UPDATE
// after each publish and any wait for a pool connection. Milliseconds when healthy; this allows
// for a slow database.
export const LEASE_MARGIN_MS = 5000;

// Defaults: a batch of 10 under a 30 s lease. The lease must outlast a batch's worst case, every
// publish timing out one after another: 10 × PUBLISH_TIMEOUT_MS (2 s) = 20 s, plus
// LEASE_MARGIN_MS for the database work in between = 25 s. The lease is also how long a crashed
// relay holds back the events it had claimed, so it stays short, and the batch size gives way
// instead. That costs little: a healthy publish is acknowledged in about a
// millisecond, and a full batch is followed by the next one at once.
//
// z.coerce: environment variables are always strings.
const relayEnvSchema = natsEnvSchema
  .extend({
    // How many events one claim takes.
    RELAY_BATCH_SIZE: z.coerce.number().int().min(1).max(1000).default(10),
    // How long a claim holds its events. At most 60 s: a crashed relay's events are published
    // again when its lease expires, and the stream drops that repeat only within its
    // duplicate_window (120 s, nats/streams/DEPOSITS.json). A longer lease would quietly lose
    // the "dropped by the stream" guarantee (consumers would still cope: they are idempotent on
    // state).
    RELAY_LEASE_MS: z.coerce.number().int().min(1000).max(60_000).default(30_000),
  })
  .refine(
    (env) => env.RELAY_LEASE_MS >= env.RELAY_BATCH_SIZE * PUBLISH_TIMEOUT_MS + LEASE_MARGIN_MS,
    {
      message:
        `RELAY_LEASE_MS must be at least RELAY_BATCH_SIZE × ${PUBLISH_TIMEOUT_MS} ms ` +
        `(the publish timeout) + ${LEASE_MARGIN_MS} ms (database work): a batch whose ` +
        'publishes all time out would otherwise outlast its lease, and another relay would ' +
        'claim and publish the same events while this one is still on them',
      path: ['RELAY_LEASE_MS'],
    },
  );

export type RelayConfig = SharedConfig & {
  readonly natsUrl: string;
  readonly batchSize: number;
  readonly leaseMs: number;
};

export function loadRelayConfig(env: NodeJS.ProcessEnv = process.env): RelayConfig {
  const shared = loadSharedConfig(env);
  const parsed = parseEnv(relayEnvSchema, env);
  return Object.freeze({
    ...shared,
    natsUrl: parsed.NATS_URL,
    batchSize: parsed.RELAY_BATCH_SIZE,
    leaseMs: parsed.RELAY_LEASE_MS,
  });
}
