import { decideRandomly } from '../compliance/decision.ts';
import { startConsuming, type Consuming } from '../consumer/consume.ts';
import { createPool } from '../shared/db.ts';
import { createLogger } from '../shared/logger.ts';
import {
  assertStreamExists,
  closeNats,
  connectNats,
  CONSUMERS,
  NATS_DRAIN_TIMEOUT_MS,
  STREAMS,
} from '../shared/nats.ts';
import { loadWorkerConfig } from './config.ts';
import { handleDelivery, type HandlerDeps } from './handler.ts';

// Entry point: node src/worker/main.ts. Pulls payment.created messages from the durable
// `worker` consumer (created by nats-setup from nats/consumers/) and handles up to
// WORKER_CONCURRENCY of them at once, through the shared pull loop (src/consumer/consume.ts:
// fetch exactly the free slots, retries, shutdown grace). Several instances can share the
// consumer: JetStream gives each message to one of them.

// working() at least this often during a review: well under the consumer's 10 s ack_wait.
const WORKING_INTERVAL_MS = 3000;
// After a transient failure handling a message: nak with 1 s, doubling per attempt up to 60 s.
const DELIVERY_RETRY = { retryBaseDelayMs: 1000, retryMaxDelayMs: 60_000 };

const config = loadWorkerConfig();
const logger = createLogger({ service: 'worker', level: config.logLevel });

let shuttingDown = false;

type Running = {
  readonly consuming: Consuming;
  readonly shutdown: (reason: string) => Promise<void>;
};

// Connects, checks the stream and consumer exist, starts the pull loop and wires the signal
// handlers. Any failure here is a startup failure; failures after it are handled around the
// loop's `done` below.
async function start(): Promise<Running> {
  const nc = await connectNats({ url: config.natsUrl, name: 'ledger-demo-worker', logger });
  await assertStreamExists(nc, STREAMS.payments);
  const pool = createPool({ databaseUrl: config.databaseUrl, logger });

  const deps: HandlerDeps = {
    pool,
    logger,
    decide: () => decideRandomly(config.approveRate),
    reviewDelayMs: config.reviewDelayMs,
    workingIntervalMs: WORKING_INTERVAL_MS,
    ...DELIVERY_RETRY,
  };

  // Throws if the consumer doesn't exist (nats-setup not run).
  const consuming = await startConsuming({
    nc,
    stream: STREAMS.payments,
    consumer: CONSUMERS.worker,
    concurrency: config.concurrency,
    logger,
    handle: (msg) => handleDelivery(deps, msg),
  });

  // Stop the loop (in-flight messages get their grace period), then drain NATS, then close the
  // pool.
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
}

let running: Running;
try {
  running = await start();
} catch (err) {
  // Nothing is running yet that a graceful shutdown would protect; exit at once, since a
  // half-started process (e.g. a NATS connection still reconnecting) would otherwise linger.
  logger.fatal({ err }, 'startup failed');
  process.exit(1);
}

logger.info({ concurrency: config.concurrency }, 'worker started');

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
