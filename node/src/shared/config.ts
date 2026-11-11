import { z } from 'zod';

export const LOG_LEVELS = ['fatal', 'error', 'warn', 'info', 'debug', 'trace', 'silent'] as const;
export type LogLevel = (typeof LOG_LEVELS)[number];

// Environment variables shared by the long-running processes that use the database (the API,
// the relay, the deposit-worker and the submitter); scripts and provider-sim parse their own.
// Process-specific variables (PORT, REVIEW_DELAY_MS, …) are parsed by their own modules.
export const sharedEnvSchema = z.object({
  DATABASE_URL: z.url(),
  LOG_LEVEL: z.enum(LOG_LEVELS).default('info'),
  // Telemetry's endpoint (slice 5): optional, and unset or empty is telemetry off, as in Go. Read by
  // startTelemetry, before the process's config; checked here too, so a bad value is a startup
  // failure like any other configuration problem (telemetry stays off for it meanwhile).
  OTEL_EXPORTER_OTLP_ENDPOINT: z.union([z.literal(''), z.url()]).optional(),
  // The log lines' copy to the telemetry backend (slice 5 decision 93): otlp (the specification's
  // default) or none (stdout only, where an agent ships stdout). Read by startTelemetry, as the
  // endpoint is. Only these two: the specification's others (console) have no use here.
  OTEL_LOGS_EXPORTER: z.enum(['otlp', 'none']).default('otlp'),
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
