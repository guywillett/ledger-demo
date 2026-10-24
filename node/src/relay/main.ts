import { pause, retryDelay } from '../shared/backoff.ts';
import { createPool } from '../shared/db.ts';
import { createLogger } from '../shared/logger.ts';
import {
  assertStreamExists,
  closeNats,
  connectNats,
  jetStreamPublisher,
  NATS_DRAIN_TIMEOUT_MS,
  PUBLISH_TIMEOUT_MS,
  STREAMS,
} from '../shared/nats.ts';
import { loadRelayConfig } from './config.ts';
import { relayBatch, type BatchResult, type RelayDeps } from './relay.ts';

// Entry point: node src/relay/main.ts. Polls the outbox and publishes what it finds (relay.ts).
// Several instances can run at once: claims are disjoint (FOR UPDATE SKIP LOCKED) and leased.
//
// Polling: a full batch means there may be more, so the next one runs at once. Otherwise the
// relay has caught up and waits: 200 ms after a batch that found something, then doubling while
// it keeps finding nothing, up to 1 s. LISTEN/NOTIFY could wake it at once instead (a later
// optimisation). A batch in which nothing could be published (e.g. NATS down, every publish
// failing fast) backs off like a failed batch, rather than racing through the backlog.

// Between polls when caught up: 200 ms, doubling while idle, at most 1 s.
const IDLE_DELAY = { retryBaseDelayMs: 200, retryMaxDelayMs: 1000 };
// After a failed batch (e.g. the database is down), or one in which every publish failed: 1 s,
// doubling, at most 30 s.
const FAILURE_RETRY = { retryBaseDelayMs: 1000, retryMaxDelayMs: 30_000 };
// Before retrying an event whose publish failed: 1 s after the first attempt, doubling per
// attempt, at most 60 s.
const PUBLISH_RETRY = { retryBaseDelayMs: 1000, retryMaxDelayMs: 60_000 };

const config = loadRelayConfig();
const logger = createLogger({ service: 'relay', level: config.logLevel });

let shuttingDown = false;
// Aborted on shutdown, to cut short a wait between polls.
const stop = new AbortController();

// The poll loop, as a promise that settles when the loop has stopped, so shutdown can wait for
// the batch in progress. Assigned once the loop starts.
let polling: Promise<void> = Promise.resolve();

// Connects, checks the stream exists, and wires the signal handlers. Any failure here is a
// startup failure.
async function start(): Promise<RelayDeps> {
  const nc = await connectNats({ url: config.natsUrl, name: 'ledger-demo-relay', logger });
  for (const stream of Object.values(STREAMS)) await assertStreamExists(nc, stream);
  const pool = createPool({ databaseUrl: config.databaseUrl, logger });

  const deps: RelayDeps = {
    pool,
    publisher: jetStreamPublisher(nc, { timeoutMs: PUBLISH_TIMEOUT_MS }),
    logger,
    batchSize: config.batchSize,
    leaseMs: config.leaseMs,
    retryDelay: (attempts) => retryDelay(PUBLISH_RETRY, attempts),
  };

  // Stop polling and let the batch in progress finish (its events are then marked, so nothing
  // is published twice), then drain NATS (giving up if it is unreachable) and close the pool.
  const shutdown = async (reason: string): Promise<void> => {
    if (shuttingDown) return;
    shuttingDown = true;
    logger.info({ reason }, 'shutting down');
    try {
      stop.abort();
      await polling;
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

  // As in the worker: outside a shutdown, the connection only closes for good on a fatal error,
  // and every publish would fail from then on. Exit with a failing code, so the container is
  // restarted.
  void nc.closed().then((err) => {
    if (shuttingDown) return;
    logger.fatal({ err }, 'nats connection closed unexpectedly');
    process.exitCode = 1;
    shutdown('nats connection closed').catch((shutdownErr: unknown) => {
      logger.error({ err: shutdownErr }, 'shutdown failed');
    });
  });

  return deps;
}

// Never throws: a failed batch is logged and retried with backoff. Returns on shutdown.
async function poll(deps: RelayDeps): Promise<void> {
  let idle = 0;
  let failures = 0;
  while (!shuttingDown) {
    let batch: BatchResult;
    try {
      batch = await relayBatch(deps);
    } catch (err) {
      if (shuttingDown) return;
      failures += 1;
      logger.error({ err, attempt: failures }, 'relay batch failed; retrying');
      await pause(retryDelay(FAILURE_RETRY, failures), stop.signal);
      continue;
    }
    if (batch.claimed > 0 && batch.published === 0) {
      // Each event is already backed off on its own (markFailed), but the rest of the backlog
      // would fail the same way: wait before claiming more.
      failures += 1;
      logger.warn(
        { claimed: batch.claimed, attempt: failures },
        'nothing in the batch could be published; backing off',
      );
      await pause(retryDelay(FAILURE_RETRY, failures), stop.signal);
      continue;
    }
    failures = 0;
    if (batch.claimed === deps.batchSize) {
      idle = 0;
      continue;
    }
    idle = batch.claimed === 0 ? idle + 1 : 1;
    await pause(retryDelay(IDLE_DELAY, idle), stop.signal);
  }
}

let deps: RelayDeps;
try {
  deps = await start();
} catch (err) {
  // Nothing is running yet that a graceful shutdown would protect; exit at once, since a
  // half-started process (e.g. a NATS connection still reconnecting) would otherwise linger.
  logger.fatal({ err }, 'startup failed');
  process.exit(1);
}

logger.info({ batch_size: config.batchSize, lease_ms: config.leaseMs }, 'relay started');
polling = poll(deps);
await polling;
