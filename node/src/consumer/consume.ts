import { setTimeout as sleep } from 'node:timers/promises';
import {
  jetstream,
  jetstreamManager,
  JetStreamApiCodes,
  JetStreamApiError,
  type ConsumerMessages,
  type JsMsg,
} from '@nats-io/jetstream';
import { pause, retryDelay, type Backoff } from '../shared/backoff.ts';
import type { Logger } from '../shared/logger.ts';
import type { NatsConnection } from '../shared/nats.ts';
import { delivered, type DeliveredMessage } from './messages.ts';

// The pull loop shared by the processes that consume a durable JetStream consumer (the worker,
// the submitter): handles up to `concurrency` messages at once. Several instances can share the
// consumer: JetStream gives each message to one of them.
//
// Messages are pulled only when there's a free slot (fetch exactly the free slots), so every
// message a process holds is being handled: its handler starts, and with it any working()
// signals, as soon as it arrives. (A prefetching consume() would hold messages in a buffer with
// their ack_wait running and nobody signalling, and redeliver them if the handlers ahead take
// longer than ack_wait.)

export type ConsumeOptions = {
  readonly nc: NatsConnection;
  readonly stream: string;
  readonly consumer: string;
  readonly concurrency: number;
  readonly logger: Logger;
  // Handles one message, acking, naking or terminating it itself. A rejection is logged as a
  // last resort; handlers are expected to deal with their own errors.
  readonly handle: (msg: DeliveredMessage) => Promise<unknown>;
  // An idle fetch long-polls this long on the server before ending empty; the loop then fetches
  // again. A message arriving meanwhile is delivered at once. The client also sets an idle
  // heartbeat of half this, so a dead connection is noticed during the poll.
  readonly fetchExpiresMs?: number;
  // After a failed fetch: wait 1 s, doubling up to 30 s, before the next one.
  readonly fetchRetry?: Backoff;
  // A missing consumer or stream is a configuration problem (nats-setup not run, or the
  // consumer deleted): after this many attempts in a row, `done` rejects.
  readonly maxMissingAttempts?: number;
  // On stop(), how long in-flight messages get to finish. Anything still unfinished isn't
  // acked, so JetStream redelivers it: a clean shutdown and a crash take the same path.
  readonly shutdownGraceMs?: number;
};

const DEFAULTS = {
  fetchExpiresMs: 5000,
  fetchRetry: { retryBaseDelayMs: 1000, retryMaxDelayMs: 30_000 },
  maxMissingAttempts: 5,
  shutdownGraceMs: 5000,
} as const;

export type Consuming = {
  // Settles when the loop ends: resolves after stop(), rejects if the consumer stays missing.
  readonly done: Promise<void>;
  // Stops pulling (ends a waiting fetch at once, cuts short a retry wait), then waits up to the
  // grace period for in-flight handlers. Safe to call more than once.
  stop(): Promise<void>;
  // How many messages are being handled right now (for logs).
  inFlight(): number;
};

// Looks up the consumer (throws if it doesn't exist: a startup failure for the caller), then
// starts the loop in the background.
export async function startConsuming(options: ConsumeOptions): Promise<Consuming> {
  // Each default resolved with ?? rather than spread ({ ...DEFAULTS, ...options }): a spread lets
  // an option that is present but undefined (e.g. fetchExpiresMs: config.x, with x unset)
  // override its default with undefined. exactOptionalPropertyTypes rejects that at compile
  // time, but a value typed loosely elsewhere could still bring it in.
  const o = {
    ...options,
    fetchExpiresMs: options.fetchExpiresMs ?? DEFAULTS.fetchExpiresMs,
    fetchRetry: options.fetchRetry ?? DEFAULTS.fetchRetry,
    maxMissingAttempts: options.maxMissingAttempts ?? DEFAULTS.maxMissingAttempts,
    shutdownGraceMs: options.shutdownGraceMs ?? DEFAULTS.shutdownGraceMs,
  };
  const consumer = await jetstream(o.nc).consumers.get(o.stream, o.consumer);
  const jsm = await jetstreamManager(o.nc);

  const inFlight = new Set<Promise<void>>();
  // The fetch currently long-polling, if any, so stop() can close it at once.
  let fetching: ConsumerMessages | undefined;
  // Aborted by stop(), to cut short a retry wait.
  const stopSignal = new AbortController();
  let stopping = false;
  let stopped: Promise<void> | undefined;

  // Fetches exactly as many messages as there are free slots and starts a handler for each as
  // it arrives, without awaiting it here; when every slot is busy, it waits for one to free up
  // instead of fetching. Returns only after stop(); throws if the consumer stays missing.
  const loop = async (): Promise<void> => {
    let failures = 0;
    let missing = 0;
    while (!stopping) {
      const free = o.concurrency - inFlight.size;
      if (free === 0) {
        await Promise.race(inFlight);
        continue;
      }
      try {
        const batch = await consumer.fetch({ max_messages: free, expires: o.fetchExpiresMs });
        fetching = batch;
        for await (const m of batch) dispatch(m);
        failures = 0;
        missing = 0;
      } catch (err) {
        if (stopping) return;
        if (await consumerMissing()) {
          missing += 1;
          o.logger.error(
            { err, attempt: missing },
            `${o.consumer} consumer or its stream not found`,
          );
          if (missing >= o.maxMissingAttempts) {
            throw new Error(`${o.consumer} consumer not found after ${missing} attempts`, {
              cause: err,
            });
          }
        } else {
          missing = 0;
          failures += 1;
          o.logger.warn({ err, attempt: failures }, 'fetch failed; retrying');
        }
        await pause(retryDelay(o.fetchRetry, failures + missing), stopSignal.signal);
      } finally {
        fetching = undefined;
      }
    }
  };

  function dispatch(m: JsMsg): void {
    const handling: Promise<void> = o
      .handle(delivered(m))
      .then(() => undefined)
      .catch((err: unknown) => {
        // Handlers handle their own errors; this is a last resort.
        o.logger.error({ err }, 'unhandled error while handling a message');
      })
      .finally(() => inFlight.delete(handling));
    inFlight.add(handling);
  }

  // After a failed fetch, tells a missing consumer or stream (a configuration problem) from
  // anything else (transient, e.g. NATS reconnecting, in which case this lookup fails too).
  // The fetch itself only reports "no responders" when its consumer is gone.
  async function consumerMissing(): Promise<boolean> {
    try {
      await jsm.consumers.info(o.stream, o.consumer);
      return false;
    } catch (err) {
      return (
        err instanceof JetStreamApiError &&
        (err.code === JetStreamApiCodes.ConsumerNotFound ||
          err.code === JetStreamApiCodes.StreamNotFound)
      );
    }
  }

  async function stopNow(): Promise<void> {
    stopping = true;
    // Stop pulling: end a waiting fetch at once and cut short any retry wait.
    stopSignal.abort();
    await fetching?.close();
    const finished = await Promise.race([
      Promise.allSettled(inFlight).then(() => true),
      sleep(o.shutdownGraceMs, false, { ref: false }),
    ]);
    if (!finished) {
      o.logger.warn({ in_flight: inFlight.size }, 'unfinished messages will be redelivered');
    }
  }

  return {
    done: loop(),
    stop: () => (stopped ??= stopNow()),
    inFlight: () => inFlight.size,
  };
}
