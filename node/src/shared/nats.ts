import { setTimeout as sleep } from 'node:timers/promises';
import { jetstream, jetstreamManager } from '@nats-io/jetstream';
import { connect, type NatsConnection, type Status } from '@nats-io/transport-node';
import type { Logger } from './logger.ts';

export type { NatsConnection };

// Streams are created by the nats-setup compose service from nats/streams/*.json, not by the
// apps; see nats/setup.sh.
export const STREAMS = { payments: 'PAYMENTS' } as const;
// Durable consumers, created by nats-setup from nats/consumers/<STREAM>/<NAME>.json.
export const CONSUMERS = { worker: 'worker' } as const;

// How long a publish waits for the stream's acknowledgement before it counts as failed.
export const PUBLISH_TIMEOUT_MS = 2000;
// On shutdown, how long to let the connection drain before closing it (see closeNats).
export const NATS_DRAIN_TIMEOUT_MS = 5000;

export async function connectNats(options: {
  url: string;
  name: string;
  logger: Logger;
}): Promise<NatsConnection> {
  const nc = await connect({
    servers: options.url,
    // Shown in the server's connection list (nats server report connections, /connz).
    name: options.name,
    // Keep trying for as long as the process runs; publishes fail (and are reported) meanwhile.
    maxReconnectAttempts: -1,
  });
  // status() is an async iterator that ends when the connection closes. Consumed in the
  // background: `void` marks the promise as deliberately not awaited.
  void logStatus(nc, options.logger);
  return nc;
}

async function logStatus(nc: NatsConnection, logger: Logger): Promise<void> {
  for await (const status of nc.status()) {
    logStatusEvent(status, logger);
  }
}

function logStatusEvent(status: Status, logger: Logger): void {
  switch (status.type) {
    case 'disconnect':
    case 'staleConnection':
    case 'slowConsumer':
      logger.warn({ nats_status: status.type }, 'nats connection problem');
      return;
    case 'error':
      logger.error({ nats_status: status.type, err: status.error }, 'nats server error');
      return;
    case 'reconnect':
    case 'close':
      logger.info({ nats_status: status.type }, 'nats connection status');
      return;
    case 'reconnecting':
    case 'forceReconnect':
    case 'update':
    case 'ldm':
    case 'ping':
      logger.debug({ nats_status: status.type }, 'nats connection status');
      return;
  }
}

// Fails fast at startup if a stream the process needs doesn't exist (nats-setup not run).
export async function assertStreamExists(nc: NatsConnection, stream: string): Promise<void> {
  const jsm = await jetstreamManager(nc);
  try {
    await jsm.streams.info(stream);
  } catch (err) {
    throw new Error(`JetStream stream ${stream} not found; run the nats-setup service`, {
      cause: err,
    });
  }
}

export type PublishResult = {
  readonly stream: string;
  readonly seq: number;
  readonly duplicate: boolean;
};

// The transport layer: how an event gets onto NATS. Code that announces events depends on
// this type, not on JetStream, so the publisher is chosen where the app is wired together:
// jetStreamPublisher in src/api/main.ts, a recording or failing one in the tests
// (test/helpers.ts).
export type EventPublisher = {
  // Publishes a JSON event and waits for the stream to acknowledge it. `msgId` becomes the
  // Nats-Msg-Id header, which the stream uses to drop repeats within its duplicate window.
  publish(subject: string, event: object, msgId: string): Promise<PublishResult>;
};

export function jetStreamPublisher(
  nc: NatsConnection,
  options: { stream: string; timeoutMs: number },
): EventPublisher {
  const js = jetstream(nc);
  return {
    async publish(subject, event, msgId) {
      const ack = await js.publish(subject, JSON.stringify(event), {
        msgID: msgId,
        timeout: options.timeoutMs,
        // The server rejects the message unless it lands in this stream, so a misconfigured
        // subject fails loudly instead of being published to no stream.
        expect: { streamName: options.stream },
      });
      return { stream: ack.stream, seq: ack.seq, duplicate: ack.duplicate };
    },
  };
}

// Closes the connection on shutdown: drain() first, which lets pending publishes and
// in-flight messages finish, but only for `timeoutMs`. If NATS is unreachable the connection
// is reconnecting forever and drain() would never finish, so after the timeout (or if drain()
// fails, e.g. the connection is already closed) it is closed outright. Never throws.
export async function closeNats(
  nc: NatsConnection,
  timeoutMs: number,
): Promise<'drained' | 'closed'> {
  // Settles either way, so the race below can't leave an unhandled rejection behind.
  const drained = nc.drain().then(
    () => true,
    () => false,
  );
  // ref: false, so this timer alone doesn't keep the process alive.
  const timedOut = sleep(timeoutMs, false, { ref: false });
  if (await Promise.race([drained, timedOut])) return 'drained';
  if (!nc.isClosed()) await nc.close();
  return 'closed';
}
