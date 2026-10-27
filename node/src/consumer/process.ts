import type pg from 'pg';
import { createPool } from '../shared/db.ts';
import type { LogLevel } from '../shared/config.ts';
import { createLogger, type Logger } from '../shared/logger.ts';
import {
  assertStreamExists,
  closeNats,
  connectNats,
  NATS_DRAIN_TIMEOUT_MS,
  type NatsConnection,
} from '../shared/nats.ts';
import { startConsuming, type Consuming } from './consume.ts';
import type { DeliveredMessage } from './messages.ts';

// The process lifecycle shared by every consumer process (the deposit-worker, the submitter):
// connect, check what the process needs, start the pull loop (consume.ts), shut down gracefully on
// a signal, and exit with a failing code when something goes wrong for good, so the container is
// restarted. Each entry point (src/<consumer>/main.ts) only loads its config and builds its
// handler: what differs between consumers is exactly what they pass in here.

export type ConsumerProcessOptions = {
  // Names the logger's service, the NATS connection (ledger-demo-<service>) and the
  // "<service> started" line.
  readonly service: string;
  readonly logLevel: LogLevel;
  readonly natsUrl: string;
  readonly databaseUrl: string;
  readonly stream: string;
  readonly consumer: string;
  readonly concurrency: number;
  // Extra checks once NATS is connected and the stream exists, before anything else is
  // created; a throw is a startup failure.
  readonly startupCheck?: (nc: NatsConnection) => Promise<void>;
  // Builds the message handler, given the process's pool and logger.
  readonly makeHandler: (env: {
    readonly pool: pg.Pool;
    readonly logger: Logger;
  }) => (msg: DeliveredMessage) => Promise<unknown>;
  // Extra fields for the "<service> started" line (concurrency is always there).
  readonly startedFields?: Record<string, unknown>;
};

type Running = {
  readonly consuming: Consuming;
  readonly shutdown: (reason: string) => Promise<void>;
};

// Runs until the loop stops: after a signal's graceful shutdown, or a fatal failure (with exit
// code 1). Exits at once if startup fails.
export async function runConsumerProcess(o: ConsumerProcessOptions): Promise<void> {
  const logger = createLogger({ service: o.service, level: o.logLevel });
  let shuttingDown = false;

  // Connects, checks the stream (and whatever startupCheck adds) and the consumer exist, starts
  // the pull loop and wires the signal handlers. Any failure here is a startup failure; failures
  // after it are handled around the loop's `done` below.
  const start = async (): Promise<Running> => {
    const nc = await connectNats({ url: o.natsUrl, name: `ledger-demo-${o.service}`, logger });
    await assertStreamExists(nc, o.stream);
    await o.startupCheck?.(nc);
    const pool = createPool({ databaseUrl: o.databaseUrl, logger });

    // Throws if the consumer doesn't exist (nats-setup not run).
    const consuming = await startConsuming({
      nc,
      stream: o.stream,
      consumer: o.consumer,
      concurrency: o.concurrency,
      logger,
      handle: o.makeHandler({ pool, logger }),
    });

    // Stop the loop (in-flight messages get their grace period; unfinished ones are
    // redelivered), then drain NATS, then close the pool.
    const shutdown = async (reason: string): Promise<void> => {
      if (shuttingDown) return;
      shuttingDown = true;
      logger.info({ reason, in_flight: consuming.inFlight() }, 'shutting down');
      try {
        await consuming.stop();
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

    // As in the API: outside a shutdown, the connection only closes for good on a fatal error.
    void nc.closed().then((err) => {
      if (shuttingDown) return;
      logger.fatal({ err }, 'nats connection closed unexpectedly');
      process.exitCode = 1;
      shutdown('nats connection closed').catch((shutdownErr: unknown) => {
        logger.error({ err: shutdownErr }, 'shutdown failed');
      });
    });

    return { consuming, shutdown };
  };

  let running: Running;
  try {
    running = await start();
  } catch (err) {
    // Nothing is running yet that a graceful shutdown would protect; exit at once, since a
    // half-started process (e.g. a NATS connection still reconnecting) would otherwise linger.
    logger.fatal({ err }, 'startup failed');
    process.exit(1);
  }

  logger.info({ concurrency: o.concurrency, ...o.startedFields }, `${o.service} started`);

  // After startup, failures go through the graceful shutdown: in-flight messages get their
  // grace period, NATS is drained, the pool closed. The exit code says it wasn't a clean stop,
  // so the container is restarted.
  try {
    await running.consuming.done;
  } catch (err) {
    logger.fatal({ err }, 'consume loop failed');
    process.exitCode = 1;
    await running.shutdown('consume loop failed').catch((shutdownErr: unknown) => {
      logger.error({ err: shutdownErr }, 'shutdown failed');
    });
  }
}
