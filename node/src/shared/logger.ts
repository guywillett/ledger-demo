import {
  SeverityNumber,
  context as otelContext,
  isSpanContextValid,
  logs,
  trace,
  type AnyValue,
} from '@opentelemetry/api';
import { pino, type Logger } from 'pino';
import type { LogLevel } from './config.ts';

export type { Logger };

// Whether loggers made from now on also copy each line to the telemetry backend (slice 5 decision
// 93): turned on by startTelemetry once its logger provider is registered, unless
// OTEL_LOGS_EXPORTER is none. Off in a process without telemetry, so its loggers are stdout only.
let copying = false;

export function copyLogLines(on: boolean): void {
  copying = on;
}

// Structured JSON to stdout. Context such as request_id, deposit_id and event_id
// is attached with child loggers: logger.child({ deposit_id }).
// BigInts are converted in the object passed to each log call, but not in child bindings:
// pino deliberately bypasses formatters for child bindings (it swaps formatters.bindings
// for a pass-through when creating a child). So bind only IDs and other strings, and put
// amounts in the log call's object.
// `destination` defaults to stdout; tests pass an in-memory stream.
//
// While copyLogLines is on, every line also goes to OpenTelemetry's logs API (telemetryCopy), the
// stdout destination and the copy side by side in a pino multistream, which hands both the same
// line: stdout's bytes are unchanged. A child logger writes through its parent's streams.
export function createLogger(
  options: { service: string; level: LogLevel },
  destination?: pino.DestinationStream,
): Logger {
  const pinoOptions: pino.LoggerOptions = {
    level: options.level,
    // implementation: which implementation wrote the line (slice 3 decision 51), as the Go
    // implementation's lines say "go", so a mixed stack's logs say who did what.
    base: { service: options.service, implementation: 'node' },
    timestamp: pino.stdTimeFunctions.isoTime,
    // trace_id and span_id of the span active when the line is logged, so a line leads to its trace
    // (slice 5 decision 86). Nothing outside a traced context, or with telemetry off (no span is
    // ever active then).
    mixin: traceFields,
    // Errors are left alone by the log formatter below (it walks plain objects only), and
    // pino's err serializer runs after it, copying the error's own properties: a BigInt
    // property (e.g. OverdraftError.amountMinor) would then be written as a bare number.
    // So convert BigInts in the serialized error too. The serializer's result has its own
    // prototype (not a plain object, which stringifyBigInts would leave alone), so copy its
    // own fields into a plain object first.
    serializers: {
      err: (err: Error) => stringifyBigInts({ ...pino.stdSerializers.err(err) }),
    },
    formatters: {
      // "level": "info" rather than pino's default numeric "level": 30.
      level: (label) => ({ level: label }),
      log: (object) => stringifyBigInts(object),
    },
  };
  if (!copying) return destination ? pino(pinoOptions, destination) : pino(pinoOptions);
  // pino's own default destination, built explicitly to go in the multistream. Each stream's level
  // the lowest: the logger's level has already decided what's written.
  const stdout = destination ?? pino.destination(process.stdout.fd);
  return pino(
    pinoOptions,
    pino.multistream([
      { level: 'trace', stream: stdout },
      { level: 'trace', stream: telemetryCopy() },
    ]),
  );
}

function traceFields(): Record<string, string> {
  const spanContext = trace.getActiveSpan()?.spanContext();
  if (spanContext === undefined || !isSpanContextValid(spanContext)) return {};
  return { trace_id: spanContext.traceId, span_id: spanContext.spanId };
}

// Our levels as OpenTelemetry's severity numbers (contracts/telemetry.md, Logs). The text is the
// level as on stdout.
const SEVERITY: Record<string, SeverityNumber> = {
  trace: SeverityNumber.TRACE,
  debug: SeverityNumber.DEBUG,
  info: SeverityNumber.INFO,
  warn: SeverityNumber.WARN,
  error: SeverityNumber.ERROR,
  fatal: SeverityNumber.FATAL,
};

// The line's fields that aren't attributes of its record: those the record has fields for (time,
// level, msg, and trace_id and span_id: its own trace and span IDs), err (exception, below), and
// those the resource has (service and implementation: contracts/telemetry.md, Resource).
const NOT_ATTRIBUTES = new Set([
  'time',
  'level',
  'msg',
  'trace_id',
  'span_id',
  'err',
  'service',
  'implementation',
]);

// The copy: a pino destination (anything with a write method) that turns each line into an
// OpenTelemetry log record and emits it through the global logger provider, which batches and
// exports it (decision 79: droppable, never in the way). The mapping is the contract's, the same as
// Go's. pino writes a line synchronously, inside the log call, so the active context here is the
// call's: a line logged in a span gets that span's trace and span IDs as the record's own (the
// fields trace_id and span_id the mixin added say the same; they aren't repeated as attributes).
// Never throws: a record that can't be made is dropped, and stdout has the line anyway.
function telemetryCopy(): pino.DestinationStream {
  const otelLogger = logs.getLogger('ledger-demo/logger');
  return {
    write(line: string): void {
      try {
        const fields = JSON.parse(line) as Record<string, AnyValue>;
        const attributes: Record<string, AnyValue> = {};
        for (const [key, value] of Object.entries(fields)) {
          if (!NOT_ATTRIBUTES.has(key)) attributes[key] = value;
        }
        const level = String(fields.level);
        otelLogger.emit({
          timestamp: new Date(String(fields.time)),
          severityNumber: SEVERITY[level] ?? SeverityNumber.UNSPECIFIED,
          severityText: level,
          ...(fields.msg === undefined ? {} : { body: fields.msg }),
          attributes: { ...attributes, ...exception(fields.err) },
          context: otelContext.active(),
        });
      } catch {
        // Dropped: see above.
      }
    },
  };
}

// A line's err (pino's serialised error: type, message, stack) as OpenTelemetry's exception
// attributes, as Go's SDK turns a logged error into them. Anything else under err stays as it is.
function exception(err: AnyValue): Record<string, AnyValue> {
  if (err === undefined) return {};
  if (err === null || typeof err !== 'object' || Array.isArray(err) || err instanceof Uint8Array) {
    return { err };
  }
  const { type, message, stack } = err as Record<string, AnyValue>;
  return {
    ...(type === undefined ? {} : { 'exception.type': type }),
    ...(message === undefined ? {} : { 'exception.message': message }),
    ...(stack === undefined ? {} : { 'exception.stacktrace': stack }),
  };
}

// pino writes a BigInt as a bare JSON number, which JSON parsers round above 2^53.
// Logs follow the same rule as the API and events: BigInt values become digit strings.
// Only plain objects and arrays are walked; errors, dates and other instances are left
// to pino's serialisers. Returns a converted copy; the input is not modified.
// See docs/decisions/0001-money-representation.md.
export function stringifyBigInts(object: Record<string, unknown>): Record<string, unknown> {
  return convert(object, new Map()) as Record<string, unknown>;
}

// `memo` maps each object or array already visited to its converted copy, so an object
// that appears twice is converted once and reused, and a cycle in the input becomes the
// same cycle in the output (which pino then prints as "[Circular]").
function convert(value: unknown, memo: Map<object, unknown>): unknown {
  if (typeof value === 'bigint') return value.toString();
  if (value === null || typeof value !== 'object') return value;

  const done = memo.get(value);
  if (done !== undefined) return done;

  if (Array.isArray(value)) {
    const out: unknown[] = [];
    memo.set(value, out);
    for (const item of value) out.push(convert(item, memo));
    return out;
  }

  const proto: unknown = Object.getPrototypeOf(value);
  if (proto !== Object.prototype && proto !== null) return value;

  const out: Record<string, unknown> = {};
  memo.set(value, out);
  for (const [key, item] of Object.entries(value)) out[key] = convert(item, memo);
  return out;
}
