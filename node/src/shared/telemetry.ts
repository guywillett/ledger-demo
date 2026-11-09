import {
  ROOT_CONTEXT,
  SpanStatusCode,
  context as otelContext,
  type Context,
  type TextMapGetter,
  type TextMapPropagator,
  type TextMapSetter,
} from '@opentelemetry/api';
import {
  CompositePropagator,
  W3CBaggagePropagator,
  W3CTraceContextPropagator,
  setGlobalErrorHandler,
} from '@opentelemetry/core';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-proto';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';
import { HttpInstrumentation } from '@opentelemetry/instrumentation-http';
import { PgInstrumentation } from '@opentelemetry/instrumentation-pg';
import { defaultResource, resourceFromAttributes } from '@opentelemetry/resources';
import {
  AggregationType,
  PeriodicExportingMetricReader,
  createAllowListAttributesProcessor,
  type IMetricReader,
  type ViewOptions,
} from '@opentelemetry/sdk-metrics';
import { NodeSDK } from '@opentelemetry/sdk-node';
import {
  BatchSpanProcessor,
  SimpleSpanProcessor,
  type Span,
  type SpanExporter,
  type SpanProcessor,
} from '@opentelemetry/sdk-trace';
import { sharedEnvSchema } from './config.ts';
import { createLogger, type Logger } from './logger.ts';

// OpenTelemetry for the four application processes (slice 5): traces and metrics over OTLP, by the
// rules of contracts/telemetry.md. Go: internal/telemetry.
//
// Started by each process's bin/ file before anything else is imported (startTelemetry, then a
// dynamic import of the process's main.ts): the instrumentations patch pg and node:http when they
// are first loaded, so they must be registered first. Node 26 loads a CommonJS package imported from
// an ES module (pg, Fastify's use of node:http) through the require hook they patch, so no ESM
// loader hook is needed. Processes started any other way (npm run dev:api, the tests) run without
// telemetry unless they start it themselves.
//
// Off unless OTEL_EXPORTER_OTLP_ENDPOINT is set: no SDK, no patching, nothing exported or logged.
// Set, it exports there, in the background and in batches (slice 5 decision 79): a refusing or slow
// backend costs telemetry, never a request or a delivery. The one exception found: an endpoint
// whose name doesn't resolve, whose failing lookups hold libuv's thread pool, which pg's lookups
// need too (the slice 5 spec's Known gaps; mitigated by the image's larger pool, node/Dockerfile).

export type TelemetryService = 'api' | 'relay' | 'deposit-worker' | 'submitter';

export type TelemetryOptions = {
  // The resource's service.name: the process's role, the same in both implementations (decision 90).
  readonly service: TelemetryService;
  readonly env?: NodeJS.ProcessEnv;
  // For tests: exporters in place of OTLP, used whatever the environment says. Spans are then
  // exported one at a time as they end (SimpleSpanProcessor), so a test sees them at once.
  readonly spanExporter?: SpanExporter;
  readonly metricReader?: IMetricReader;
};

// The one SDK of the process, for shutdownTelemetry; undefined while telemetry is off.
let running: { readonly sdk: NodeSDK; readonly logger: Logger } | undefined;

// Starts the SDK if OTEL_EXPORTER_OTLP_ENDPOINT is a URL (or a test passes exporters); returns
// whether it did. Call it once, before the process's other modules load.
export function startTelemetry(options: TelemetryOptions): boolean {
  const env = options.env ?? process.env;
  const testing = options.spanExporter !== undefined || options.metricReader !== undefined;
  // By the process's own rule (an absolute URL, or unset or empty for off). An invalid one leaves
  // telemetry off, and the process's configuration check then fails its startup.
  const endpoint = sharedEnvSchema.shape.OTEL_EXPORTER_OTLP_ENDPOINT.safeParse(
    env.OTEL_EXPORTER_OTLP_ENDPOINT,
  );
  if (!testing && (!endpoint.success || endpoint.data === undefined || endpoint.data === '')) {
    return false;
  }

  // Its own logger: the process's isn't made yet (its main.ts hasn't loaded). The level as the
  // process reads it; an invalid LOG_LEVEL is the process's own startup failure to report.
  const level = sharedEnvSchema.shape.LOG_LEVEL.safeParse(env.LOG_LEVEL);
  const logger = createLogger({
    service: options.service,
    level: level.success ? level.data : 'info',
  });
  setGlobalErrorHandler(rateLimitedErrorHandler(logger));

  const sdk = new NodeSDK({
    // Exactly the contract's resource attributes, plus the SDK's own (telemetry.sdk.*): no
    // detectors, so nothing about the host or the process's arguments is attached.
    resource: defaultResource().merge(
      resourceFromAttributes({ 'service.name': options.service, implementation: 'node' }),
    ),
    resourceDetectors: [],
    // W3C Trace Context and Baggage (decision 80). The API's callers are outside the system: it
    // never takes their context (decision 84).
    textMapPropagator:
      options.service === 'api' ? new InjectOnlyPropagator(W3C_PROPAGATOR) : W3C_PROPAGATOR,
    spanProcessors: [
      // First, so the exporter's processor gets the span as redacted.
      new DatabaseErrorRedaction(),
      options.spanExporter === undefined
        ? new BatchSpanProcessor({ exporter: new OTLPTraceExporter() })
        : new SimpleSpanProcessor({ exporter: options.spanExporter }),
    ],
    metricReaders: [
      options.metricReader ??
        new PeriodicExportingMetricReader({
          exporter: new OTLPMetricExporter(),
          exportIntervalMillis: exportInterval(env.OTEL_METRIC_EXPORT_INTERVAL),
        }),
    ],
    views: METRIC_VIEWS,
    // Logs go to stdout as JSON (with trace_id and span_id: logger.ts), not over OTLP.
    logRecordProcessors: [],
    instrumentations: [
      new HttpInstrumentation({
        // Incoming requests only. The one outgoing call is the submitter's to the provider, with
        // fetch, which this doesn't instrument anyway: no trace headers go to a third party
        // (decision 84), and none to anything else.
        disableOutgoingRequestInstrumentation: true,
        // Attributes the conventions record that the contract keeps out (its privacy rule): the
        // query string, as the access log leaves it out (caller input that could carry anything:
        // an IBAN, a name); the client's IP addresses, personal data under the GDPR; and its user
        // agent, nearly so. The hook's attributes override the instrumentation's, and an
        // undefined one isn't set. Go drops the same at export (internal/telemetry).
        startIncomingSpanHook: () => ({
          'url.query': undefined,
          'client.address': undefined,
          'network.peer.address': undefined,
          'user_agent.original': undefined,
        }),
      }),
      new PgInstrumentation({
        // A query outside any span gets none: the relay's and the consumers' polling, which would
        // otherwise be a trace a second per process (decision 85).
        requireParentSpan: true,
        ignoreConnectSpans: true,
        // The statement text only, never its parameters (the contract's privacy rule). false is
        // the default; said here because it is the rule.
        enhancedDatabaseReporting: false,
        // Named by the statement's operation, as Go's tracer names it (the contract), not this
        // instrumentation's "pg.query:SELECT ledger".
        requestHook: (span, info) => {
          const operation = statementOperation(info.query.text);
          span.updateName(operation);
          span.setAttribute('db.operation.name', operation);
        },
      }),
    ],
  });
  sdk.start();
  running = { sdk, logger };
  return true;
}

// How long shutdown may take to export what's left. Within every process's grace period: the API
// has 5 s in all, and the backend may be absent (each export would retry for its own 10 s timeout).
const SHUTDOWN_TIMEOUT_MS = 2000;

// Flushes and stops the SDK, after the process's work has stopped (so its last spans are in).
// Never throws: telemetry that can't be exported is dropped, with one warning. Nothing to do while
// telemetry is off.
export async function shutdownTelemetry(): Promise<void> {
  if (running === undefined) return;
  const { sdk, logger } = running;
  running = undefined;
  // Settled either way, so a shutdown still running when the timeout wins can't reject unhandled.
  const flushed = sdk.shutdown().then(
    () => 'flushed' as const,
    (err: unknown) => (err instanceof Error ? err : new Error(String(err))),
  );
  const timedOut = new Promise<'timed out'>((resolve) => {
    setTimeout(resolve, SHUTDOWN_TIMEOUT_MS, 'timed out').unref();
  });
  const result = await Promise.race([flushed, timedOut]);
  if (result === 'timed out') {
    logger.warn({ timeout_ms: SHUTDOWN_TIMEOUT_MS }, 'telemetry not flushed in time; dropped');
  } else if (result instanceof Error) {
    logger.warn({ err: result }, 'telemetry not flushed; dropped');
  }
}

// A failed statement's span keeps its error's type (the SQLSTATE, as error.type) and an error status
// with a fixed description, never Postgres's message: it can quote the values sent (22P02: invalid
// input syntax for type uuid: "…"). The pg instrumentation puts that message in the status's
// description, with no option to leave it out, and the SQLSTATE only in its own exception event
// (a sanitised "PostgreSQL error of type 'error' occurred (code: 22P02)"), not as error.type. So as
// the span ends (onEnding: before it is closed, so still writable, and before any exporter sees
// it) the code is taken from that event into error.type, the event dropped (the contract records
// none) and the description replaced. The event's format is the instrumentation's: the 22P02 test
// (test/telemetry/) fails if an upgrade changes it. Go's tracer records the same directly.
export const DATABASE_ERROR = 'database error';
const SANITISED_CODE = /\(code: ([0-9A-Z]{5})\)$/;

class DatabaseErrorRedaction implements SpanProcessor {
  onStart(): void {}

  onEnding(span: Span): void {
    if (span.attributes['db.system.name'] === undefined) return;
    if (span.status.code !== SpanStatusCode.ERROR) return;
    for (const event of span.events) {
      const code = SANITISED_CODE.exec(String(event.attributes?.['exception.message']))?.[1];
      if (event.name === 'exception' && code !== undefined) span.setAttribute('error.type', code);
    }
    span.setStatus({ code: SpanStatusCode.ERROR, message: DATABASE_ERROR });
    const kept = span.events.filter((event) => event.name !== 'exception');
    span.events.splice(0, span.events.length, ...kept);
  }

  onEnd(): void {}

  async forceFlush(): Promise<void> {}

  async shutdown(): Promise<void> {}
}

const W3C_PROPAGATOR = new CompositePropagator({
  propagators: [new W3CTraceContextPropagator(), new W3CBaggagePropagator()],
});

// A propagator that injects (into the outbox, in step 2) but never extracts. The HTTP server
// instrumentation reads an incoming request's context through the global propagator, so this is
// where the API refuses it: every request starts a new trace (decision 84).
export class InjectOnlyPropagator implements TextMapPropagator {
  readonly #inner: TextMapPropagator;

  constructor(inner: TextMapPropagator) {
    this.#inner = inner;
  }

  inject(context: Context, carrier: unknown, setter: TextMapSetter): void {
    this.#inner.inject(context, carrier, setter);
  }

  extract(context: Context, _carrier: unknown, _getter: TextMapGetter): Context {
    return context;
  }

  fields(): string[] {
    return this.#inner.fields();
  }
}

// The metrics the contract lists, and only those, each with only its listed attributes (bounded
// cardinality: no raw path, no address): a view per kept metric, naming it exactly, and the other
// instruments the instrumentations create dropped. Not a catch-all "*" dropping the rest: this SDK
// gives an instrument one storage per metric name, so an instrument matching two views (its own
// and "*") records into that storage twice, doubling every count. So each instrument matches one
// view at most: the pg instrumentation's are all dropped by its meter's name, the HTTP
// instrumentation's client duration (outgoing requests aren't instrumented) by its own. The
// metrics test (test/telemetry/) fails on any other metric exported, and on a count that isn't 1.
// Go has no such trap: its view is a function giving one stream per instrument.
const METRIC_VIEWS: ViewOptions[] = [
  {
    instrumentName: 'http.server.request.duration',
    attributesProcessors: [
      createAllowListAttributesProcessor([
        'http.request.method',
        'http.route',
        'http.response.status_code',
      ]),
    ],
  },
  { meterName: '@opentelemetry/instrumentation-pg', aggregation: { type: AggregationType.DROP } },
  { instrumentName: 'http.client.request.duration', aggregation: { type: AggregationType.DROP } },
];

// OTEL_METRIC_EXPORT_INTERVAL in ms, the variable Go's SDK reads itself; 60 s by default, as both
// SDKs' default.
function exportInterval(value: string | undefined): number {
  const ms = Number(value);
  return Number.isInteger(ms) && ms > 0 ? ms : 60_000;
}

// A statement's operation: its first keyword, upper-cased (SELECT, INSERT, UPDATE, WITH, BEGIN, …).
// Every statement is a constant in our code, so this is a bounded set.
export function statementOperation(text: string): string {
  return /^\s*([A-Za-z]+)/.exec(text)?.[1]?.toUpperCase() ?? 'QUERY';
}

// The SDK's own errors (mostly an export that failed: the backend absent or slow) as log lines,
// without flooding: the first at once, then at most one per interval, counting those left out.
export function rateLimitedErrorHandler(
  logger: Logger,
  intervalMs = 60_000,
  now: () => number = Date.now,
): (err: unknown) => void {
  let lastLogged = -Infinity;
  let suppressed = 0;
  return (err) => {
    const at = now();
    if (at - lastLogged < intervalMs) {
      suppressed += 1;
      return;
    }
    // Outside any context: an export runs in the context of whatever it exports, and the line
    // would otherwise carry that request's trace_id, as if the failure were part of it.
    otelContext.with(ROOT_CONTEXT, () => {
      logger.warn(
        { err: err instanceof Error ? err : new Error(String(err)), suppressed },
        'telemetry export failed',
      );
    });
    lastLogged = at;
    suppressed = 0;
  };
}
