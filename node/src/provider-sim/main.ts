import { setTimeout as sleep } from 'node:timers/promises';
import { createLogger } from '../shared/logger.ts';
import { buildSimApp, type TransferUpdate } from './app.ts';
import { loadSimConfig } from './config.ts';

// Entry point: node src/provider-sim/main.ts (compose service provider-sim). See app.ts.

const config = loadSimConfig();
const logger = createLogger({ service: 'provider-sim', level: config.logLevel });

// POSTs one update to the webhook. fetch() itself never times out, so AbortSignal.timeout()
// aborts a request that hangs; any non-2xx answer counts as a failed delivery, to be retried.
function webhookSender(url: string): (update: TransferUpdate) => Promise<void> {
  return async (update) => {
    const response = await fetch(url, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(update),
      signal: AbortSignal.timeout(config.webhookTimeoutMs),
    });
    if (!response.ok) throw new Error(`webhook answered ${response.status}`);
  };
}

const sim = buildSimApp({
  logger,
  now: () => new Date(),
  random: Math.random,
  deliver: config.webhookUrl === undefined ? undefined : webhookSender(config.webhookUrl),
  // Aborting settles the wait at once; the app checks the signal afterwards.
  sleep: (ms, signal) => sleep(ms, undefined, { signal }).catch(() => undefined),
  executionDelayMs: config.executionDelayMs,
  slowDelayMs: config.slowDelayMs,
  webhookMaxAttempts: config.webhookMaxAttempts,
  rates: config.rates,
});

let shuttingDown = false;

// Stops accepting requests, then abandons scheduled results and webhook retries (the transfers
// are forgotten anyway: state is in memory).
const shutdown = async (reason: string): Promise<void> => {
  if (shuttingDown) return;
  shuttingDown = true;
  logger.info({ reason }, 'shutting down');
  await sim.app.close();
  const abandoned = sim.stop();
  if (abandoned > 0) logger.warn({ abandoned }, 'abandoned pending transfer results');
};

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.once(signal, (received) => {
    shutdown(received).catch((err: unknown) => {
      logger.error({ err }, 'shutdown failed');
      process.exitCode = 1;
    });
  });
}

try {
  await sim.app.listen({ host: '0.0.0.0', port: config.port });
  logger.info(
    { webhook: config.webhookUrl ?? null, execution_delay_ms: config.executionDelayMs },
    'provider-sim started',
  );
} catch (err) {
  logger.fatal({ err }, 'startup failed');
  process.exit(1);
}
