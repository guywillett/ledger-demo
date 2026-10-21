import { randomUUID } from 'node:crypto';
import { Writable } from 'node:stream';
import type pg from 'pg';
import { buildApp } from '../src/api/app.ts';
import { paymentEvents, type PaymentEvents } from '../src/ingestion/events.ts';
import type { App } from '../src/api/types.ts';
import { loadSharedConfig } from '../src/shared/config.ts';
import { recordNotification, type Payment } from '../src/ingestion/payments.ts';
import { createPool, withTransaction, type Queryable } from '../src/shared/db.ts';
import { parseIban, type Iban } from '../src/shared/iban.ts';
import { newUuid, toTypeId, type TypeIdString, type Uuid } from '../src/shared/ids.ts';
import {
  connectNats,
  jetStreamPublisher,
  PUBLISH_TIMEOUT_MS,
  STREAMS,
  type EventPublisher,
  type NatsConnection,
  type PublishResult,
} from '../src/shared/nats.ts';
import { createLogger, type Logger } from '../src/shared/logger.ts';
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

// Tests share the development database, so every test uses its own network transaction IDs
// and never deletes anything: test files run in parallel without interfering.
export function newNetworkTransactionId(): string {
  return `test-${randomUUID()}`;
}

export function notification(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    network_transaction_id: newNetworkTransactionId(),
    creditor_iban: ALICE.iban,
    amount_minor: '1250',
    currency: 'EUR',
    network: 'SEPA',
    ...overrides,
  };
}

export type RecordedEvent = {
  readonly subject: string;
  readonly event: Record<string, unknown>;
  readonly msgId: string;
  readonly result: PublishResult;
};

// Wraps a publisher, checks every event against its contract and records what was published,
// so tests can assert exactly which events a request produced (including "none").
function recording(publisher: EventPublisher, published: RecordedEvent[]): EventPublisher {
  return {
    async publish(subject, event, msgId) {
      expectEventMatchesContract(event);
      const result = await publisher.publish(subject, event, msgId);
      published.push({ subject, event: event as Record<string, unknown>, msgId, result });
      return result;
    },
  };
}

export type TestApp = {
  app: App;
  pool: pg.Pool;
  nc: NatsConnection;
  // Events published through this app, in order.
  published: RecordedEvent[];
  close: () => Promise<void>;
};

// The app against the compose Postgres and NATS. `publisher` replaces the real JetStream
// publisher (e.g. one that fails); it is still recorded and contract-checked.
export async function createTestApp(
  options: { logger?: Logger; publisher?: EventPublisher } = {},
): Promise<TestApp> {
  const config = loadSharedConfig();
  const log = options.logger ?? createLogger({ service: 'test', level: config.logLevel });
  // NATS first: if it isn't running, fail before anything else is created.
  const nc = await connectNats({ url: config.natsUrl, name: 'ledger-demo-test', logger: log });
  const pool = createPool({ databaseUrl: config.databaseUrl, logger: log });
  const published: RecordedEvent[] = [];
  const publisher =
    options.publisher ??
    jetStreamPublisher(nc, { stream: STREAMS.payments, timeoutMs: PUBLISH_TIMEOUT_MS });
  const app = buildApp({
    db: pool,
    events: paymentEvents(recording(publisher, published)),
    logger: log,
  });
  return {
    app,
    pool,
    nc,
    published,
    close: async () => {
      await app.close();
      await nc.drain();
      await pool.end();
    },
  };
}

// The events published for one payment (tests share an app, so filter by payment).
export function eventsFor(testApp: TestApp, paymentId: unknown): RecordedEvent[] {
  return testApp.published.filter((recorded) => recorded.event.payment_id === paymentId);
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

// A stub Queryable for recordNotification's refused path: the upsert returns no row (nothing
// recorded), then the follow-up read returns `stored`. For cases the real database can't
// produce without changing shared seed data.
export function refusingDb(stored: {
  creditor_iban: string;
  amount_minor: bigint;
  currency: string;
}): Queryable {
  const responses = [{ rows: [] }, { rows: [{ id: newUuid('pay'), ...stored }] }];
  const db = {
    query: async () => {
      const next = responses.shift();
      if (next === undefined) throw new Error('unexpected extra query');
      return next;
    },
  };
  return db as unknown as Queryable;
}

// For apps whose requests must never publish (e.g. the stubbed refused-notification path).
export const unusedEvents: PaymentEvents = {
  paymentCreated: async () => {
    throw new Error('unexpected publish');
  },
};

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

// An account of the test's own, with a zero balance row, so balance assertions don't depend
// on other tests. Never deleted: the test database is reset as a whole (npm run test:db:reset).
export async function createTestAccount(pool: pg.Pool): Promise<TestAccount> {
  const id = newUuid('acct');
  const iban = newTestIban();
  await withTransaction(pool, async (tx) => {
    await tx.query('INSERT INTO accounts (id, name, iban) VALUES ($1, $2, $3)', [
      id,
      `test-${id}`,
      iban,
    ]);
    await tx.query('INSERT INTO account_balances (account_id) VALUES ($1)', [id]);
  });
  return { id, typeId: toTypeId('acct', id), iban };
}

// A payment in status `created` for the account, recorded through ingestion (the real path).
export async function createTestPayment(
  pool: pg.Pool,
  account: TestAccount,
  amountMinor = 1250n,
): Promise<Payment> {
  const result = await recordNotification(pool, {
    creditorIban: account.iban,
    providerAccountId: null,
    amountMinor,
    currency: 'EUR',
    network: 'SEPA',
    networkTransactionId: newNetworkTransactionId(),
  });
  if (result.kind !== 'created') throw new Error(`test payment not created: ${result.kind}`);
  return result.payment;
}

export async function balancesOf(
  pool: pg.Pool,
  accountId: Uuid<'acct'>,
): Promise<{ pendingIn: bigint; available: bigint }> {
  const result = await pool.query<{ pending_in: bigint; available: bigint }>(
    'SELECT pending_in, available FROM account_balances WHERE account_id = $1',
    [accountId],
  );
  const row = result.rows[0];
  if (row === undefined) throw new Error(`no balance row for ${accountId}`);
  return { pendingIn: row.pending_in, available: row.available };
}

export async function statusOf(pool: pg.Pool, paymentId: Uuid<'pay'>): Promise<string> {
  const result = await pool.query<{ status: string }>(
    'SELECT status FROM incoming_payments WHERE id = $1',
    [paymentId],
  );
  return result.rows[0]?.status ?? 'missing';
}
