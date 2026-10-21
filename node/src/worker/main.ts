import { setTimeout as sleep } from 'node:timers/promises';
import {
  jetstream,
  jetstreamManager,
  JetStreamApiCodes,
  JetStreamApiError,
  type Consumer,
  type ConsumerMessages,
  type JetStreamManager,
  type JsMsg,
} from '@nats-io/jetstream';
import { decideRandomly } from '../compliance/decision.ts';
import { createPool } from '../shared/db.ts';
import { createLogger } from '../shared/logger.ts';
import { assertStreamExists, closeNats, connectNats, CONSUMERS, STREAMS } from '../shared/nats.ts';
import { loadWorkerConfig } from './config.ts';
import { handleDelivery, retryDelay, type DeliveredMessage, type HandlerDeps } from './handler.ts';

// Entry point: node src/worker/main.ts. Pulls payment.created messages from the durable
// `worker` consumer (created by nats-setup from nats/consumers/) and handles up to
// WORKER_CONCURRENCY of them at once. Several instances can share the consumer: JetStream
// gives each message to one of them.
//
// Messages are pulled only when there's a free slot (fetch exactly the free slots), so every
// message this worker holds is being handled: its handler starts, and with it the working()
// signals, as soon as it arrives. (A prefetching consume() would hold messages in a buffer
// with their ack_wait running and nobody signalling, and redeliver them if the handlers ahead
// take longer than ack_wait.)

// working() at least this often during a review: well under the consumer's 10 s ack_wait.
const WORKING_INTERVAL_MS = 3000;
// On shutdown, how long in-flight messages get to finish. Anything still unfinished isn't
// acked, so JetStream redelivers it: a clean shutdown and a crash take the same path.
const SHUTDOWN_GRACE_MS = 5000;
const NATS_DRAIN_TIMEOUT_MS = 5000;
// An idle fetch long-polls this long on the server before ending empty; the loop then fetches
// again. A message arriving meanwhile is delivered at once. The client also sets an idle
// heartbeat of half this, so a dead connection is noticed during the poll.
const FETCH_EXPIRES_MS = 5000;
// After a failed fetch: wait 1 s, doubling up to 30 s, before the next one.
const FETCH_RETRY = { retryBaseDelayMs: 1000, retryMaxDelayMs: 30_000 };
// A missing consumer or stream is a configuration problem (nats-setup not run, or the
// consumer deleted): after this many attempts in a row, exit with a failing code.
const MAX_MISSING_ATTEMPTS = 5;

const config = loadWorkerConfig();
const logger = createLogger({ service: 'worker', level: config.logLevel });

let shuttingDown = false;

type Running = {
  readonly consumer: Consumer;
  readonly jsm: JetStreamManager;
  readonly deps: HandlerDeps;
  readonly inFlight: Set<Promise<void>>;
  // The fetch currently long-polling, if any, so shutdown can close it at once.
  readonly fetching: { current: ConsumerMessages | undefined };
  // Aborted on shutdown, to cut short a retry wait.
  readonly stopping: AbortSignal;
  readonly shutdown: (reason: string) => Promise<void>;
};

// Connects, checks the stream and consumer exist, and wires the signal handlers. Any failure
// here is a startup failure; failures after it are handled around the consume loop below.
async function start(): Promise<Running> {
  const nc = await connectNats({ url: config.natsUrl, name: 'ledger-demo-worker', logger });
  await assertStreamExists(nc, STREAMS.payments);
  // Throws if the consumer doesn't exist (nats-setup not run).
  const consumer = await jetstream(nc).consumers.get(STREAMS.payments, CONSUMERS.worker);
  const jsm = await jetstreamManager(nc);
  const pool = createPool({ databaseUrl: config.databaseUrl, logger });

  const deps: HandlerDeps = {
    pool,
    logger,
    decide: () => decideRandomly(config.approveRate),
    reviewDelayMs: config.reviewDelayMs,
    workingIntervalMs: WORKING_INTERVAL_MS,
    retryBaseDelayMs: 1000,
    retryMaxDelayMs: 60_000,
  };

  const inFlight = new Set<Promise<void>>();
  const fetching: Running['fetching'] = { current: undefined };
  const stop = new AbortController();

  const shutdown = async (reason: string): Promise<void> => {
    if (shuttingDown) return;
    shuttingDown = true;
    logger.info({ reason, in_flight: inFlight.size }, 'shutting down');
    try {
      // Stop pulling: end a waiting fetch at once and cut short any retry wait.
      stop.abort();
      await fetching.current?.close();
      const finished = await Promise.race([
        Promise.allSettled(inFlight).then(() => true),
        sleep(SHUTDOWN_GRACE_MS, false, { ref: false }),
      ]);
      if (!finished) {
        logger.warn({ in_flight: inFlight.size }, 'unfinished messages will be redelivered');
      }
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
    failAndShutDown('nats connection closed');
  });

  return { consumer, jsm, deps, inFlight, fetching, stopping: stop.signal, shutdown };

  function failAndShutDown(reason: string): void {
    process.exitCode = 1;
    shutdown(reason).catch((err: unknown) => {
      logger.error({ err }, 'shutdown failed');
    });
  }
}

// Fetches exactly as many messages as there are free slots and starts a handler for each as it
// arrives, without awaiting it here; when every slot is busy, it waits for one to free up
// instead of fetching. Safe because every step is a per-payment conditional transition and
// move() guards itself. Returns only on shutdown; throws if the consumer stays missing.
async function consume(r: Running): Promise<void> {
  let failures = 0;
  let missing = 0;
  while (!shuttingDown) {
    const free = config.concurrency - r.inFlight.size;
    if (free === 0) {
      await Promise.race(r.inFlight);
      continue;
    }
    try {
      const batch = await r.consumer.fetch({ max_messages: free, expires: FETCH_EXPIRES_MS });
      r.fetching.current = batch;
      for await (const m of batch) dispatch(r, m);
      failures = 0;
      missing = 0;
    } catch (err) {
      if (shuttingDown) return;
      if (await consumerMissing(r)) {
        missing += 1;
        logger.error({ err, attempt: missing }, 'worker consumer or its stream not found');
        if (missing >= MAX_MISSING_ATTEMPTS) {
          throw new Error(`worker consumer not found after ${missing} attempts`, { cause: err });
        }
      } else {
        missing = 0;
        failures += 1;
        logger.warn({ err, attempt: failures }, 'fetch failed; retrying');
      }
      await pause(retryDelay(FETCH_RETRY, failures + missing), r.stopping);
    } finally {
      r.fetching.current = undefined;
    }
  }

  function dispatch(running: Running, m: JsMsg): void {
    const handling: Promise<void> = handleDelivery(running.deps, delivered(m))
      .then(() => undefined)
      .catch((err: unknown) => {
        // handleDelivery handles its own errors; this is a last resort.
        logger.error({ err }, 'unhandled error while handling a message');
      })
      .finally(() => running.inFlight.delete(handling));
    running.inFlight.add(handling);
  }
}

// After a failed fetch, tells a missing consumer or stream (a configuration problem) from
// anything else (transient, e.g. NATS reconnecting, in which case this lookup fails too).
// The fetch itself only reports "no responders" when its consumer is gone.
async function consumerMissing(r: Running): Promise<boolean> {
  try {
    await r.jsm.consumers.info(STREAMS.payments, CONSUMERS.worker);
    return false;
  } catch (err) {
    return (
      err instanceof JetStreamApiError &&
      (err.code === JetStreamApiCodes.ConsumerNotFound ||
        err.code === JetStreamApiCodes.StreamNotFound)
    );
  }
}

// Waits, but returns early on shutdown.
async function pause(ms: number, signal: AbortSignal): Promise<void> {
  try {
    await sleep(ms, undefined, { signal });
  } catch {
    // Aborted: shutting down.
  }
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
  await consume(running);
} catch (err) {
  logger.fatal({ err }, 'consume loop failed');
  process.exitCode = 1;
  await running.shutdown('consume loop failed').catch((shutdownErr: unknown) => {
    logger.error({ err: shutdownErr }, 'shutdown failed');
  });
}

// Adapts a JetStream message to what the handler needs.
function delivered(m: JsMsg): DeliveredMessage {
  return {
    body: m.string(),
    natsMsgId: m.headers?.get('Nats-Msg-Id') || null,
    deliveryAttempt: m.info.deliveryCount,
    ack: () => m.ack(),
    term: (reason) => m.term(reason),
    nak: (delayMs) => m.nak(delayMs),
    working: () => m.working(),
  };
}
