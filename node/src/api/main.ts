import { paymentEvents } from '../ingestion/events.ts';
import { createPool } from '../shared/db.ts';
import { createLogger } from '../shared/logger.ts';
import {
  assertStreamExists,
  closeNats,
  connectNats,
  jetStreamPublisher,
  PUBLISH_TIMEOUT_MS,
  STREAMS,
} from '../shared/nats.ts';
import { buildApp } from './app.ts';
import { loadApiConfig } from './config.ts';

// Entry point: node src/api/main.ts. Top-level await works because this is an ES module.

const NATS_DRAIN_TIMEOUT_MS = 5000;

const config = loadApiConfig();
const logger = createLogger({ service: 'api', level: config.logLevel });

let shuttingDown = false;

try {
  // External dependencies first: if NATS or its stream is missing, nothing else is created.
  const nc = await connectNats({ url: config.natsUrl, name: 'ledger-demo-api', logger });
  await assertStreamExists(nc, STREAMS.payments);
  const pool = createPool({ databaseUrl: config.databaseUrl, logger });

  // Wiring: the only place the real implementations are chosen. paymentEvents() wraps the
  // JetStream publisher; the app receives only the PaymentEvents interface. Tests wire the
  // same app with other publishers (test/helpers.ts).
  const events = paymentEvents(
    jetStreamPublisher(nc, { stream: STREAMS.payments, timeoutMs: PUBLISH_TIMEOUT_MS }),
  );
  const app = buildApp({ db: pool, events, logger });

  // app.close() stops accepting connections and waits for in-flight requests (including
  // their publishes); closeNats() drains the connection, but gives up if NATS is unreachable;
  // the pool is closed whatever happened before it.
  const shutdown = async (reason: string): Promise<void> => {
    if (shuttingDown) return;
    shuttingDown = true;
    logger.info({ reason }, 'shutting down');
    try {
      await app.close();
      const nats = await closeNats(nc, NATS_DRAIN_TIMEOUT_MS);
      if (nats === 'closed') logger.warn('nats connection closed without draining');
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

  // The connection reconnects forever, so it only closes for good on shutdown or a fatal
  // error (e.g. an authorization failure). Outside a shutdown, every publish would fail from
  // then on: shut down with a failing exit code instead, so the container is restarted.
  void nc.closed().then((err) => {
    if (shuttingDown) return;
    logger.fatal({ err }, 'nats connection closed unexpectedly');
    process.exitCode = 1;
    shutdown('nats connection closed').catch((shutdownErr: unknown) => {
      logger.error({ err: shutdownErr }, 'shutdown failed');
    });
  });

  await app.listen({ host: '0.0.0.0', port: config.port });
} catch (err) {
  // One structured line instead of a raw stack trace; exit at once (container will restart), since a half-started
  // process (e.g. a NATS connection still reconnecting) would otherwise keep running.
  logger.fatal({ err }, 'startup failed');
  process.exit(1);
}
