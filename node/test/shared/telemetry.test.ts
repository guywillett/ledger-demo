import {
  ROOT_CONTEXT,
  defaultTextMapGetter,
  defaultTextMapSetter,
  trace,
} from '@opentelemetry/api';
import { hostname } from 'node:os';
import { W3CTraceContextPropagator } from '@opentelemetry/core';
import { describe, expect, it } from 'vitest';
import { routeTemplate } from '../../src/api/app.ts';
import { loadSharedConfig, sharedEnvSchema } from '../../src/shared/config.ts';
import {
  InjectOnlyPropagator,
  rateLimitedErrorHandler,
  shutdownTelemetry,
  startTelemetry,
  statementOperation,
  telemetryResource,
} from '../../src/shared/telemetry.ts';
import { capturingLogger } from '../helpers.ts';

// The telemetry module's parts that need no running SDK. The SDK itself, with in-memory exporters,
// is tested in test/telemetry/ (its own project: it patches pg and node:http).

describe('startTelemetry', () => {
  it('is off without OTEL_EXPORTER_OTLP_ENDPOINT: nothing started, shutdown a no-op', async () => {
    expect(startTelemetry({ service: 'api', env: {} })).toBe(false);
    expect(startTelemetry({ service: 'api', env: { OTEL_EXPORTER_OTLP_ENDPOINT: '' } })).toBe(
      false,
    );
    await shutdownTelemetry();
  });

  it("is off for an endpoint that is no URL: the process's configuration check then fails", () => {
    expect(
      startTelemetry({ service: 'api', env: { OTEL_EXPORTER_OTLP_ENDPOINT: 'otel-lgtm' } }),
    ).toBe(false);
  });
});

describe('OTEL_EXPORTER_OTLP_ENDPOINT in the shared configuration', () => {
  const base = { DATABASE_URL: 'postgres://ledger:ledger@localhost:5432/ledger_test' };

  it.each([
    ['unset', {}],
    ['empty', { OTEL_EXPORTER_OTLP_ENDPOINT: '' }],
    ['a URL', { OTEL_EXPORTER_OTLP_ENDPOINT: 'http://otel-lgtm:4318' }],
  ])('accepts it %s', (_case, vars) => {
    expect(() => loadSharedConfig({ ...base, ...vars })).not.toThrow();
  });

  it.each([
    ['no scheme', 'otel-lgtm'],
    ['not a URL', 'http://%zz'],
  ])('refuses %s, naming the variable', (_case, value) => {
    expect(() => loadSharedConfig({ ...base, OTEL_EXPORTER_OTLP_ENDPOINT: value })).toThrow(
      /OTEL_EXPORTER_OTLP_ENDPOINT/,
    );
  });
});

describe('OTEL_LOGS_EXPORTER in the shared configuration', () => {
  const base = { DATABASE_URL: 'postgres://ledger:ledger@localhost:5432/ledger_test' };

  it.each([
    ['otlp', 'otlp'],
    ['none', 'none'],
  ])('accepts %s', (_case, value) => {
    expect(() => loadSharedConfig({ ...base, OTEL_LOGS_EXPORTER: value })).not.toThrow();
  });

  it("defaults to otlp, the specification's default", () => {
    expect(sharedEnvSchema.parse(base).OTEL_LOGS_EXPORTER).toBe('otlp');
  });

  it.each([
    ['console (the specification has it; we have no use for it)', 'console'],
    ['empty', ''],
  ])('refuses %s, naming the variable', (_case, value) => {
    expect(() => loadSharedConfig({ ...base, OTEL_LOGS_EXPORTER: value })).toThrow(
      /OTEL_LOGS_EXPORTER/,
    );
  });
});

// Decision 90: two instances of one service are told apart (identical metric series otherwise), and
// a person can find each one's container.
describe('telemetryResource', () => {
  it('gives each process start its own service.instance.id, a UUID, and the host name', () => {
    const first = telemetryResource('deposit-worker').attributes;
    const second = telemetryResource('deposit-worker').attributes;
    expect(first['service.instance.id']).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
    );
    expect(second['service.instance.id']).not.toBe(first['service.instance.id']);
    expect(first['host.name']).toBe(hostname());
    expect(first).toMatchObject({ 'service.name': 'deposit-worker', implementation: 'node' });
  });
});

describe('rateLimitedErrorHandler', () => {
  it('logs the first error at once, then at most one per interval, counting those left out', () => {
    const { logger, lines } = capturingLogger();
    let now = 0;
    const handle = rateLimitedErrorHandler(logger, 60_000, () => now);

    handle(new Error('connect ECONNREFUSED'));
    now = 1000;
    handle(new Error('again'));
    handle('a string, as the SDK sometimes passes');
    now = 61_000;
    handle(new Error('a minute on'));

    expect(lines().map((l) => [l.level, l.msg, l.suppressed])).toEqual([
      ['warn', 'telemetry export failed', 0],
      ['warn', 'telemetry export failed', 2],
    ]);
    expect(lines().map((l) => (l.err as { message: string }).message)).toEqual([
      'connect ECONNREFUSED',
      'a minute on',
    ]);
  });
});

describe('InjectOnlyPropagator', () => {
  const spanContext = {
    traceId: '0af7651916cd43dd8448eb211c80319c',
    spanId: 'b7ad6b7169203331',
    traceFlags: 1,
  };

  it('ignores an incoming context', () => {
    const propagator = new InjectOnlyPropagator(new W3CTraceContextPropagator());
    const carrier = { traceparent: `00-${spanContext.traceId}-${spanContext.spanId}-01` };
    const extracted = propagator.extract(ROOT_CONTEXT, carrier, defaultTextMapGetter);
    expect(trace.getSpanContext(extracted)).toBeUndefined();
  });

  it('still injects its own', () => {
    const propagator = new InjectOnlyPropagator(new W3CTraceContextPropagator());
    const carrier: Record<string, string> = {};
    const context = trace.setSpanContext(ROOT_CONTEXT, spanContext);
    propagator.inject(context, carrier, defaultTextMapSetter);
    expect(carrier).toEqual({ traceparent: `00-${spanContext.traceId}-${spanContext.spanId}-01` });
    expect(propagator.fields()).toEqual(['traceparent', 'tracestate']);
  });
});

describe('names', () => {
  it.each([
    ['SELECT id FROM accounts WHERE id = $1', 'SELECT'],
    ['\n  insert into deposits (id) values ($1)', 'INSERT'],
    ['WITH claimed AS (SELECT 1) UPDATE outbox SET x = 1', 'WITH'],
    ['BEGIN ISOLATION LEVEL REPEATABLE READ', 'BEGIN'],
    ['-- a comment first', 'QUERY'],
  ])('names the statement %j by its operation, %s', (text, operation) => {
    expect(statementOperation(text)).toBe(operation);
  });

  it.each([
    ['/accounts/:account_id/deposits', '/accounts/{account_id}/deposits'],
    [
      '/accounts/:account_id/withdrawals/:withdrawal_id',
      '/accounts/{account_id}/withdrawals/{withdrawal_id}',
    ],
    ['/health', '/health'],
  ])("writes Fastify's route %s as the contract's %s", (url, template) => {
    expect(routeTemplate(url)).toBe(template);
  });
});
