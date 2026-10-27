import { decideRandomly } from '../compliance/decision.ts';
import { DELIVERY_RETRY } from '../consumer/delivery-log.ts';
import { runConsumerProcess } from '../consumer/process.ts';
import { CONSUMERS, STREAMS } from '../shared/nats.ts';
import { loadDepositWorkerConfig } from './config.ts';
import { handleDelivery, type HandlerDeps } from './handler.ts';

// The deposit-worker: named for its flow, with a generic role (ADR 0002). It runs the whole
// deposit lifecycle (book, review, post the decision) as one consumer for now. The compliance
// review is the natural first thing to split into its own consumer, and the name stays true
// after that: whatever remains still works on deposits.
//
// Entry point: node src/deposit-worker/main.ts. Pulls deposit.created messages from the durable
// `deposit-worker` consumer (created by nats-setup from nats/consumers/) and handles up to
// DEPOSIT_WORKER_CONCURRENCY of them at once. The process lifecycle (connect, pull loop,
// signals, graceful shutdown, exit codes) is shared with the submitter: src/consumer/process.ts.
// Several instances can share the consumer: JetStream gives each message to one of them.

// working() at least this often during a review: well under the consumer's 10 s ack_wait.
const WORKING_INTERVAL_MS = 3000;
// A transient failure handling a message is retried with DELIVERY_RETRY (consumer/delivery-log.ts).

const config = loadDepositWorkerConfig();

await runConsumerProcess({
  service: 'deposit-worker',
  logLevel: config.logLevel,
  natsUrl: config.natsUrl,
  databaseUrl: config.databaseUrl,
  stream: STREAMS.deposits,
  consumer: CONSUMERS.depositWorker,
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
