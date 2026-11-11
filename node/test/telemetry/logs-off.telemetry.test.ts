import { trace } from '@opentelemetry/api';
import { InMemoryLogRecordExporter } from '@opentelemetry/sdk-logs';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { expect, it } from 'vitest';
import { capturingLogger } from '../helpers.ts';
import { startTelemetry } from '../../src/shared/telemetry.ts';

// OTEL_LOGS_EXPORTER=none (slice 5 decision 93): stdout only, where an agent ships stdout. Traces
// are still exported; no log record is, even with a log exporter given.
const records = new InMemoryLogRecordExporter();
const spans = new InMemorySpanExporter();
startTelemetry({
  service: 'api',
  env: { OTEL_LOGS_EXPORTER: 'none' },
  spanExporter: spans,
  logExporter: records,
});

it('exports no log record, and stdout has every line with its trace and span IDs', () => {
  const { logger, lines } = capturingLogger();
  trace.getTracer('test').startActiveSpan('work', (span) => {
    logger.info('one');
    logger.warn('two');
    span.end();
  });
  expect(records.getFinishedLogRecords()).toEqual([]);
  expect(spans.getFinishedSpans()).toHaveLength(1);
  expect(lines().map((line) => [line.msg, typeof line.trace_id])).toEqual([
    ['one', 'string'],
    ['two', 'string'],
  ]);
});
