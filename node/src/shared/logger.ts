import { pino, type Logger } from 'pino';
import type { LogLevel } from './config.ts';

export type { Logger };

// Structured JSON to stdout. Context such as request_id, deposit_id and event_id
// is attached with child loggers: logger.child({ deposit_id }).
// BigInts are converted in the object passed to each log call, but not in child bindings:
// pino deliberately bypasses formatters for child bindings (it swaps formatters.bindings
// for a pass-through when creating a child). So bind only IDs and other strings, and put
// amounts in the log call's object.
// `destination` defaults to stdout; tests pass an in-memory stream.
export function createLogger(
  options: { service: string; level: LogLevel },
  destination?: pino.DestinationStream,
): Logger {
  const pinoOptions: pino.LoggerOptions = {
    level: options.level,
    base: { service: options.service },
    timestamp: pino.stdTimeFunctions.isoTime,
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
  return destination ? pino(pinoOptions, destination) : pino(pinoOptions);
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
