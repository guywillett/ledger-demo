import type { JsMsg } from '@nats-io/jetstream';

// What a message handler needs from a message. A JetStream message is adapted to this by
// delivered() below; tests pass a fake. JetStream's ack methods are fire-and-forget.
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
