import { Writable } from 'node:stream';
import { SeverityNumber, trace } from '@opentelemetry/api';
import { InMemoryLogRecordExporter } from '@opentelemetry/sdk-logs';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyLogLines, createLogger } from '../../src/shared/logger.ts';
import { startTelemetry } from '../../src/shared/telemetry.ts';

// The log lines' copy to the telemetry backend (slice 5 decision 93), against contracts/telemetry.md
// (Logs): each line, as written to stdout, also exported as an OpenTelemetry log record with the
// contract's mapping. With an in-memory log exporter; OTEL_LOGS_EXPORTER=none and an absent backend
// have files of their own (logs-off, logs-absent), as each starts the SDK its own way.
const records = new InMemoryLogRecordExporter();
startTelemetry({
  service: 'deposit-worker',
  spanExporter: new InMemorySpanExporter(),
  logExporter: records,
});
const tracer = trace.getTracer('test');

// A logger made now (telemetry started: copying) whose stdout is collected, as raw text.
function capture(level: 'info' | 'trace' = 'info') {
  let text = '';
  const destination = new Writable({
    write(chunk, _encoding, callback) {
      text += String(chunk);
      callback();
    },
  });
  return {
    logger: createLogger({ service: 'deposit-worker', level }, destination),
    text: () => text,
  };
}

beforeEach(() => {
  records.reset();
});

describe('a log line', () => {
  it('is exported with its span, severity, message and fields', () => {
    const { logger } = capture();
    const span = tracer.startActiveSpan('work', (span) => {
      logger.child({ deposit_id: 'dep_1' }).warn({ amount_minor: 1250n, attempt: 2 }, 'retrying');
      span.end();
      return span;
    });

    const [record, ...rest] = records.getFinishedLogRecords();
    expect(rest).toEqual([]);
    expect(record?.spanContext).toMatchObject({
      traceId: span.spanContext().traceId,
      spanId: span.spanContext().spanId,
    });
    expect(record?.severityNumber).toBe(SeverityNumber.WARN);
    expect(record?.severityText).toBe('warn');
    expect(record?.body).toBe('retrying');
    // The line's own fields, by the names it has on stdout: not service and implementation (the
    // resource's), nor trace_id and span_id (the record's own), nor time, level and msg.
    expect(record?.attributes).toEqual({ deposit_id: 'dep_1', amount_minor: '1250', attempt: 2 });
    expect(record?.resource.attributes).toMatchObject({
      'service.name': 'deposit-worker',
      implementation: 'node',
    });
  });

  it('has no trace context outside a span', () => {
    capture().logger.info('idle');
    const [record] = records.getFinishedLogRecords();
    expect(record?.body).toBe('idle');
    expect(record?.spanContext).toBeUndefined();
  });

  it("maps each of our levels to OpenTelemetry's severity, fatal included", () => {
    const { logger } = capture('trace');
    for (const level of ['trace', 'debug', 'info', 'warn', 'error', 'fatal'] as const) {
      logger[level](level);
    }
    expect(records.getFinishedLogRecords().map((r) => [r.severityText, r.severityNumber])).toEqual([
      ['trace', SeverityNumber.TRACE],
      ['debug', SeverityNumber.DEBUG],
      ['info', SeverityNumber.INFO],
      ['warn', SeverityNumber.WARN],
      ['error', SeverityNumber.ERROR],
      ['fatal', SeverityNumber.FATAL],
    ]);
  });

  it("carries an error as OpenTelemetry's exception attributes", () => {
    class OverdraftError extends Error {}
    capture().logger.error({ err: new OverdraftError('insufficient funds') }, 'hold failed');
    const [record] = records.getFinishedLogRecords();
    expect(record?.attributes).toEqual({
      'exception.type': 'OverdraftError',
      'exception.message': 'insufficient funds',
      'exception.stacktrace': expect.stringContaining('insufficient funds\n    at '),
    });
  });

  it("below the logger's level is neither written nor exported", () => {
    const { logger, text } = capture('info');
    logger.debug('not this');
    expect(text()).toBe('');
    expect(records.getFinishedLogRecords()).toEqual([]);
  });
});

describe("stdout's line", () => {
  // The same clock for both loggers, so the lines' time is the same.
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-11-10T12:00:00.123Z'));
  });
  afterEach(() => {
    vi.useRealTimers();
    copyLogLines(true);
  });

  it('is byte for byte what it is without the copy', () => {
    copyLogLines(false);
    const plain = capture();
    copyLogLines(true);
    const copying = capture();
    tracer.startActiveSpan('work', (span) => {
      for (const { logger } of [plain, copying]) {
        logger.child({ deposit_id: 'dep_1' }).info({ amount_minor: 5n }, 'booked');
        logger.error({ err: new Error('boom') }, 'failed');
      }
      span.end();
    });
    expect(plain.text()).not.toBe('');
    expect(copying.text()).toBe(plain.text());
    // Only the copying logger's lines were exported.
    expect(records.getFinishedLogRecords()).toHaveLength(2);
  });
});
