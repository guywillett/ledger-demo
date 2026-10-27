import { z } from 'zod';
import { loadSharedConfig, natsEnvSchema, parseEnv, type SharedConfig } from '../shared/config.ts';

// z.coerce: environment variables are always strings.
const workerEnvSchema = natsEnvSchema.extend({
  // How long the pseudo compliance review takes.
  REVIEW_DELAY_MS: z.coerce.number().int().min(0).max(60_000).default(2000),
  // Probability of approving a deposit (the rest are rejected).
  APPROVE_RATE: z.coerce.number().min(0).max(1).default(0.8),
  // How many messages one worker instance handles at once.
  WORKER_CONCURRENCY: z.coerce.number().int().min(1).max(1000).default(10),
});

export type WorkerConfig = SharedConfig & {
  readonly natsUrl: string;
  readonly reviewDelayMs: number;
  readonly approveRate: number;
  readonly concurrency: number;
};

export function loadWorkerConfig(env: NodeJS.ProcessEnv = process.env): WorkerConfig {
  const shared = loadSharedConfig(env);
  const parsed = parseEnv(workerEnvSchema, env);
  return Object.freeze({
    ...shared,
    natsUrl: parsed.NATS_URL,
    reviewDelayMs: parsed.REVIEW_DELAY_MS,
    approveRate: parsed.APPROVE_RATE,
    concurrency: parsed.WORKER_CONCURRENCY,
  });
}
