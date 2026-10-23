import { jetstreamManager } from '@nats-io/jetstream';
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
import { assertTimeoutFitsAckWait, loadSubmitterConfig } from './config.ts';
import { handleSubmission, type SubmitterDeps } from './handler.ts';
import { providerClient } from './provider.ts';

// Entry point: node src/submitter/main.ts. Pulls withdrawal.held messages from the durable
// `submitter` consumer and sends each withdrawal to the provider, up to SUBMITTER_CONCURRENCY at
// once, through the pull loop shared with the worker (src/consumer/consume.ts). Several
// instances can share the consumer: JetStream gives each message to one of them.

// After an unknown outcome or a transient failure: nak with 1 s, doubling per attempt up to 60 s.
const DELIVERY_RETRY = { retryBaseDelayMs: 1000, retryMaxDelayMs: 60_000 };

const config = loadSubmitterConfig();
const logger = createLogger({ service: 'submitter', level: config.logLevel });

let shuttingDown = false;

type Running = {
  readonly consuming: Consuming;
  readonly shutdown: (reason: string) => Promise<void>;
};

// Connects, checks the stream and consumer exist, starts the pull loop and wires the signal
// handlers. Any failure here is a startup failure.
async function start(): Promise<Running> {
  const nc = await connectNats({ url: config.natsUrl, name: 'ledger-demo-submitter', logger });
  await assertStreamExists(nc, STREAMS.withdrawals);
  // The consumer's real ack_wait (nanoseconds in NATS), so the timeout check can't drift from
  // nats/consumers/WITHDRAWALS/submitter.json. Throws if the consumer doesn't exist.
  const consumerInfo = await (
    await jetstreamManager(nc)
  ).consumers.info(STREAMS.withdrawals, CONSUMERS.submitter);
  // Unset means the server's default, 30 s.
  const ackWaitNs = consumerInfo.config.ack_wait ?? 30_000_000_000;
  assertTimeoutFitsAckWait(config.providerTimeoutMs, ackWaitNs / 1e6);
  const pool = createPool({ databaseUrl: config.databaseUrl, logger });

  const deps: SubmitterDeps = {
    pool,
    logger,
    provider: providerClient({ baseUrl: config.providerUrl, timeoutMs: config.providerTimeoutMs }),
    ...DELIVERY_RETRY,
  };

  // Throws if the consumer doesn't exist (nats-setup not run).
  const consuming = await startConsuming({
    nc,
    stream: STREAMS.withdrawals,
    consumer: CONSUMERS.submitter,
    concurrency: config.concurrency,
    logger,
    handle: (msg) => handleSubmission(deps, msg),
  });

  // As the worker: stop the loop (in-flight messages get their grace period), drain NATS, then
  // close the pool. An unfinished submission is redelivered and asks again with the same key.
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

  // Outside a shutdown, the connection only closes for good on a fatal error.
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
  logger.fatal({ err }, 'startup failed');
  process.exit(1);
}

logger.info(
  { concurrency: config.concurrency, provider_timeout_ms: config.providerTimeoutMs },
  'submitter started',
);

try {
  await running.consuming.done;
} catch (err) {
  logger.fatal({ err }, 'consume loop failed');
  process.exitCode = 1;
  await running.shutdown('consume loop failed').catch((shutdownErr: unknown) => {
    logger.error({ err: shutdownErr }, 'shutdown failed');
  });
}
