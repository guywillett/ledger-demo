import { jetstreamManager } from '@nats-io/jetstream';
import { runConsumerProcess } from '../consumer/process.ts';
import { CONSUMERS, STREAMS } from '../shared/nats.ts';
import { assertTimeoutFitsAckWait, loadSubmitterConfig } from './config.ts';
import { handleSubmission, type SubmitterDeps } from './handler.ts';
import { providerClient } from './provider.ts';

// Entry point: node src/submitter/main.ts. Pulls withdrawal.held messages from the durable
// `submitter` consumer and sends each withdrawal to the provider, up to SUBMITTER_CONCURRENCY at
// once. The process lifecycle (connect, pull loop, signals, graceful shutdown, exit codes) is
// shared with the worker: src/consumer/process.ts. Several instances can share the consumer:
// JetStream gives each message to one of them. An unfinished submission is redelivered on
// shutdown, and asks again with the same key.

// After an unknown outcome or a transient failure: nak with 1 s, doubling per attempt up to 60 s.
const DELIVERY_RETRY = { retryBaseDelayMs: 1000, retryMaxDelayMs: 60_000 };

const config = loadSubmitterConfig();
const provider = providerClient({
  baseUrl: config.providerUrl,
  timeoutMs: config.providerTimeoutMs,
});

await runConsumerProcess({
  service: 'submitter',
  logLevel: config.logLevel,
  natsUrl: config.natsUrl,
  databaseUrl: config.databaseUrl,
  stream: STREAMS.withdrawals,
  consumer: CONSUMERS.submitter,
  concurrency: config.concurrency,
  // The consumer's real ack_wait (nanoseconds in NATS), so the timeout check can't drift from
  // nats/consumers/WITHDRAWALS/submitter.json. Throws if the consumer doesn't exist.
  startupCheck: async (nc) => {
    const info = await (
      await jetstreamManager(nc)
    ).consumers.info(STREAMS.withdrawals, CONSUMERS.submitter);
    // Unset means the server's default, 30 s.
    const ackWaitNs = info.config.ack_wait ?? 30_000_000_000;
    assertTimeoutFitsAckWait(config.providerTimeoutMs, ackWaitNs / 1e6);
  },
  makeHandler: ({ pool, logger }) => {
    const deps: SubmitterDeps = { pool, logger, provider, ...DELIVERY_RETRY };
    return (msg) => handleSubmission(deps, msg);
  },
  startedFields: { provider_timeout_ms: config.providerTimeoutMs },
});
