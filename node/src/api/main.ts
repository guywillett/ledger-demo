import { createPool } from '../shared/db.ts';
import { createLogger } from '../shared/logger.ts';
import { buildApp } from './app.ts';
import { loadApiConfig } from './config.ts';

// Entry point: node src/api/main.ts. Top-level await works because this is an ES module.
// The API needs only Postgres: events go to the outbox, and the relay publishes them.

const config = loadApiConfig();
const logger = createLogger({ service: 'api', level: config.logLevel });

let shuttingDown = false;

try {
  const pool = createPool({ databaseUrl: config.databaseUrl, logger });
  const app = buildApp({ pool, logger });

  // app.close() stops accepting connections and waits for in-flight requests; the pool is
  // closed whatever happened before it.
  const shutdown = async (reason: string): Promise<void> => {
    if (shuttingDown) return;
    shuttingDown = true;
    logger.info({ reason }, 'shutting down');
    try {
      await app.close();
    } finally {
      await pool.end();
    }
  };

  // docker compose stop sends SIGTERM; Ctrl-C sends SIGINT.
  for (const signal of ['SIGTERM', 'SIGINT'] as const) {
    process.once(signal, (received) => {
      shutdown(received).catch((err: unknown) => {
        logger.error({ err }, 'shutdown failed');
        process.exitCode = 1;
      });
    });
  }

  await app.listen({ host: '0.0.0.0', port: config.port });
} catch (err) {
  // One structured line instead of a raw stack trace; exit at once (the container will
  // restart).
  logger.fatal({ err }, 'startup failed');
  process.exit(1);
}
