import { randomUUID } from 'node:crypto';
import Fastify, { LogController } from 'fastify';
import { z } from 'zod';
import { validationErrors } from '../api/problem.ts';
import type { App } from '../api/types.ts';
import { retryDelay, type Backoff } from '../shared/backoff.ts';
import type { Logger } from '../shared/logger.ts';
import {
  amountMinorSchema,
  ibanSchema,
  optionalSepaTextSchema,
  sepaTextSchema,
} from '../shared/schemas.ts';
import type { Rates } from './config.ts';

// provider-sim: a fake BaaS provider for the submitter and the transfer-update webhook to talk
// to (slice 2 decision 31: test infrastructure, part of neither implementation). It accepts
// SEPA transfers idempotently on Idempotency-Key and reports each one's result to a webhook
// later, with failures chosen by triggers in remittance_info or by random rates (decision 34).
//
// State lives in memory: a restart forgets every transfer (a Known gap).
//
// GET /_test/transfers is a test-only read of that state (slice 4 decision 73): for tests and
// scripts, to see what the provider did with a withdrawal. The system under test must never call
// it: the submitter learns an outcome from its call's answer and the webhook, as with a real
// provider.

// What the webhook receives. provider_event_id identifies the update: a duplicate delivery
// carries the same one.
export type TransferUpdate = {
  readonly provider_event_id: string;
  readonly transfer_id: string;
  readonly reference: string;
  readonly status: 'executed' | 'failed';
  readonly reason?: string;
  readonly occurred_at: string;
};

export type SimDeps = {
  readonly logger: Logger;
  readonly now: () => Date;
  // For the random failure rates, [0, 1).
  readonly random: () => number;
  // Sends one update to the webhook; throws unless it was answered with a 2xx. Undefined when no
  // webhook is configured: results are then logged, not sent.
  readonly deliver: ((update: TransferUpdate) => Promise<void>) | undefined;
  // Waits ms; settles early (either way) when `signal` aborts, on stop().
  readonly sleep: (ms: number, signal: AbortSignal) => Promise<void>;
  readonly executionDelayMs: number;
  readonly slowDelayMs: number;
  readonly webhookMaxAttempts: number;
  // Between failed webhook attempts: 1 s, doubling, at most 30 s by default.
  readonly webhookRetry?: Backoff;
  readonly rates: Rates;
};

export type SimApp = {
  readonly app: App;
  // Resolves once no result is scheduled or being delivered (for tests).
  idle(): Promise<void>;
  // Abandons every scheduled result and webhook retry; returns how many were abandoned.
  stop(): number;
};

// A behaviour chosen for a new transfer: a trigger at the start of remittance_info, or a
// random rate, or none. Each trigger is valid SEPA basic text, so it passes our API's checks.
type Behaviour = 'refuse' | 'error' | 'slow' | 'fail' | 'duplicate' | 'early';
const TRIGGERS: readonly (readonly [string, Behaviour])[] = [
  ['SIM REFUSE', 'refuse'],
  ['SIM ERROR', 'error'],
  ['SIM SLOW', 'slow'],
  ['SIM FAIL', 'fail'],
  ['SIM DUPLICATE', 'duplicate'],
  ['SIM EARLY', 'early'],
];

const REFUSAL_REASON = 'creditor account closed';
const FAILURE_REASON = 'rejected by the beneficiary bank';

// Validated like a real SEPA provider would: the same IBAN and SEPA text rules as our API.
// reference: our withdrawal TypeID, echoed back in updates; at most 35 characters, the SEPA
// end-to-end ID limit. A tolerant reader (z.object), as providers are.
const transferBodySchema = z.object({
  amount_minor: amountMinorSchema,
  currency: z.literal('EUR'),
  creditor_iban: ibanSchema,
  creditor_name: sepaTextSchema(70),
  remittance_info: optionalSepaTextSchema(140),
  reference: z.string().min(1).max(35),
});
type TransferRequest = z.infer<typeof transferBodySchema>;

const headersSchema = z.object({
  'idempotency-key': z.string().regex(/^[A-Za-z0-9._:-]{1,255}$/),
});

type Transfer = {
  readonly transferId: string;
  readonly request: TransferRequest;
  readonly outcome: { readonly status: 'executed' } | { readonly status: 'failed'; reason: string };
  readonly duplicate: boolean;
};

// GET /_test/transfers's query: the reference, by POST /transfers's rule.
const testTransferQuerySchema = z.object({ reference: transferBodySchema.shape.reference });

// What GET /_test/transfers needs for a reference: how many handled requests have carried it, and
// the key of the first. Only that key is followed: a transfer created for the reference under a
// later, second key doesn't show (the submitters send key = reference, so it can't happen here).
type ReferenceSeen = {
  count: number;
  readonly key: string;
};

export function buildSimApp(deps: SimDeps): SimApp {
  const backoff = deps.webhookRetry ?? { retryBaseDelayMs: 1000, retryMaxDelayMs: 30_000 };
  const transfers = new Map<string, Transfer>();
  // Keys whose first submission was answered 503 (SIM ERROR), with that request's reference: the
  // retry succeeds.
  const errored = new Map<string, string>();
  // Refused requests by key: a refusal is permanent, so the same request is refused again
  // (without re-rolling SIM_REFUSE_RATE), and another request with the key is a 409.
  const refused = new Map<string, TransferRequest>();
  // Scheduled results and deliveries in progress, so idle() and stop() can reach them.
  const pending = new Set<Promise<void>>();
  const stopping = new AbortController();
  // By reference: the requests handled (any answer but a 400) for GET /_test/transfers.
  const requests = new Map<string, ReferenceSeen>();

  // No per-request log lines: the sim logs its own one line per transfer and delivery.
  const app: App = Fastify({
    loggerInstance: deps.logger,
    logController: new LogController({ disableRequestLogging: true }),
  });

  // Test only endpoint (decision 73): only tests and scripts call it. The system under test never
  // calls this.
  app.get('/_test/transfers', async (request, reply) => {
    const query = testTransferQuerySchema.safeParse(request.query);
    if (!query.success) {
      const errors = validationErrors(query.error);
      deps.logger.info({ errors }, 'invalid _test transfer request');
      return reply.code(400).send({ error: 'invalid _test transfer request', errors });
    }
    const reference = query.data.reference;
    const seen = requests.get(reference);
    if (seen === undefined) {
      return reply.code(404).send({ error: 'reference not found' });
    }
    const key = seen.key;
    // The key's transfer, refusal and 503 only if they were this reference's: a 409 can carry a
    // new reference under another reference's key.
    const stored = transfers.get(key);
    const transfer = stored?.request.reference === reference ? stored : undefined;
    const transferView =
      transfer === undefined
        ? null
        : {
            transfer_id: transfer.transferId,
            amount_minor: transfer.request.amount_minor.toString(),
            currency: transfer.request.currency,
            creditor_iban: transfer.request.creditor_iban,
            creditor_name: transfer.request.creditor_name,
            remittance_info: transfer.request.remittance_info,
            outcome: transfer.outcome.status,
            failure_reason: transfer.outcome.status === 'failed' ? transfer.outcome.reason : null,
            reports_twice: transfer.duplicate,
          };
    return reply.code(200).send({
      reference,
      requests: seen.count,
      transfer: transferView,
      refused: refused.get(key)?.reference === reference,
      errored: errored.get(key) === reference,
    });
  });

  app.post('/transfers', async (request, reply) => {
    const headers = headersSchema.safeParse(request.headers);
    const body = transferBodySchema.safeParse(request.body);
    if (!headers.success || !body.success) {
      // Path and message of each problem, never the input values (as our API's 400s).
      const errors = [
        ...(headers.success ? [] : validationErrors(headers.error)),
        ...(body.success ? [] : validationErrors(body.error)),
      ];
      deps.logger.info({ errors }, 'invalid transfer request');
      return reply.code(400).send({ error: 'invalid transfer request', errors });
    }
    const key = headers.data['idempotency-key'];
    const req = body.data;
    // A handled request, counted for its reference: 409s, 422s, 503s, repeats and creations all
    // count. The key is the first request's.
    const seen = requests.get(req.reference);
    if (seen === undefined) {
      requests.set(req.reference, { count: 1, key });
    } else {
      seen.count += 1;
    }

    // The reads and the writes below have no await between them. Node runs one callback at a
    // time on its event loop, so no other request can run in between: two concurrent requests
    // with one key can't both create a transfer, with no lock needed.
    const refusal = refused.get(key);
    if (refusal !== undefined) {
      if (!sameRequest(refusal, req)) {
        deps.logger.warn({ reference: req.reference }, 'idempotency key reused');
        return reply.code(409).send({ error: 'Idempotency-Key reused with a different request' });
      }
      deps.logger.info({ reference: req.reference, reason: REFUSAL_REASON }, 'transfer refused');
      return reply.code(422).send({ reason: REFUSAL_REASON });
    }
    const existing = transfers.get(key);
    if (existing !== undefined) {
      if (!sameRequest(existing.request, req)) {
        deps.logger.warn({ transfer_id: existing.transferId }, 'idempotency key reused');
        return reply.code(409).send({ error: 'Idempotency-Key reused with a different request' });
      }
      deps.logger.info(
        { transfer_id: existing.transferId, reference: req.reference },
        'repeated transfer request',
      );
      return reply.code(200).send({ transfer_id: existing.transferId, status: 'accepted' });
    }

    const behaviour = chooseBehaviour(req);
    if (behaviour === 'refuse') {
      refused.set(key, req);
      deps.logger.info({ reference: req.reference, reason: REFUSAL_REASON }, 'transfer refused');
      return reply.code(422).send({ reason: REFUSAL_REASON });
    }
    if (behaviour === 'error' && !errored.has(key)) {
      errored.set(key, req.reference);
      deps.logger.info({ reference: req.reference }, 'transfer request answered 503 (simulated)');
      return reply.code(503).send({ error: 'temporarily unavailable' });
    }

    const transfer: Transfer = {
      transferId: `tr_${randomUUID()}`,
      request: req,
      outcome:
        behaviour === 'fail'
          ? { status: 'failed', reason: FAILURE_REASON }
          : { status: 'executed' },
      duplicate: behaviour === 'duplicate',
    };
    transfers.set(key, transfer);
    // scripts/demo.sh (step 13) and the e2e tests read this line (its msg and reference fields).
    deps.logger.info(
      { transfer_id: transfer.transferId, reference: req.reference, behaviour: behaviour ?? null },
      'transfer created',
    );

    if (behaviour === 'early') {
      // One delivery attempt before this response, so the result can reach the webhook first:
      // the race of decision 27. Should that attempt fail, the retries carry on in the
      // background; the response doesn't wait for them, or a failing webhook would turn the race
      // into an unknown outcome (the submitter timing out).
      const update = resultUpdate(transfer);
      if (!(await attemptDelivery(update, 1))) void track(sendWithRetries(update, 2));
    } else {
      void track(
        deps
          .sleep(deps.executionDelayMs, stopping.signal)
          .then(() => (stopping.signal.aborted ? undefined : reportResult(transfer))),
      );
    }
    if (behaviour === 'slow') {
      // The transfer exists now; the answer comes after the submitter has given up waiting.
      await deps.sleep(deps.slowDelayMs, stopping.signal);
    }
    return reply.code(201).send({ transfer_id: transfer.transferId, status: 'accepted' });
  });

  // A trigger wins; otherwise each random rate in turn.
  function chooseBehaviour(req: TransferRequest): Behaviour | undefined {
    const text = req.remittance_info ?? '';
    const triggered = TRIGGERS.find(([prefix]) => text.startsWith(prefix));
    if (triggered !== undefined) return triggered[1];
    const rates: readonly (readonly [number, Behaviour])[] = [
      [deps.rates.refuse, 'refuse'],
      [deps.rates.error, 'error'],
      [deps.rates.slow, 'slow'],
      [deps.rates.fail, 'fail'],
      [deps.rates.duplicate, 'duplicate'],
      [deps.rates.early, 'early'],
    ];
    return rates.find(([rate]) => rate > 0 && deps.random() < rate)?.[1];
  }

  function resultUpdate(transfer: Transfer): TransferUpdate {
    return {
      provider_event_id: `pev_${randomUUID()}`,
      transfer_id: transfer.transferId,
      reference: transfer.request.reference,
      ...transfer.outcome,
      occurred_at: deps.now().toISOString(),
    };
  }

  // Reports the transfer's result: once, or twice with the same provider_event_id.
  async function reportResult(transfer: Transfer): Promise<void> {
    const update = resultUpdate(transfer);
    await sendWithRetries(update, 1);
    if (transfer.duplicate) await sendWithRetries(update, 1);
  }

  // Attempts `from` up to the maximum, waiting the backoff before each one after the first.
  async function sendWithRetries(update: TransferUpdate, from: number): Promise<void> {
    for (let attempt = from; attempt <= deps.webhookMaxAttempts; attempt++) {
      if (attempt > 1) await deps.sleep(retryDelay(backoff, attempt - 1), stopping.signal);
      if (stopping.signal.aborted) return;
      if (await attemptDelivery(update, attempt)) return;
    }
    deps.logger.error(
      { ...logFields(update), attempts: deps.webhookMaxAttempts },
      'gave up delivering the transfer result',
    );
  }

  // One attempt; true if it is done (delivered, or there is no webhook to deliver to).
  async function attemptDelivery(update: TransferUpdate, attempt: number): Promise<boolean> {
    const fields = logFields(update);
    if (deps.deliver === undefined) {
      deps.logger.info(fields, 'transfer result (not sent: SIM_WEBHOOK_URL unset)');
      return true;
    }
    try {
      await deps.deliver(update);
      deps.logger.info({ ...fields, attempt }, 'transfer result delivered');
      return true;
    } catch (err) {
      deps.logger.warn({ ...fields, attempt, err }, 'transfer result delivery failed');
      return false;
    }
  }

  function track(work: Promise<void>): Promise<void> {
    const tracked: Promise<void> = work
      .catch((err: unknown) => {
        deps.logger.error({ err }, 'unexpected error reporting a transfer result');
      })
      .finally(() => pending.delete(tracked));
    pending.add(tracked);
    return tracked;
  }

  return {
    app,
    idle: async () => {
      while (pending.size > 0) await Promise.allSettled(pending);
    },
    stop: () => {
      const abandoned = pending.size;
      stopping.abort();
      return abandoned;
    },
  };
}

// The same request field by field (amounts are BigInts, so not a JSON comparison).
function sameRequest(a: TransferRequest, b: TransferRequest): boolean {
  return (
    a.amount_minor === b.amount_minor &&
    a.currency === b.currency &&
    a.creditor_iban === b.creditor_iban &&
    a.creditor_name === b.creditor_name &&
    a.remittance_info === b.remittance_info &&
    a.reference === b.reference
  );
}

function logFields(update: TransferUpdate): Record<string, string> {
  return { transfer_id: update.transfer_id, reference: update.reference, status: update.status };
}
