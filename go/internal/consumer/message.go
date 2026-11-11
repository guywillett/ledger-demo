package consumer

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/propagation"

	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Message is what the delivery lifecycle needs from a message: its body, its transport details,
// and the four signals. Node: DeliveredMessage in consumer/messages.ts.
//
// An interface defined here, where it is used, not by nats.go: anything with these methods is a
// Message (implicitly, as a TypeScript structural type), so the tests pass a fake and the loop
// passes a JetStream message through a few-line adapter (Delivered). jetstream.Msg doesn't fit as
// it is: its delivery count is behind Metadata(), which can fail, and the adapter reads it once.
//
// The signals are as fire-and-forget as Node's: each sends one message to the server and returns
// without waiting for a reply. They can still fail to send (the connection is closed), which
// nats.js reports by throwing and nats.go by returning the error.
//
// Signal vs record, for the outcomes the delivery log records (see Lifecycle):
//   - done (Ack): record the outcome first, then ack. If recording fails, the message is naked
//     instead, and the redelivery finds the work done and completes the log. Acking first would
//     leave a crash trace (a stale in_progress row) for a delivery that succeeded.
//   - stop (Term) and retry later (Nak): signal first, then record, logging a failed write rather
//     than failing. A stop must hold whatever happens to the log: recording first and failing would
//     leave the message to be redelivered, repeating work (and alerts) that retrying can't help.
//     A missed write leaves a stale in_progress row instead, which reconciliation reports.
type Message interface {
	// Body is the message's data.
	Body() []byte
	// Subject is the subject it was published on (deposit.created, …).
	Subject() string
	// Header is its headers: Nats-Msg-Id, and the publish's trace context (traceparent, …) for the
	// processing span's link (slice 5 decision 82). Observation only (decision 83). Never nil.
	Header() nats.Header
	// NATSMsgID is the Nats-Msg-Id header (transport only), "" if there is none.
	NATSMsgID() string
	// DeliveryAttempt is 1 for the first delivery, then 2, 3, ... for redeliveries.
	DeliveryAttempt() int
	// Ack: done, don't deliver again.
	Ack() error
	// Term: invalid, never deliver again (redelivering can't help).
	Term(reason string) error
	// Nak: failed, deliver again after delay.
	Nak(delay time.Duration) error
	// Working: still working, reset the redelivery timer (ack_wait).
	Working() error
}

// jetStreamMessage adapts a JetStream message to Message.
type jetStreamMessage struct {
	msg     jetstream.Msg
	attempt int
}

// Delivered adapts a JetStream message to what the lifecycle needs, as Node's delivered(). The
// delivery count comes from the message's reply subject (Metadata), which every message a
// consumer delivers has; if it can't be read, the message counts as a first delivery.
func Delivered(m jetstream.Msg) Message {
	attempt := 1
	if meta, err := m.Metadata(); err == nil && meta.NumDelivered > 0 {
		attempt = int(meta.NumDelivered)
	}
	return jetStreamMessage{msg: m, attempt: attempt}
}

func (m jetStreamMessage) Body() []byte    { return m.msg.Data() }
func (m jetStreamMessage) Subject() string { return m.msg.Subject() }
func (m jetStreamMessage) Header() nats.Header {
	if h := m.msg.Headers(); h != nil {
		return h
	}
	return nats.Header{}
}
func (m jetStreamMessage) NATSMsgID() string {
	return m.msg.Headers().Get(jetstream.MsgIDHeader)
}
func (m jetStreamMessage) DeliveryAttempt() int          { return m.attempt }
func (m jetStreamMessage) Ack() error                    { return m.msg.Ack() }
func (m jetStreamMessage) Term(reason string) error      { return m.msg.TermWithReason(reason) }
func (m jetStreamMessage) Nak(delay time.Duration) error { return m.msg.NakWithDelay(delay) }
func (m jetStreamMessage) Working() error                { return m.msg.InProgress() }

// peekID is called by peekIDs and is the ID under idKey (event_id, deposit_id, …) of a message that may be invalid:
// nil if there is none to read (not JSON, not an object, no
// such string, or not a TypeID of kind K holding a UUIDv7).
func peekID[K ids.Kind](object map[string]json.RawMessage, idKey string) *ids.ID[K] {
	if object == nil {
		return nil
	}
	var s string
	if json.Unmarshal(object[idKey], &s) != nil {
		return nil
	}
	id, err := ids.FromTypeID[K](s)
	if err != nil {
		return nil
	}
	return &id
}

// peekIDs are the IDs under their JSON key (event_id, deposit_id, withdrawal_id) of a message that
// may be invalid, for the delivery log and the processing span: nil if there is none to read
// (not JSON, not an object, no such string, or not a TypeID of kind K holding a UUIDv7).
// Full validation comes after the delivery is recorded. Node: peekEventId and peekEntityIds.
func peekIDs(body []byte) (*ids.EventID, *ids.DepositID, *ids.WithdrawalID) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return nil, nil, nil
	}
	eventID := peekID[ids.Event](object, "event_id")
	depositID := peekID[ids.Deposit](object, "deposit_id")
	withdrawalID := peekID[ids.Withdrawal](object, "withdrawal_id")
	return eventID, depositID, withdrawalID
}

// natsHeaderCarrier adapts nats.Header to the propagators' carrier (propagation.TextMapCarrier, and
// ValuesGetter for a header sent more than once). Not propagation.HeaderCarrier: that is an
// http.Header, which canonicalises keys ("Traceparent"), while nats.Header is case-sensitive and
// both implementations write "traceparent".
type natsHeaderCarrier nats.Header

// Compile-time checks that natsHeaderCarrier is a TextMapCarrier and a ValuesGetter.
var (
	_ propagation.TextMapCarrier = natsHeaderCarrier{}
	_ propagation.ValuesGetter   = natsHeaderCarrier{}
)

// Get returns the first value associated with the passed key.
func (hc natsHeaderCarrier) Get(key string) string {
	return nats.Header(hc).Get(key)
}

// Values returns all values associated with the passed key.
func (hc natsHeaderCarrier) Values(key string) []string {
	return nats.Header(hc).Values(key)
}

// Set stores the key-value pair.
func (hc natsHeaderCarrier) Set(key, value string) {
	nats.Header(hc).Set(key, value)
}

// Keys lists the keys stored in this carrier.
func (hc natsHeaderCarrier) Keys() []string {
	return slices.Collect(maps.Keys(hc))
}
