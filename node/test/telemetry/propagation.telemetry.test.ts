import type { AddressInfo } from 'node:net';
import { SpanKind, type SpanContext } from '@opentelemetry/api';
import { jetstreamManager } from '@nats-io/jetstream';
import { InMemorySpanExporter, type ReadableSpan } from '@opentelemetry/sdk-trace';
import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { z } from 'zod';
import { startTelemetry } from '../../src/shared/telemetry.ts';

// Trace context through the outbox and NATS (slice 5 decisions 80-83), with an in-memory exporter:
// a request enqueues an event, the relay publishes it, a consumer processes it, and each span links
// to the one before, request <- publish <- process. The consumer layer (withDeliveryLog) is given
// the message as the stream stored it. Then a redelivery, an event without trace context, a
// malformed traceparent, and a republish (still dropped by the stream: decision 83).
//
// Telemetry starts as the relay's (W3C propagators, both ways) before anything that loads pg or
// node:http. Runs after the relay tests (vitest.config.ts): it drains the outbox, so its events are
// the only ones a batch claims.
const spans = new InMemorySpanExporter();
startTelemetry({ service: 'relay', spanExporter: spans });

const helpers = await import('../helpers.ts');
const { relayBatch } = await import('../../src/relay/relay.ts');
const { withDeliveryLog, DELIVERY_RETRY } = await import('../../src/consumer/delivery-log.ts');
const { STREAMS } = await import('../../src/shared/nats.ts');

const testApp = await helpers.createTestApp();
const { app, pool } = testApp;
await app.listen({ host: '127.0.0.1', port: 0 });
const base = `http://127.0.0.1:${(app.server.address() as AddressInfo).port}`;
const nc = await helpers.connectTestNats();
const stream = helpers.testStreamPublisher(nc);
afterAll(async () => {
  await nc.drain();
  await testApp.close();
});

beforeEach(async () => {
  await helpers.drainOutbox(pool, stream);
  spans.reset();
});

const named = (name: string): ReadableSpan[] =>
  spans.getFinishedSpans().filter((s) => s.name === name);

const linkedTo = (span: ReadableSpan | undefined): SpanContext[] =>
  span?.links.map((l) => l.context) ?? [];

const sameSpan = (a: SpanContext | undefined, b: SpanContext | undefined): boolean =>
  a !== undefined && b !== undefined && a.traceId === b.traceId && a.spanId === b.spanId;

// The incoming-payment webhook, over HTTP: its server span is the request's trace.
async function notify(): Promise<{ ntx: string; request: ReadableSpan }> {
  const body = helpers.notificationBody();
  const response = await fetch(`${base}/provider/incoming-payments`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  expect(response.status).toBe(204);
  await vi.waitFor(() => expect(named('POST /provider/incoming-payments')).toHaveLength(1));
  const [request] = named('POST /provider/incoming-payments');
  if (request === undefined) throw new Error('no request span');
  return { ntx: String(body.network_transaction_id), request };
}

// Publishes the claimed events through a recording publisher, as the relay does.
async function relay(): Promise<ReturnType<typeof helpers.recordingPublisher>['published']> {
  const { publisher, published } = helpers.recordingPublisher(stream);
  await relayBatch(helpers.testRelayDeps(pool, publisher));
  return published;
}

// The message as the stream stored it: subject, body and headers (the trace context with them).
async function stored(seq: number): Promise<{
  subject: string;
  body: string;
  headers: Record<string, string>;
}> {
  const jsm = await jetstreamManager(nc);
  const m = await jsm.streams.getMessage(STREAMS.deposits, { seq });
  if (m === null) throw new Error(`no message ${seq}`);
  const headers: Record<string, string> = {};
  for (const name of m.header.keys()) headers[name] = m.header.get(name);
  return { subject: m.subject, body: m.string(), headers };
}

// Runs the message through the consumer layer (the delivery log, a handler that only settles).
async function processMessage(
  message: { subject: string; body: string; headers: Record<string, string> },
  deliveryAttempt = 1,
): Promise<string> {
  const { logger } = helpers.capturingLogger();
  return withDeliveryLog(
    {
      consumer: 'deposit-worker',
      eventType: 'deposit.created',
      schema: z.object({ event_id: z.string() }),
      pool,
      logger,
      retry: DELIVERY_RETRY,
      isBug: () => false,
    },
    {
      ...message,
      natsMsgId: message.headers['Nats-Msg-Id'] ?? null,
      deliveryAttempt,
      ack: () => {},
      term: () => {},
      nak: () => {},
      working: () => {},
    },
    async (_event, ctx) => ctx.done('applied'),
  );
}

describe('a request, its event published, the event processed', () => {
  it('link: publish -> request, process -> publish; the message carries the context', async () => {
    const { request } = await notify();
    const [event] = await relay();
    if (event === undefined) throw new Error('nothing published');

    // The relay: a trace of its own per batch; the publish in it, linked to the request.
    const [batch] = named('relay batch');
    const [publish] = named('publish deposit.created');
    expect(batch?.parentSpanContext).toBeUndefined();
    expect(batch?.spanContext().traceId).not.toBe(request.spanContext().traceId);
    expect(publish?.kind).toBe(SpanKind.PRODUCER);
    expect(publish?.parentSpanContext?.spanId).toBe(batch?.spanContext().spanId);
    expect(linkedTo(publish)).toHaveLength(1);
    expect(sameSpan(linkedTo(publish)[0], request.spanContext())).toBe(true);
    expect(publish?.attributes).toMatchObject({
      'messaging.system': 'nats',
      'messaging.operation.name': 'publish',
      'messaging.operation.type': 'send',
      'messaging.destination.name': 'deposit.created',
      'messaging.message.id': event.msgId,
      event_id: event.event.event_id,
      deposit_id: event.event.deposit_id,
    });
    // The marks' statements are the batch's children (the step 1 rule: inside a span only).
    const marks = spans
      .getFinishedSpans()
      .filter(
        (s) =>
          s.kind === SpanKind.CLIENT && s.parentSpanContext?.spanId === batch?.spanContext().spanId,
      );
    expect(marks.map((s) => s.name)).toContain('UPDATE');

    // The stored message: Nats-Msg-Id as ever, and the publish span's context beside it.
    const message = await stored(event.result.seq);
    expect(message.headers['Nats-Msg-Id']).toBe(event.msgId);
    expect(message.headers.traceparent).toBe(
      `00-${publish?.spanContext().traceId}-${publish?.spanContext().spanId}-01`,
    );

    // The consumer: a root span of its own, linked to the publish; the delivery's statements in it.
    expect(await processMessage(message)).toBe('applied');
    const [process] = named('process deposit.created');
    expect(process?.kind).toBe(SpanKind.CONSUMER);
    expect(process?.parentSpanContext).toBeUndefined();
    expect(linkedTo(process)).toHaveLength(1);
    expect(sameSpan(linkedTo(process)[0], publish?.spanContext())).toBe(true);
    expect(process?.attributes).toMatchObject({
      'messaging.system': 'nats',
      'messaging.operation.name': 'process',
      'messaging.operation.type': 'process',
      'messaging.destination.name': 'deposit.created',
      'messaging.consumer.group.name': 'deposit-worker',
      'messaging.message.id': event.msgId,
      delivery_attempt: 1,
      event_id: event.event.event_id,
      deposit_id: event.event.deposit_id,
      delivery_outcome: 'applied',
    });
    const statements = spans
      .getFinishedSpans()
      .filter((s) => s.parentSpanContext?.spanId === process?.spanContext().spanId);
    expect(statements.map((s) => s.name)).toEqual(['INSERT', 'UPDATE']); // the delivery log
  });

  it('gives a redelivery a processing span of its own, linked to the same publish', async () => {
    await notify();
    const [event] = await relay();
    if (event === undefined) throw new Error('nothing published');
    const message = await stored(event.result.seq);
    await processMessage(message, 1);
    await processMessage(message, 2);

    const [publish] = named('publish deposit.created');
    const processes = named('process deposit.created');
    expect(processes.map((s) => s.attributes.delivery_attempt)).toEqual([1, 2]);
    for (const process of processes) {
      expect(sameSpan(linkedTo(process)[0], publish?.spanContext())).toBe(true);
    }
    expect(processes[0]?.spanContext().traceId).not.toBe(processes[1]?.spanContext().traceId);
  });
});

describe('without trace context, or with a malformed one', () => {
  it('stores none for an event enqueued outside a traced context; it publishes and processes normally, unlinked', async () => {
    const account = await helpers.createTestAccount(pool);
    const deposit = await helpers.createTestDeposit(pool, account); // no span: no context
    const row = await pool.query<{ trace_context: unknown }>(
      'SELECT trace_context FROM outbox WHERE entity_id = $1',
      [deposit.id],
    );
    expect(row.rows).toEqual([{ trace_context: null }]);

    const [event] = await relay();
    if (event === undefined) throw new Error('nothing published');
    expect(event.result.duplicate).toBe(false);
    expect(linkedTo(named('publish deposit.created')[0])).toEqual([]);

    const { traceparent: _publishContext, ...withoutContext } = (await stored(event.result.seq))
      .headers;
    const message = { ...(await stored(event.result.seq)), headers: withoutContext };
    expect(await processMessage(message)).toBe('applied');
    expect(linkedTo(named('process deposit.created')[0])).toEqual([]);
  });

  it('processes a message with a malformed traceparent normally: no link, no error', async () => {
    await notify();
    const [event] = await relay();
    if (event === undefined) throw new Error('nothing published');
    const message = await stored(event.result.seq);
    const malformed = {
      ...message,
      headers: { ...message.headers, traceparent: 'not-a-traceparent' },
    };

    expect(await processMessage(malformed)).toBe('applied');
    const [process] = named('process deposit.created');
    expect(linkedTo(process)).toEqual([]);
    expect(process?.status.code).not.toBe(2); // not ERROR
  });
});

describe('Nats-Msg-Id (decision 83)', () => {
  it('is untouched by trace context: a republish with other trace headers is dropped by the stream', async () => {
    await notify();
    const [event] = await relay();
    if (event === undefined) throw new Error('nothing published');
    const again = await stream.publish(event.subject, event.event, event.msgId, {
      traceparent: '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01',
    });
    expect(again).toMatchObject({ duplicate: true, seq: event.result.seq });
  });
});
