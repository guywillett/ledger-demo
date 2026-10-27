import { z } from 'zod';

export const LOG_LEVELS = ['fatal', 'error', 'warn', 'info', 'debug', 'trace', 'silent'] as const;
export type LogLevel = (typeof LOG_LEVELS)[number];

// Environment variables shared by the long-running processes that use the database (the API,
// the relay, the deposit-worker and the submitter); scripts and provider-sim parse their own.
// Process-specific variables (PORT, REVIEW_DELAY_MS, …) are parsed by their own modules.
export const sharedEnvSchema = z.object({
  DATABASE_URL: z.url(),
  LOG_LEVEL: z.enum(LOG_LEVELS).default('info'),
});

export type SharedConfig = {
  readonly databaseUrl: string;
  readonly logLevel: LogLevel;
};

// For every process that connects to NATS (the relay, the deposit-worker and the submitter):
// their env schemas extend this one. The API doesn't connect to NATS, so it doesn't require
// NATS_URL.
export const natsEnvSchema = z.object({
  NATS_URL: z.url(),
});

// Validates `env` against `schema`, throwing one error that lists every problem.
export function parseEnv<S extends z.ZodType>(
  schema: S,
  env: NodeJS.ProcessEnv = process.env,
): z.infer<S> {
  const result = schema.safeParse(env);
  if (!result.success) {
    throw new Error(`Invalid configuration:\n${z.prettifyError(result.error)}`);
  }
  return result.data;
}

export function loadSharedConfig(env: NodeJS.ProcessEnv = process.env): SharedConfig {
  const parsed = parseEnv(sharedEnvSchema, env);
  return Object.freeze({
    databaseUrl: parsed.DATABASE_URL,
    logLevel: parsed.LOG_LEVEL,
  });
}
