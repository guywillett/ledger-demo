import { createServer, type Server, type Socket } from 'node:net';
import type { AddressInfo } from 'node:net';
import { afterAll, describe, expect, it } from 'vitest';
import { buildApp } from '../../src/api/app.ts';
import { HEALTH_TIMEOUT_MS } from '../../src/api/health.ts';
import { loadSharedConfig } from '../../src/shared/config.ts';
import { createPool } from '../../src/shared/db.ts';
import { expectMatchesContract } from '../contract.ts';
import { capturingLogger } from '../helpers.ts';

// GET /health (slice 4 decision 72): 200 {"status":"ok"} when the database answers SELECT 1 in
// time, a 503 problem when it doesn't. Logged only on failure. Go's go/internal/api/health_test.go
// has the same cases.

const NOT_READY = {
  type: 'urn:ledger-demo:problem:not-ready',
  title: 'Not ready',
  status: 503,
  detail: 'The database did not answer.',
};

// The app with its own captured log, on a pool to `databaseUrl` (the test database by default).
function appWith(databaseUrl = loadSharedConfig().databaseUrl) {
  const own = capturingLogger();
  const pool = createPool({ databaseUrl, logger: own.logger });
  const app = buildApp({ pool, logger: own.logger });
  return {
    app,
    lines: own.lines,
    close: async () => {
      await app.close();
      await pool.end();
    },
  };
}

// A server that accepts connections and never answers: a database that hangs (no reply to the
// connection's startup message), so only the timeout ends the check.
async function silentServer(): Promise<{ url: string; close: () => Promise<void> }> {
  const sockets = new Set<Socket>();
  const server: Server = createServer((socket) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `postgres://ledger:ledger@127.0.0.1:${port}/none_test`,
    close: async () => {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

describe('GET /health', () => {
  const healthy = appWith();
  afterAll(healthy.close);

  it('answers 200 {"status":"ok"} when the database answers, logging nothing', async () => {
    const response = await healthy.app.inject({
      method: 'GET',
      url: '/health',
      headers: { 'x-request-id': 'probe-1' },
    });
    expectMatchesContract('GET', '/health', response);
    expect(response.statusCode).toBe(200);
    expect(response.body).toBe('{"status":"ok"}');
    expect(response.headers['content-type']).toBe('application/json; charset=utf-8');
    expect(response.headers['x-request-id']).toBe('probe-1');
    // No access line for a successful probe (nor anything else).
    expect(healthy.lines()).toEqual([]);
  });

  it('answers HEAD with the headers only, logging nothing', async () => {
    const response = await healthy.app.inject({ method: 'HEAD', url: '/health' });
    expect(response.statusCode).toBe(200);
    expect(response.body).toBe('');
    expect(response.headers['content-length']).toBe(String('{"status":"ok"}'.length));
    expect(response.headers['content-type']).toBe('application/json; charset=utf-8');
    expect(response.headers['x-request-id']).toBeDefined();
    expect(healthy.lines()).toEqual([]);
  });
});

describe('GET /health when the database does not answer', () => {
  // A 503 problem, one error line with the cause, and the access line (a failure is logged).
  // beforeClose runs before the app's pool is ended: pool.end() waits for a connection attempt
  // still pending, which only ends once the database side goes away.
  async function expectNotReady(
    databaseUrl: string,
    beforeClose: () => Promise<void> = async () => undefined,
  ): Promise<void> {
    const broken = appWith(databaseUrl);
    try {
      const started = Date.now();
      const response = await broken.app.inject({ method: 'GET', url: '/health' });
      const elapsed = Date.now() - started;

      expectMatchesContract('GET', '/health', response);
      expect(response.statusCode).toBe(503);
      expect(response.json()).toEqual(NOT_READY);
      expect(response.headers['content-type']).toBe('application/problem+json; charset=utf-8');
      expect(response.headers['x-request-id']).toBeDefined();
      expect(elapsed).toBeLessThan(HEALTH_TIMEOUT_MS + 1000);

      const errors = broken.lines().filter((line) => line.level === 'error');
      expect(errors).toEqual([
        expect.objectContaining({ msg: 'health check failed', err: expect.any(Object) }),
      ]);
      expect(broken.lines().filter((line) => line.msg === 'request completed')).toEqual([
        expect.objectContaining({ path: '/health', status_code: 503 }),
      ]);
    } finally {
      await beforeClose();
      await broken.close();
    }
  }

  it('answers 503 when the database refuses connections', async () => {
    // Nothing listens on port 1: the connection fails at once.
    await expectNotReady('postgres://ledger:ledger@127.0.0.1:1/none_test');
  });

  it('answers 503 within the timeout when the database hangs', async () => {
    const silent = await silentServer();
    await expectNotReady(silent.url, silent.close);
  });

  // The first probe's connection attempt is still hanging after its 503: the next probe answers
  // 503 at once rather than start a second attempt (ping, shared/db.ts), so a hung database holds
  // one of the pool's connections, not one per probe.
  it('answers 503 at once while an earlier check against a hung database is unfinished', async () => {
    const silent = await silentServer();
    const broken = appWith(silent.url);
    try {
      const first = await broken.app.inject({ method: 'GET', url: '/health' });
      expect(first.statusCode).toBe(503);

      const started = Date.now();
      const second = await broken.app.inject({ method: 'GET', url: '/health' });
      expectMatchesContract('GET', '/health', second);
      expect(second.statusCode).toBe(503);
      expect(second.json()).toEqual(NOT_READY);
      expect(Date.now() - started).toBeLessThan(200);
      expect(broken.lines().filter((line) => line.level === 'error')).toHaveLength(2);
    } finally {
      await silent.close();
      await broken.close();
    }
  });
});
