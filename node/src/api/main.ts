import { createPool } from '../shared/db.ts';
import { createLogger } from '../shared/logger.ts';
import { buildApp } from './app.ts';
import { loadApiConfig } from './config.ts';

// Entry point: node src/api/main.ts. Top-level await works because this is an ES module.

const config = loadApiConfig();
const logger = createLogger({ service: 'api', level: config.logLevel });
const pool = createPool({ databaseUrl: config.databaseUrl, logger });
const app = buildApp({ db: pool, logger });

// docker compose stop sends SIGTERM; Ctrl-C sends SIGINT. app.close() stops accepting
// connections and waits for in-flight requests; then the pool's connections are closed.
let shuttingDown = false;
async function shutdown(signal: NodeJS.Signals): Promise<void> {
  if (shuttingDown) return;
  shuttingDown = true;
  logger.info({ signal }, 'shutting down');
  await app.close();
  await pool.end();
}
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.once(signal, (received) => {
    shutdown(received).catch((err: unknown) => {
      logger.error({ err }, 'shutdown failed');
      process.exitCode = 1;
    });
  });
}

await app.listen({ host: '0.0.0.0', port: config.port });
