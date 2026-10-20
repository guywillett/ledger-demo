import { randomUUID } from 'node:crypto';
import { Writable } from 'node:stream';
import type pg from 'pg';
import { buildApp } from '../src/api/app.ts';
import type { App } from '../src/api/types.ts';
import { loadSharedConfig } from '../src/shared/config.ts';
import { createPool, type Queryable } from '../src/shared/db.ts';
import { newUuid } from '../src/shared/ids.ts';
import { createLogger, type Logger } from '../src/shared/logger.ts';
import { expectMatchesContract } from './contract.ts';

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

export type TestApp = {
  app: App;
  pool: pg.Pool;
  close: () => Promise<void>;
};

export function createTestApp(logger?: Logger): TestApp {
  const config = loadSharedConfig();
  const log = logger ?? createLogger({ service: 'test', level: config.logLevel });
  const pool = createPool({ databaseUrl: config.databaseUrl, logger: log });
  const app = buildApp({ db: pool, logger: log });
  return {
    app,
    pool,
    close: async () => {
      await app.close();
      await pool.end();
    },
  };
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
