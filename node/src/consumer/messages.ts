import type { JsMsg } from '@nats-io/jetstream';
import type { z } from 'zod';
import { fromTypeId, type Uuid } from '../shared/ids.ts';

// What a message handler needs from a message. A JetStream message is adapted to this by
// delivered() below; tests pass a fake. JetStream's ack methods are fire-and-forget: each sends
// one message to the server and returns at once.
//
// Signal vs record, in the handlers that also keep a delivery log (consumer/deliveries.ts):
// - done (ack): record the outcome first, then ack. If recording fails, the handler naks
//   instead, and the redelivery finds the work done and completes the log. Acking first would
//   leave a crash trace (a stale in_progress row) for a delivery that succeeded.
// - stop (term) and retry later (nak): signal first, then record, logging a failed write rather
//   than throwing. A stop must hold whatever happens to the log: recording first and failing
//   would leave the message to be redelivered, repeating work (and alerts) that retrying can't
//   help. A missed write leaves a stale in_progress row instead, which reconciliation reports.
export type DeliveredMessage = {
  readonly body: string;
  // The Nats-Msg-Id header (transport only).
  readonly natsMsgId: string | null;
  // 1 for the first delivery, then 2, 3, ... for redeliveries.
  readonly deliveryAttempt: number;
  // Done: don't deliver again.
  ack(): void;
  // Invalid: never deliver again (redelivering can't help).
  term(reason: string): void;
  // Failed: deliver again after the delay.
  nak(delayMs: number): void;
  // Still working: reset the redelivery timer (ack_wait).
  working(): void;
};

// Adapts a JetStream message to what a handler needs.
export function delivered(m: JsMsg): DeliveredMessage {
  return {
    body: m.string(),
    natsMsgId: m.headers?.get('Nats-Msg-Id') || null,
    deliveryAttempt: m.info.deliveryCount,
    ack: () => m.ack(),
    term: (reason) => m.term(reason),
    nak: (delayMs) => m.nak(delayMs),
    working: () => m.working(),
  };
}

// Parses a message body against an event schema: undefined if it is not JSON or not that event.
export function parseEvent<S extends z.ZodType>(schema: S, body: string): z.output<S> | undefined {
  let json: unknown;
  try {
    json = JSON.parse(body);
  } catch {
    return undefined;
  }
  const result = schema.safeParse(json);
  return result.success ? result.data : undefined;
}

// The event_id of a message that may be invalid, for the delivery log: null if there is none
// to read. Full validation comes after the delivery is recorded.
export function peekEventId(body: string): Uuid<'evt'> | null {
  try {
    const json: unknown = JSON.parse(body);
    if (typeof json === 'object' && json !== null && 'event_id' in json) {
      const value = json.event_id;
      if (typeof value === 'string') return fromTypeId('evt', value);
    }
  } catch {
    // Not JSON, or not an evt TypeID holding a UUIDv7.
  }
  return null;
}
