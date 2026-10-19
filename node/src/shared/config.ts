import { z } from 'zod';

export const LOG_LEVELS = ['fatal', 'error', 'warn', 'info', 'debug', 'trace', 'silent'] as const;
export type LogLevel = (typeof LOG_LEVELS)[number];

// Environment variables shared by every process (API, worker, scripts).
// Process-specific variables (PORT, REVIEW_DELAY_MS, …) extend this schema in their own modules.
export const sharedEnvSchema = z.object({
  DATABASE_URL: z.url(),
  NATS_URL: z.url(),
  LOG_LEVEL: z.enum(LOG_LEVELS).default('info'),
});

export type SharedConfig = {
  readonly databaseUrl: string;
  readonly natsUrl: string;
  readonly logLevel: LogLevel;
};

// Validates `env` against `schema`, throwing one error that lists every problem.
export function parseEnv<S extends z.ZodType>(schema: S, env: NodeJS.ProcessEnv = process.env): z.infer<S> {
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
    natsUrl: parsed.NATS_URL,
    logLevel: parsed.LOG_LEVEL,
  });
}
