import { randomUUID } from 'node:crypto';
import { Writable } from 'node:stream';
import type pg from 'pg';
import { buildApp } from '../src/api/app.ts';
import type { App } from '../src/api/types.ts';
import { loadSharedConfig, natsEnvSchema, parseEnv } from '../src/shared/config.ts';
import { decide, startReview } from '../src/compliance/transitions.ts';
import { recordNotification } from '../src/ingestion/notifications.ts';
import { book, postDecision } from '../src/ledger/posting.ts';
import { type Payment } from '../src/payments/payments.ts';
import { relayBatch, type RelayDeps } from '../src/relay/relay.ts';
import { createPool, withTransaction } from '../src/shared/db.ts';
import { parseIban, type Iban } from '../src/shared/iban.ts';
import { newUuid, toTypeId, type TypeIdString, type Uuid } from '../src/shared/ids.ts';
import {
  connectNats,
  jetStreamPublisher,
  PUBLISH_TIMEOUT_MS,
  type EventPublisher,
  type NatsConnection,
  type PublishResult,
} from '../src/shared/nats.ts';
import { createLogger, type Logger } from '../src/shared/logger.ts';
import { requestWithdrawal } from '../src/withdrawals/requests.ts';
import type { Withdrawal } from '../src/withdrawals/withdrawals.ts';
import { expectEventMatchesContract, expectMatchesContract } from './contract.ts';

// Seeded demo accounts (db/migrations/20261018000002_seed_demo_accounts.sql).
export const ALICE = {
  id: 'acct_01m564xr00fx7rjvqryn5dzwxx',
  iban: 'FR7612345000010000000000193',
} as const;
export const BOB = {
  id: 'acct_01m564xr01ecrvnckn84evqsax',
  iban: 'FR7612345000010000000000290',
} as const;
export const CAROL = {
  id: 'acct_01m564xr02e70a25p2hkva3gbn',
  iban: 'FR7612345000010000000000387',
} as const;
// Valid (checksum-correct) IBANs that are not ours.
export const FOREIGN_IBAN = 'DE75512108001245126199';

// Every test uses its own network transaction IDs (and its own accounts) and never deletes
// anything, so test files can run in parallel without interfering.
export function newNetworkTransactionId(): string {
  return `test-${randomUUID()}`;
}

// A POST /payments body (the wire format), with a fresh network transaction ID.
export function notificationBody(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    network_transaction_id: newNetworkTransactionId(),
    creditor_iban: ALICE.iban,
    amount_minor: '1250',
    currency: 'EUR',
    network: 'SEPA',
    ...overrides,
  };
}

export type TestApp = {
  app: App;
  pool: pg.Pool;
  close: () => Promise<void>;
};

// The app against the compose Postgres. No NATS: the API only writes events to the outbox.
export async function createTestApp(options: { logger?: Logger } = {}): Promise<TestApp> {
  const config = loadSharedConfig();
  const log = options.logger ?? createLogger({ service: 'test', level: config.logLevel });
  const pool = createPool({ databaseUrl: config.databaseUrl, logger: log });
  const app = buildApp({ pool, logger: log });
  return {
    app,
    pool,
    close: async () => {
      await app.close();
      await pool.end();
    },
  };
}

// The test NATS (nats-test), for the relay tests and the e2e tests.
export async function connectTestNats(): Promise<NatsConnection> {
  const { NATS_URL } = parseEnv(natsEnvSchema);
  return connectNats({
    url: NATS_URL,
    name: 'ledger-demo-test',
    logger: createLogger({ service: 'test', level: loadSharedConfig().logLevel }),
  });
}

// The relay's real publisher, on the test streams.
export function testStreamPublisher(nc: NatsConnection): EventPublisher {
  return jetStreamPublisher(nc, { timeoutMs: PUBLISH_TIMEOUT_MS });
}

export type RecordedEvent = {
  readonly subject: string;
  readonly event: Record<string, unknown>;
  readonly msgId: string;
  readonly result: PublishResult;
};

// Wraps a publisher, checks every event against its contract and records what was published,
// so tests can assert exactly which events the relay published.
export function recordingPublisher(publisher: EventPublisher): {
  publisher: EventPublisher;
  published: RecordedEvent[];
} {
  const published: RecordedEvent[] = [];
  return {
    published,
    publisher: {
      async publish(subject, event, msgId) {
        expectEventMatchesContract(event);
        const result = await publisher.publish(subject, event, msgId);
        published.push({ subject, event: event as Record<string, unknown>, msgId, result });
        return result;
      },
    },
  };
}

// Relay dependencies for tests: a big batch, a 30 s lease and a fixed retry delay unless
// overridden.
export function testRelayDeps(
  pool: pg.Pool,
  publisher: EventPublisher,
  overrides: Partial<RelayDeps> = {},
): RelayDeps {
  return {
    pool,
    publisher,
    logger: createLogger({ service: 'test', level: loadSharedConfig().logLevel }),
    batchSize: 100,
    leaseMs: 30_000,
    retryDelay: () => 1000,
    ...overrides,
  };
}

// Publishes the outbox's backlog until a batch claims nothing: the unpublished events other test
// files left behind (every created payment has one). Only for the tests that run last and one
// file at a time (the "relay" project in vitest.config.ts), so nothing else adds to the backlog
// meanwhile. Events whose lease hasn't expired (e.g. backed off after a simulated failure) can't
// be claimed and stay unpublished: tests assert only on their own events.
export async function drainOutbox(pool: pg.Pool, publisher: EventPublisher): Promise<void> {
  const deps = testRelayDeps(pool, publisher);
  while ((await relayBatch(deps)).claimed > 0) {
    // Keep going.
  }
}

export type OutboxRow = {
  id: Uuid<'evt'>;
  entity_type: string;
  entity_id: string;
  subject: string;
  msg_id: string;
  payload: Record<string, unknown>;
  attempts: number;
  claimed_until: Date | null;
  last_error: string | null;
  published_at: Date | null;
};

// The outbox rows for one payment or withdrawal.
export async function outboxRowsFor(
  pool: pg.Pool,
  entityId: Uuid<'pay'> | Uuid<'wd'>,
): Promise<OutboxRow[]> {
  const result = await pool.query<OutboxRow>(
    `SELECT id, entity_type, entity_id, subject, msg_id, payload, attempts, claimed_until,
            last_error, published_at
     FROM outbox WHERE entity_id = $1 ORDER BY id`,
    [entityId],
  );
  return result.rows;
}

export type JsonResponse = {
  statusCode: number;
  headers: Record<string, string | string[] | number | undefined>;
  body: string;
  json: Record<string, unknown>;
};

// POSTs a notification and checks the response against contracts/openapi.yaml. A string
// payload is sent as-is (e.g. malformed JSON); anything else is JSON-encoded.
export async function postPayment(
  app: App,
  payload: unknown,
  headers: Record<string, string> = {},
): Promise<JsonResponse> {
  const response = await app.inject({
    method: 'POST',
    url: '/payments',
    headers: { 'content-type': 'application/json', ...headers },
    payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
  });
  expectMatchesContract('POST', '/payments', response);
  return {
    statusCode: response.statusCode,
    headers: response.headers,
    body: response.body,
    json: JSON.parse(response.body) as Record<string, unknown>,
  };
}

// POSTs a withdrawal request for the account (an acct_ TypeID) with an Idempotency-Key (none if
// undefined) and checks the response against the contract.
export async function postWithdrawal(
  app: App,
  accountTypeId: string,
  payload: unknown,
  idempotencyKey: string | undefined,
): Promise<JsonResponse> {
  const response = await app.inject({
    method: 'POST',
    url: `/accounts/${accountTypeId}/withdrawals`,
    headers: {
      'content-type': 'application/json',
      ...(idempotencyKey === undefined ? {} : { 'idempotency-key': idempotencyKey }),
    },
    payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
  });
  expectMatchesContract('POST', '/accounts/{account_id}/withdrawals', response);
  return {
    statusCode: response.statusCode,
    headers: response.headers,
    body: response.body,
    json: JSON.parse(response.body) as Record<string, unknown>,
  };
}

// A withdrawal request body (the wire format).
export function withdrawalBody(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    amount_minor: '400',
    currency: 'EUR',
    creditor_iban: FOREIGN_IBAN,
    creditor_name: 'Jane Doe',
    remittance_info: 'Rent October',
    ...overrides,
  };
}

// GETs `url` and checks the response against the contract operation at `contractPath` (the
// path template, e.g. '/accounts/{account_id}/balance').
export async function getJson(app: App, url: string, contractPath: string): Promise<JsonResponse> {
  const response = await app.inject({ method: 'GET', url });
  expectMatchesContract('GET', contractPath, response);
  return {
    statusCode: response.statusCode,
    headers: response.headers,
    body: response.body,
    json: JSON.parse(response.body) as Record<string, unknown>,
  };
}

// A logger whose JSON lines are collected instead of written to stdout.
export function capturingLogger(): { logger: Logger; lines: () => Record<string, unknown>[] } {
  const chunks: string[] = [];
  const destination = new Writable({
    write(chunk, _encoding, callback) {
      chunks.push(String(chunk));
      callback();
    },
  });
  const lines = (): Record<string, unknown>[] =>
    chunks
      .join('')
      .split('\n')
      .filter((line) => line !== '')
      .map((line) => JSON.parse(line) as Record<string, unknown>);
  return { logger: createLogger({ service: 'test', level: 'info' }, destination), lines };
}

// A stub pool for recordNotification's refused path, used through withTransaction: BEGIN,
// COMMIT and ROLLBACK succeed; then the upsert returns no row (nothing recorded), and the
// follow-up read returns `stored`. For cases the real database can't produce without changing
// shared seed data.
export function refusingPool(stored: {
  creditor_iban: string;
  amount_minor: bigint;
  currency: string;
}): pg.Pool {
  const responses = [{ rows: [] }, { rows: [{ id: newUuid('pay'), ...stored }] }];
  const client = {
    query: async (sql: string) => {
      if (/^(BEGIN|COMMIT|ROLLBACK)/.test(sql)) return { rows: [] };
      const next = responses.shift();
      if (next === undefined) throw new Error('unexpected extra query');
      return next;
    },
    release: () => undefined,
  };
  const pool = { connect: async () => client, end: async () => undefined };
  return pool as unknown as pg.Pool;
}

// The pool, except that completing a delivery-log row (finishDelivery's UPDATE) fails, as when
// the database goes away after the work: for the signal-vs-record ordering in the handlers.
export function failingDeliveryFinish(pool: pg.Pool): pg.Pool {
  const failing = {
    connect: pool.connect.bind(pool),
    query: (sql: unknown, params?: unknown) => {
      if (typeof sql === 'string' && sql.includes('UPDATE event_deliveries')) {
        return Promise.reject(new Error('database unavailable'));
      }
      return (pool.query as (sql: unknown, params?: unknown) => Promise<unknown>)(sql, params);
    },
  };
  return failing as unknown as pg.Pool;
}

// A publisher that always fails, as when NATS is down or the publish times out.
export const failingPublisher: EventPublisher = {
  publish: async () => {
    throw new Error('simulated publish failure');
  },
};

// A pool on the test database, for tests that call data-access functions directly.
export function createTestPool(): pg.Pool {
  const config = loadSharedConfig();
  return createPool({
    databaseUrl: config.databaseUrl,
    logger: createLogger({ service: 'test', level: config.logLevel }),
  });
}

// A fresh, valid French IBAN (made-up bank 99999, random account number): correct RIB key and
// IBAN check digits, so parseIban accepts it. Random, so every test account gets its own.
export function newTestIban(): Iban {
  const bank = '99999';
  const branch = '00001';
  const account = String(Math.floor(Math.random() * 1e11)).padStart(11, '0');
  const ribKey = 97n - ((89n * BigInt(bank) + 15n * BigInt(branch) + 3n * BigInt(account)) % 97n);
  const bban = `${bank}${branch}${account}${ribKey.toString().padStart(2, '0')}`;
  // Check digits: 98 - (BBAN + "FR00", letters as numbers) mod 97. F = 15, R = 27.
  const checkDigits = 98n - (BigInt(`${bban}152700`) % 97n);
  const result = parseIban(`FR${checkDigits.toString().padStart(2, '0')}${bban}`);
  if (!result.ok) throw new Error(`generated an invalid test IBAN: ${result.reason}`);
  return result.iban;
}

export type TestAccount = {
  readonly id: Uuid<'acct'>;
  readonly typeId: TypeIdString<'acct'>;
  readonly iban: Iban;
};

// An account of the test's own (with the zero balance row the database trigger adds), so
// balance assertions don't depend on other tests. Never deleted: the test database is reset as
// a whole (npm run test:db:reset).
export async function createTestAccount(pool: pg.Pool): Promise<TestAccount> {
  const id = newUuid('acct');
  const iban = newTestIban();
  await pool.query('INSERT INTO accounts (id, name, iban) VALUES ($1, $2, $3)', [
    id,
    `test-${id}`,
    iban,
  ]);
  return { id, typeId: toTypeId('acct', id), iban };
}

// A payment in status `created` for the account, recorded through ingestion (the real path).
export async function createTestPayment(
  pool: pg.Pool,
  account: TestAccount,
  amountMinor = 1250n,
): Promise<Payment> {
  const notification = {
    creditorIban: account.iban,
    providerAccountId: null,
    amountMinor,
    currency: 'EUR',
    network: 'SEPA',
    networkTransactionId: newNetworkTransactionId(),
  } as const;
  const result = await withTransaction(pool, (tx) => recordNotification(tx, notification));
  if (result.kind !== 'created') throw new Error(`test payment not created: ${result.kind}`);
  return result.payment;
}

// Puts `amountMinor` into the account's available balance the real way: a payment booked,
// reviewed, approved and credited. So the account stays consistent for reconciliation.
export async function fundTestAccount(
  pool: pg.Pool,
  account: TestAccount,
  amountMinor: bigint,
): Promise<Payment> {
  const payment = await createTestPayment(pool, account, amountMinor);
  await withTransaction(pool, (tx) => book(tx, payment.id));
  await withTransaction(pool, (tx) => startReview(tx, payment.id));
  await withTransaction(pool, (tx) => decide(tx, payment.id, { outcome: 'approved' }));
  await withTransaction(pool, (tx) => postDecision(tx, payment.id));
  return payment;
}

// A withdrawal in `held` with its funds reserved and its withdrawal.held event in the outbox, as
// the API creates it (requestWithdrawal in its own transaction). Throws InsufficientFundsError
// (and stores nothing) if the account is short.
export async function createTestWithdrawal(
  pool: pg.Pool,
  account: TestAccount,
  amountMinor: bigint,
  idempotencyKey = randomUUID(),
): Promise<Withdrawal> {
  return withTransaction(pool, async (tx) => {
    const result = await requestWithdrawal(tx, {
      accountId: account.id,
      idempotencyKey,
      amountMinor,
      currency: 'EUR',
      creditorIban: newTestIban(),
      creditorName: 'Jane Doe',
      remittanceInfo: 'test withdrawal',
    });
    if (result.kind !== 'created') throw new Error(`idempotency key ${idempotencyKey} is taken`);
    return result.withdrawal;
  });
}

export async function balancesOf(
  pool: pg.Pool,
  accountId: Uuid<'acct'>,
): Promise<{ pendingIn: bigint; available: bigint; held: bigint }> {
  const result = await pool.query<{ pending_in: bigint; available: bigint; held: bigint }>(
    'SELECT pending_in, available, held FROM account_balances WHERE account_id = $1',
    [accountId],
  );
  const row = result.rows[0];
  if (row === undefined) throw new Error(`no balance row for ${accountId}`);
  return { pendingIn: row.pending_in, available: row.available, held: row.held };
}

export async function statusOf(pool: pg.Pool, paymentId: Uuid<'pay'>): Promise<string> {
  const result = await pool.query<{ status: string }>(
    'SELECT status FROM incoming_payments WHERE id = $1',
    [paymentId],
  );
  return result.rows[0]?.status ?? 'missing';
}
