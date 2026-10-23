import { z } from 'zod';
import { loadSharedConfig, natsEnvSchema, parseEnv, type SharedConfig } from '../shared/config.ts';

// A provider call must end well inside the submitter consumer's ack_wait: at most this share of
// it. The ack_wait comes from NATS at startup (assertTimeoutFitsAckWait), not a copy here, so
// changing nats/consumers/WITHDRAWALS/submitter.json can't silently outdate the check.
const MAX_SHARE_OF_ACK_WAIT = 2 / 3;

// z.coerce: environment variables are always strings.
const submitterEnvSchema = natsEnvSchema.extend({
  // The provider's base URL (provider-sim in compose).
  PROVIDER_URL: z.url(),
  // A provider call that hasn't answered by then is abandoned: its outcome is unknown, and the
  // message is redelivered later to try again with the same key.
  // Also checked against the consumer's ack_wait at startup (assertTimeoutFitsAckWait).
  PROVIDER_TIMEOUT_MS: z.coerce.number().int().min(100).max(120_000).default(5000),
  // How many withdrawals one submitter instance handles at once.
  SUBMITTER_CONCURRENCY: z.coerce.number().int().min(1).max(1000).default(10),
});

export type SubmitterConfig = SharedConfig & {
  readonly natsUrl: string;
  readonly providerUrl: string;
  readonly providerTimeoutMs: number;
  readonly concurrency: number;
};

export function loadSubmitterConfig(env: NodeJS.ProcessEnv = process.env): SubmitterConfig {
  const shared = loadSharedConfig(env);
  const parsed = parseEnv(submitterEnvSchema, env);
  return Object.freeze({
    ...shared,
    natsUrl: parsed.NATS_URL,
    providerUrl: parsed.PROVIDER_URL,
    providerTimeoutMs: parsed.PROVIDER_TIMEOUT_MS,
    concurrency: parsed.SUBMITTER_CONCURRENCY,
  });
}

// Fails startup unless a provider call fits well inside the consumer's ack_wait. A call that
// outlived its message's lease would see the message redelivered to another instance while it
// is still waiting: a second call with the same key, at the same time. Harmless to the books
// (the provider's idempotency and the conditional transitions decide), but some providers
// answer a concurrent request with the same key with a 409, which the submitter treats as a bug.
export function assertTimeoutFitsAckWait(providerTimeoutMs: number, ackWaitMs: number): void {
  const limit = Math.floor(ackWaitMs * MAX_SHARE_OF_ACK_WAIT);
  if (providerTimeoutMs > limit) {
    throw new Error(
      `PROVIDER_TIMEOUT_MS (${providerTimeoutMs} ms) must be at most ${limit} ms, two thirds of ` +
        `the submitter consumer's ack_wait (${ackWaitMs} ms): a call that outlives its ` +
        "message's lease would see the message redelivered to another instance, and the " +
        'withdrawal sent again, while it is still waiting',
    );
  }
}
