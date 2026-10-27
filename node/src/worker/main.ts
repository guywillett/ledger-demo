import { decideRandomly } from '../compliance/decision.ts';
import { DELIVERY_RETRY } from '../consumer/delivery-log.ts';
import { runConsumerProcess } from '../consumer/process.ts';
import { CONSUMERS, STREAMS } from '../shared/nats.ts';
import { loadWorkerConfig } from './config.ts';
import { handleDelivery, type HandlerDeps } from './handler.ts';

// Entry point: node src/worker/main.ts. Pulls deposit.created messages from the durable
// `worker` consumer (created by nats-setup from nats/consumers/) and handles up to
// WORKER_CONCURRENCY of them at once. The process lifecycle (connect, pull loop, signals,
// graceful shutdown, exit codes) is shared with the submitter: src/consumer/process.ts. Several
// instances can share the consumer: JetStream gives each message to one of them.

// working() at least this often during a review: well under the consumer's 10 s ack_wait.
const WORKING_INTERVAL_MS = 3000;
// A transient failure handling a message is retried with DELIVERY_RETRY (consumer/delivery-log.ts).

const config = loadWorkerConfig();

await runConsumerProcess({
  service: 'worker',
  logLevel: config.logLevel,
  natsUrl: config.natsUrl,
  databaseUrl: config.databaseUrl,
  stream: STREAMS.deposits,
  consumer: CONSUMERS.worker,
  concurrency: config.concurrency,
  makeHandler: ({ pool, logger }) => {
    const deps: HandlerDeps = {
      pool,
      logger,
      decide: () => decideRandomly(config.approveRate),
      reviewDelayMs: config.reviewDelayMs,
      workingIntervalMs: WORKING_INTERVAL_MS,
      ...DELIVERY_RETRY,
    };
    return (msg) => handleDelivery(deps, msg);
  },
});
