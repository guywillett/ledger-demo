import { z } from 'zod';
import { LOG_LEVELS, parseEnv, type LogLevel } from '../shared/config.ts';

// provider-sim's environment. It needs no database and no NATS, so it doesn't use the shared
// schema. z.coerce: environment variables are always strings.
const rate = z.coerce.number().min(0).max(1).default(0);

const simEnvSchema = z.object({
  PORT: z.coerce.number().int().min(1).max(65535).default(4000),
  LOG_LEVEL: z.enum(LOG_LEVELS).default('info'),
  // Where transfer results are POSTed. Unset: results are logged, not sent.
  SIM_WEBHOOK_URL: z.url().optional(),
  // From a transfer's creation to its result.
  SIM_EXECUTION_DELAY_MS: z.coerce.number().int().min(0).max(600_000).default(2000),
  // How long a "slow" submission takes to answer: longer than the submitter's timeout.
  SIM_SLOW_DELAY_MS: z.coerce.number().int().min(0).max(600_000).default(10_000),
  // Webhook deliveries: give up after this many attempts.
  SIM_WEBHOOK_MAX_ATTEMPTS: z.coerce.number().int().min(1).max(1000).default(20),
  // Each webhook request is abandoned (and retried) after this long.
  SIM_WEBHOOK_TIMEOUT_MS: z.coerce.number().int().min(100).max(60_000).default(5000),
  // Random failure rates for chaos runs; 0 (off) by default. Triggers in remittance_info win.
  SIM_REFUSE_RATE: rate,
  SIM_ERROR_RATE: rate,
  SIM_SLOW_RATE: rate,
  SIM_FAIL_RATE: rate,
  SIM_DUPLICATE_RATE: rate,
  SIM_EARLY_RATE: rate,
});

export type Rates = {
  readonly refuse: number;
  readonly error: number;
  readonly slow: number;
  readonly fail: number;
  readonly duplicate: number;
  readonly early: number;
};

export type SimConfig = {
  readonly port: number;
  readonly logLevel: LogLevel;
  readonly webhookUrl: string | undefined;
  readonly executionDelayMs: number;
  readonly slowDelayMs: number;
  readonly webhookMaxAttempts: number;
  readonly webhookTimeoutMs: number;
  readonly rates: Rates;
};

export function loadSimConfig(env: NodeJS.ProcessEnv = process.env): SimConfig {
  const parsed = parseEnv(simEnvSchema, env);
  return Object.freeze({
    port: parsed.PORT,
    logLevel: parsed.LOG_LEVEL,
    webhookUrl: parsed.SIM_WEBHOOK_URL,
    executionDelayMs: parsed.SIM_EXECUTION_DELAY_MS,
    slowDelayMs: parsed.SIM_SLOW_DELAY_MS,
    webhookMaxAttempts: parsed.SIM_WEBHOOK_MAX_ATTEMPTS,
    webhookTimeoutMs: parsed.SIM_WEBHOOK_TIMEOUT_MS,
    rates: {
      refuse: parsed.SIM_REFUSE_RATE,
      error: parsed.SIM_ERROR_RATE,
      slow: parsed.SIM_SLOW_RATE,
      fail: parsed.SIM_FAIL_RATE,
      duplicate: parsed.SIM_DUPLICATE_RATE,
      early: parsed.SIM_EARLY_RATE,
    },
  });
}
