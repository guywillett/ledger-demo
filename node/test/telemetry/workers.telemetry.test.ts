import { createServer, type IncomingHttpHeaders } from 'node:http';
import type { AddressInfo } from 'node:net';
import { trace } from '@opentelemetry/api';
import { jetstreamManager } from '@nats-io/jetstream';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace';
import { afterAll, describe, expect, it } from 'vitest';
import { startTelemetry } from '../../src/shared/telemetry.ts';

// The workers' telemetry with an in-memory exporter: an idle relay and an idle consumer make no
// spans (slice 5 decision 85), and the submitter's provider calls carry no trace headers (decision
// 84). As workers' processes do, telemetry starts before anything that loads pg (the dynamic
// imports below).
const spans = new InMemorySpanExporter();
startTelemetry({ service: 'relay', spanExporter: spans });

const helpers = await import('../helpers.ts');
const { relayBatch } = await import('../../src/relay/relay.ts');
const { startConsuming } = await import('../../src/consumer/consume.ts');
const { providerClient } = await import('../../src/submitter/provider.ts');
const { createLogger } = await import('../../src/shared/logger.ts');

const testApp = await helpers.createTestApp();
const { pool } = testApp;
const nc = await helpers.connectTestNats();
afterAll(async () => {
  await nc.drain();
  await testApp.close();
});

describe('an idle relay', () => {
  it('makes no spans: a claim that finds nothing, and its query, are not traced', async () => {
    const publisher = helpers.testStreamPublisher(nc);
    await helpers.drainOutbox(pool, publisher);
    spans.reset();

    for (let i = 0; i < 3; i++) {
      expect((await relayBatch(helpers.testRelayDeps(pool, publisher))).claimed).toBe(0);
    }
    expect(spans.getFinishedSpans()).toEqual([]);
  });
});

describe('an idle consumer', () => {
  // A stream and consumer of its own, so nothing else's messages arrive: every fetch is empty.
  const STREAM = 'TELEMETRY_IDLE_TEST';

  it('makes no spans through its empty fetches', async () => {
    const jsm = await jetstreamManager(nc);
    await jsm.streams.add({ name: STREAM, subjects: ['telemetry-idle-test.>'] });
    try {
      await jsm.consumers.add(STREAM, { durable_name: 'idle', ack_policy: 'explicit' });
      spans.reset();
      const consuming = await startConsuming({
        nc,
        stream: STREAM,
        consumer: 'idle',
        concurrency: 1,
        logger: createLogger({ service: 'test', level: 'silent' }),
        handle: async () => {
          throw new Error('no message was expected');
        },
        fetchExpiresMs: 200, // several empty fetches in the time below
      });
      await new Promise((resolve) => setTimeout(resolve, 1000));
      await consuming.stop();
      expect(spans.getFinishedSpans()).toEqual([]);
    } finally {
      await jsm.streams.delete(STREAM);
    }
  });
});

describe("the submitter's provider calls", () => {
  it('carry no trace headers, even from inside a span (decision 84)', async () => {
    const received: IncomingHttpHeaders[] = [];
    const provider = createServer((req, res) => {
      received.push(req.headers);
      res.writeHead(201, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ transfer_id: 'tr_1', status: 'accepted' }));
    });
    await new Promise<void>((resolve) => provider.listen(0, '127.0.0.1', resolve));
    try {
      const { port } = provider.address() as AddressInfo;
      const client = providerClient({ baseUrl: `http://127.0.0.1:${port}`, timeoutMs: 2000 });
      const account = await helpers.createTestAccount(pool);
      await helpers.fundTestAccount(pool, account, 1000n);
      const withdrawal = await helpers.createTestWithdrawal(pool, account, 400n);

      const tracer = trace.getTracer('test');
      const result = await tracer.startActiveSpan('a delivery', async (span) => {
        try {
          return await client.submitTransfer(withdrawal);
        } finally {
          span.end();
        }
      });

      expect(result).toEqual({ kind: 'accepted', transferId: 'tr_1' });
      expect(received).toHaveLength(1);
      for (const header of ['traceparent', 'tracestate', 'baggage']) {
        expect(received[0]).not.toHaveProperty(header);
      }
    } finally {
      await new Promise<void>((resolve) => provider.close(() => resolve()));
    }
  });
});
