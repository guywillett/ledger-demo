import { randomUUID } from 'node:crypto';
import { afterAll, describe, expect, it } from 'vitest';
import { expectMatchesContract } from '../contract.ts';
import { ALICE, createTestApp } from '../helpers.ts';

// Bodies Fastify rejects before a route's handler runs: malformed JSON (400), a non-JSON content
// type (415) and an oversized body (413). The client API answers each with its exact code; the
// provider webhooks answer every one with 400, from their plugin's own error handler (Fastify
// encapsulation, api/webhooks/index.ts). Every answer is problem details, checked against the
// contract.

const { app, close } = await createTestApp();
afterAll(close);

// Over Fastify's default body limit (1 MiB), as valid JSON.
const OVERSIZED = JSON.stringify({ padding: 'x'.repeat(1_100_000) });

const BODIES = [
  ['malformed JSON', 'application/json', '{"amount_minor": '],
  ['a non-JSON content type', 'text/plain', 'hello'],
  ['an oversized body', 'application/json', OVERSIZED],
] as const;

const ROUTES = [
  {
    route: 'POST /provider/incoming-payments',
    url: '/provider/incoming-payments',
    contractPath: '/provider/incoming-payments',
    headers: {},
    // The webhook contract: one 4xx.
    expected: [400, 400, 400],
  },
  {
    route: 'POST /provider/transfer-updates',
    url: '/provider/transfer-updates',
    contractPath: '/provider/transfer-updates',
    headers: {},
    expected: [400, 400, 400],
  },
  {
    route: 'POST /accounts/:account_id/withdrawals (client API)',
    url: `/accounts/${ALICE.id}/withdrawals`,
    contractPath: '/accounts/{account_id}/withdrawals',
    headers: { 'idempotency-key': randomUUID() },
    // The exact codes.
    expected: [400, 415, 413],
  },
] as const;

describe.each(ROUTES)('$route', ({ url, contractPath, headers, expected }) => {
  it.each(
    BODIES.map(([name, contentType, payload], i) => [name, expected[i], contentType, payload]),
  )('answers %s with %i and a problem body', async (_name, status, contentType, payload) => {
    const response = await app.inject({
      method: 'POST',
      url,
      headers: { 'content-type': String(contentType), ...headers },
      payload: String(payload),
    });
    expectMatchesContract('POST', contractPath, response);
    expect(response.statusCode).toBe(status);
    expect(response.headers['content-type']).toMatch(/^application\/problem\+json/);
    expect(JSON.parse(response.body)).toMatchObject({ type: 'about:blank', status });
  });
});
