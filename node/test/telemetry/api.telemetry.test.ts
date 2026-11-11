import { randomUUID } from 'node:crypto';
import type { AddressInfo } from 'node:net';
import { SpanKind, SpanStatusCode, trace, type Attributes } from '@opentelemetry/api';
import {
  AggregationTemporality,
  InMemoryMetricExporter,
  PeriodicExportingMetricReader,
} from '@opentelemetry/sdk-metrics';
import { InMemorySpanExporter, type ReadableSpan } from '@opentelemetry/sdk-trace';
import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  DATABASE_ERROR,
  rateLimitedErrorHandler,
  startTelemetry,
} from '../../src/shared/telemetry.ts';

// The API's telemetry, against contracts/telemetry.md, with in-memory exporters: span names and
// kinds, the incoming context refused (slice 5 decision 84), no parameters or personal data in any
// span, trace_id and span_id on the log lines, the metrics' allow-list and the resource.
//
// Telemetry starts before anything that loads pg or node:http (the dynamic imports below), as
// bin/api starts it. The app listens on a real port: app.inject() bypasses node:http, which is what
// the HTTP instrumentation observes.
const spans = new InMemorySpanExporter();
const metrics = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
const metricReader = new PeriodicExportingMetricReader({
  exporter: metrics,
  exportIntervalMillis: 3_600_000, // only when a test flushes
});
startTelemetry({ service: 'api', spanExporter: spans, metricReader });

const helpers = await import('../helpers.ts');
const { ALICE, FOREIGN_IBAN, capturingLogger, createTestAccount, fundTestAccount } = helpers;

const { logger, lines } = capturingLogger();
const testApp = await helpers.createTestApp({ logger });
const { app, pool } = testApp;
await app.listen({ host: '127.0.0.1', port: 0 });
const base = `http://127.0.0.1:${(app.server.address() as AddressInfo).port}`;
afterAll(testApp.close);

beforeEach(() => {
  spans.reset();
});

function serverSpans(): ReadableSpan[] {
  return spans.getFinishedSpans().filter((s) => s.kind === SpanKind.SERVER);
}

// A server span ends when the response closes, which can be just after fetch resolves: wait for
// the n requests' spans before asserting on them.
async function settled(n: number): Promise<ReadableSpan[]> {
  await vi.waitFor(() => expect(serverSpans()).toHaveLength(n));
  return serverSpans();
}

describe('HTTP server spans', () => {
  it('are named by the route template, with http.route, never the raw path', async () => {
    const account = await createTestAccount(pool);
    expect((await fetch(`${base}/accounts/${account.typeId}/balance`)).status).toBe(200);
    expect((await fetch(`${base}/accounts/${account.typeId}/deposits?limit=1`)).status).toBe(200);

    expect((await settled(2)).map((s) => [s.name, s.attributes['http.route']])).toEqual([
      ['GET /accounts/{account_id}/balance', '/accounts/{account_id}/balance'],
      ['GET /accounts/{account_id}/deposits', '/accounts/{account_id}/deposits'],
    ]);
    expect(serverSpans().every((s) => !s.name.includes(account.typeId))).toBe(true);
  });

  it('name an unknown route and an empty path parameter by the method alone', async () => {
    expect((await fetch(`${base}/nope/${randomUUID()}`)).status).toBe(404);
    expect((await fetch(`${base}/accounts//balance`)).status).toBe(404);
    expect((await settled(2)).map((s) => [s.name, s.attributes['http.route']])).toEqual([
      ['GET', undefined],
      ['GET', undefined],
    ]);
  });

  it('start a new trace for every request, whatever traceparent it brings (decision 84)', async () => {
    const sent = '0af7651916cd43dd8448eb211c80319c';
    const response = await fetch(`${base}/health`, {
      headers: { traceparent: `00-${sent}-b7ad6b7169203331-01`, baggage: 'user=someone' },
    });
    expect(response.status).toBe(200);
    const [span] = await settled(1);
    expect(span?.spanContext().traceId).not.toBe(sent);
    expect(span?.parentSpanContext).toBeUndefined();
  });

  // IP addresses are personal data under the GDPR, a user agent nearly so (the contract's privacy
  // rule): kept out though the conventions record them.
  it('carry no client address and no user agent', async () => {
    const agent = 'Mozilla/5.0 (Macintosh; a person)';
    const response = await fetch(`${base}/health`, { headers: { 'user-agent': agent } });
    expect(response.status).toBe(200);
    const [span] = await settled(1);
    for (const key of ['client.address', 'network.peer.address', 'user_agent.original']) {
      expect(span?.attributes).not.toHaveProperty(key);
    }
    expect(JSON.stringify(span?.attributes)).not.toContain('Mozilla');
  });

  it('carry no query string', async () => {
    const response = await fetch(
      `${base}/accounts/${ALICE.id}/deposits?network_transaction_id=${randomUUID()}`,
    );
    expect(response.status).toBe(200);
    const [span] = await settled(1);
    expect(span?.attributes).not.toHaveProperty('url.query');
    expect(span?.attributes['url.path']).toBe(`/accounts/${ALICE.id}/deposits`);
  });
});

describe('database spans', () => {
  it('are children of the request, named by the operation, with the statement and no parameters', async () => {
    const account = await createTestAccount(pool);
    await fetch(`${base}/accounts/${account.typeId}`);

    const [server] = await settled(1);
    const queries = spans.getFinishedSpans().filter((s) => s.kind === SpanKind.CLIENT);
    expect(queries.length).toBeGreaterThan(0);
    for (const query of queries) {
      expect(query.parentSpanContext?.spanId).toBe(server?.spanContext().spanId);
      expect(query.name).toBe('SELECT');
      expect(query.attributes).toMatchObject({
        'db.system.name': 'postgresql',
        'db.operation.name': 'SELECT',
        'db.query.text': expect.stringContaining('$1'),
      });
      // The account's UUID was the query's parameter: nowhere in the span.
      expect(JSON.stringify(query.attributes)).not.toContain(account.id);
    }
  });
});

describe('a failed statement', () => {
  // 22P02 quotes the value it refused in its message: the span keeps the SQLSTATE and an error
  // status with a fixed description, and no exception event, so the value is nowhere in it.
  it('keeps its SQLSTATE and an error status, never its message or the value', async () => {
    const value = `not-a-uuid-${randomUUID()}`;
    await trace.getTracer('test').startActiveSpan('a request', async (parent) => {
      await expect(pool.query('SELECT $1::uuid', [value])).rejects.toMatchObject({ code: '22P02' });
      parent.end();
    });
    const [query] = spans.getFinishedSpans().filter((s) => s.kind === SpanKind.CLIENT);
    expect(query?.attributes['error.type']).toBe('22P02');
    expect(query?.status).toEqual({ code: SpanStatusCode.ERROR, message: DATABASE_ERROR });
    expect(query?.events).toEqual([]);
    expect(
      JSON.stringify({
        attributes: query?.attributes,
        events: query?.events,
        status: query?.status,
      }),
    ).not.toContain(value);
  });
});

describe('no personal data in any span', () => {
  // Through every route that receives personal data: an incoming payment (an IBAN), a withdrawal
  // (an IBAN, a name, remittance text), each as sent and as normalised.
  it('carries no IBAN, name or remittance text, as sent or normalised, anywhere', async () => {
    const account = await createTestAccount(pool);
    await fundTestAccount(pool, account, 10_000n);
    const notification = helpers.notificationBody({
      creditor_iban: 'fr76 1234 5000 0100 0000 0000 193',
    });
    const posted = await fetch(`${base}/provider/incoming-payments`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(notification),
    });
    expect(posted.status).toBe(204);
    const withdrawal = await fetch(`${base}/accounts/${account.typeId}/withdrawals`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'idempotency-key': randomUUID() },
      body: JSON.stringify(
        helpers.withdrawalBody({
          creditor_iban: 'de75 5121 0800 1245 1261 99',
          creditor_name: 'Jane Q. Payee',
          remittance_info: 'Rent for flat 4B',
        }),
      ),
    });
    expect(withdrawal.status).toBe(201);
    await fetch(await withdrawalUrl(withdrawal));
    await settled(3);

    const everything = JSON.stringify(
      spans.getFinishedSpans().map((s) => ({
        name: s.name,
        attributes: s.attributes,
        events: s.events,
        resource: s.resource.attributes,
      })),
    );
    expect(spans.getFinishedSpans().length).toBeGreaterThan(3);
    for (const personal of [
      ALICE.iban,
      'fr76 1234',
      FOREIGN_IBAN,
      'de75 5121',
      'Jane Q. Payee',
      'Rent for flat 4B',
    ]) {
      expect(everything).not.toContain(personal);
    }
  });

  async function withdrawalUrl(response: Response): Promise<string> {
    const location = response.headers.get('location');
    expect(location).not.toBeNull();
    return `${base}${location ?? ''}`;
  }
});

describe('log lines', () => {
  it('carry the trace_id and span_id of the request they were logged in (decision 86)', async () => {
    const requestId = randomUUID();
    await fetch(`${base}/accounts/${ALICE.id}`, { headers: { 'x-request-id': requestId } });
    const [server] = await settled(1);
    const access = lines().find((l) => l.request_id === requestId && l.msg === 'request completed');
    expect(access).toMatchObject({
      trace_id: server?.spanContext().traceId,
      span_id: server?.spanContext().spanId,
    });
  });

  // An export runs in the context of what it exports: its failure isn't that request's.
  it("leave them off the SDK's error lines, whatever context the error comes from", () => {
    const own = capturingLogger();
    const handle = rateLimitedErrorHandler(own.logger);
    trace.getTracer('test').startActiveSpan('an export', (span) => {
      handle(new Error('getaddrinfo ENOTFOUND otel-lgtm'));
      span.end();
    });
    expect(own.lines()).toHaveLength(1);
    expect(own.lines()[0]).not.toHaveProperty('trace_id');
  });

  it('carry neither outside a traced context', () => {
    const own = capturingLogger();
    own.logger.info('outside any span');
    expect(own.lines()[0]).not.toHaveProperty('trace_id');
    expect(own.lines()[0]).not.toHaveProperty('span_id');
  });
});

describe('metrics', () => {
  // The contract's metrics and the attributes each may carry (the views' allow-list).
  const CONTRACT: Record<string, readonly string[]> = {
    'http.server.request.duration': [
      'http.request.method',
      'http.route',
      'http.response.status_code',
    ],
    ledger_transitions_total: ['entity', 'from', 'to', 'implementation'],
  };

  // One request, one measurement: a doubled count is what an instrument matching two views gives
  // (see METRIC_VIEWS in src/shared/telemetry.ts).
  it('measure each request once', async () => {
    const route = '/accounts/{account_id}';
    const countFor = async (): Promise<number> => {
      metrics.reset();
      await metricReader.forceFlush();
      let total = 0;
      for (const metric of metrics
        .getMetrics()
        .flatMap((rm) => rm.scopeMetrics.flatMap((sm) => sm.metrics))) {
        if (metric.descriptor.name !== 'http.server.request.duration') continue;
        for (const point of metric.dataPoints) {
          const value = point.value as { count?: number };
          if (point.attributes['http.route'] === route) total += value.count ?? 0;
        }
      }
      return total;
    };
    const before = await countFor();
    await fetch(`${base}/accounts/${ALICE.id}`);
    await settled(1);
    expect((await countFor()) - before).toBe(1);
  });

  it("are the contract's only, each with only its listed attributes", async () => {
    await fetch(`${base}/accounts/${ALICE.id}/balance`);
    await settled(1);
    await metricReader.forceFlush();
    const exported = metrics
      .getMetrics()
      .flatMap((rm) => rm.scopeMetrics.flatMap((sm) => sm.metrics));
    for (const metric of exported) {
      const allowed = CONTRACT[metric.descriptor.name];
      expect(allowed, metric.descriptor.name).toBeDefined();
      for (const point of metric.dataPoints) {
        // A request with no route has no http.route: a subset of the listed ones.
        expect(Object.keys(point.attributes).filter((key) => !allowed?.includes(key))).toEqual([]);
      }
    }
    const durations = exported.filter((m) => m.descriptor.name === 'http.server.request.duration');
    const points: Attributes[] = [];
    for (const metric of durations) {
      for (const point of metric.dataPoints) points.push(point.attributes);
    }
    expect(points).toContainEqual({
      'http.request.method': 'GET',
      'http.route': '/accounts/{account_id}/balance',
      'http.response.status_code': 200,
    });
  });
});

describe('the resource', () => {
  it("is the process's role and implementation, and the SDK's own attributes", async () => {
    await fetch(`${base}/health`);
    const [span] = await settled(1);
    expect(span?.resource.attributes).toMatchObject({
      'service.name': 'api',
      implementation: 'node',
      'telemetry.sdk.language': 'nodejs',
    });
    // Nothing else about the host or the process (no detectors).
    expect(Object.keys(span?.resource.attributes ?? {}).sort()).toEqual([
      'host.name',
      'implementation',
      'service.instance.id',
      'service.name',
      'telemetry.sdk.language',
      'telemetry.sdk.name',
      'telemetry.sdk.version',
    ]);
  });
});
