import { afterAll, describe, expect, it } from 'vitest';
import { expectMatchesContract } from '../contract.ts';
import { createTestApp } from '../helpers.ts';

// An empty path parameter names no resource: the not-found handler's 404, not the route's
// validation error (slice 3 decision 62; the hook in api/app.ts). Fastify's router matches an
// empty parameter, so without the hook these would be 400s.

const { app, close } = await createTestApp();
afterAll(close);

const NO_SUCH_ROUTE = {
  type: 'about:blank',
  title: 'Not Found',
  status: 404,
  detail: 'No such route.',
};

describe('an empty path parameter', () => {
  it.each([
    ['GET', '/withdrawals/', '/withdrawals/{withdrawal_id}'],
    ['GET', '/accounts//balance', '/accounts/{account_id}/balance'],
    ['GET', '/accounts//transactions?limit=1', '/accounts/{account_id}/transactions'],
  ] as const)('%s %s answers 404, as an unknown route', async (method, url, contractPath) => {
    const response = await app.inject({ method, url });
    expect(response.statusCode).toBe(404);
    expect(response.json()).toEqual(NO_SUCH_ROUTE);
    expect(response.headers['x-request-id']).toBeDefined();
    expectMatchesContract(method, contractPath, response);
  });

  it('answers 404 before the body is parsed', async () => {
    // Malformed JSON: a 400 if the body were parsed first.
    const response = await app.inject({
      method: 'POST',
      url: '/accounts//withdrawals',
      headers: { 'content-type': 'application/json', 'idempotency-key': 'k1' },
      payload: '{"amount_minor": ',
    });
    expect(response.statusCode).toBe(404);
    expect(response.json()).toEqual(NO_SUCH_ROUTE);
  });

  it('leaves a non-empty invalid parameter to the route (400)', async () => {
    const response = await app.inject({ method: 'GET', url: '/withdrawals/nope' });
    expect(response.statusCode).toBe(400);
  });
});
