import { trace } from '@opentelemetry/api';
import { expect, it, vi } from 'vitest';
import { capturingLogger } from '../helpers.ts';
import { shutdownTelemetry, startTelemetry } from '../../src/shared/telemetry.ts';

// The backend absent (decision 79): the real OTLP exporters, pointed at a port nothing listens on.
// The copy's exports fail, in the background; every line still reaches stdout, and shutdown still
// finishes. The exporters read the endpoint from the environment themselves, so it is set there.
vi.stubEnv('OTEL_EXPORTER_OTLP_ENDPOINT', 'http://127.0.0.1:9');
startTelemetry({ service: 'relay' });

it('leaves every line on stdout, and shutdown finishes', async () => {
  const { logger, lines } = capturingLogger();
  trace.getTracer('test').startActiveSpan('work', (span) => {
    for (let i = 0; i < 1000; i++) logger.info({ i }, 'line');
    span.end();
  });
  // Exports are attempted (and fail) meanwhile.
  await new Promise((resolve) => setTimeout(resolve, 200));
  await shutdownTelemetry();
  expect(lines().map((line) => line.i)).toEqual(Array.from({ length: 1000 }, (_, i) => i));
});
