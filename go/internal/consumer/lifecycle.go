package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// The delivery lifecycle every consumer's handler shares, so each handler holds only its domain
// steps: record the delivery, decode the event, run the handler, then settle the message (ack, term
// or nak) and record the outcome in the order the signal-vs-record rule requires (see Message).
// Unexpected errors are classified once here: our bug, terminated with an alert, or transient,
// retried later. Node: consumer/delivery-log.ts.

// DeliveryRetry is the consumers' redelivery policy after a failed delivery (each says what
// triggers one): nak after 1 s, doubling per attempt, at most 60 s. Set on each nak rather than as
// a consumer backoff, because a consumer backoff also replaces ack_wait for messages still being
// processed. Lifecycle takes it as a field, so tests use their own.
var DeliveryRetry = backoff.Backoff{Base: time.Second, Max: 60 * time.Second}

// Settled is what a handler must return: proof that it settled the message. Only a Delivery's
// Done, Stop and RetryLater (or RetryAfter) make one with an outcome, as only db.InTx makes a
// db.Tx: the field is unexported, so no other package can set it.
//
// Go can't go as far as Node's branded type. Every path of a function must return, so a path that
// forgets to settle doesn't compile in either language; but Go lets any package write the zero
// value, Settled{}, which TypeScript's brand rules out. The lifecycle therefore checks at run time:
// a Settled with no outcome is a handler that never settled, treated as our bug. Settling twice is
// refused at run time, as in Node.
type Settled struct {
	outcome Outcome
}

// Outcome is the outcome the message was settled with.
func (s Settled) Outcome() Outcome { return s.outcome }

// Lifecycle is one consumer's delivery lifecycle for events of type E (Node: DeliveryLogOptions).
// A generic type: E is the decoded event, and each consumer instantiates it with its own, so its
// handler receives a typed event (Lifecycle[events.DepositCreatedV1] in the deposit-worker).
type Lifecycle[E any] struct {
	Consumer Name
	// For log lines and term reasons, e.g. "deposit.created".
	EventType string
	// Decode decodes and validates a message body as the event, hand-written as for request
	// bodies (decision 47): an error if it is not JSON or not that event.
	Decode func(body []byte) (E, error)
	// Pool is the pool: the delivery log's statements are each autocommitted.
	Pool   db.Querier
	Logger *slog.Logger
	Retry  backoff.Backoff
	// IsBug classifies an error the handler returned: true for our bug (retrying can't fix it),
	// false for a transient failure (e.g. the database went away).
	IsBug func(err error) bool
	// The error line for a transient failure ("processing failed; will retry" if empty).
	TransientFailureMessage string
}

// Handler is a consumer's domain steps for one event: it settles the message through d on every
// path, and returns an error for anything it doesn't deal with itself (classified by IsBug).
type Handler[E any] func(ctx context.Context, event E, d *Delivery) (Settled, error)

// Delivery is one message's delivery, as a handler sees it (Node: DeliveryContext): its logger,
// the message (for Working during long steps), and the three ways to settle it.
//
// Used by one goroutine, the handler's, so it needs no locking.
type Delivery struct {
	log     *slog.Logger
	message Message
	pool    db.Querier
	retry   backoff.Backoff
	id      DeliveryID
	// The outcome the message was settled with, once it has been: a message is settled once.
	settledWith Outcome
}

// Log is the delivery's logger: nats_msg_id, delivery_attempt, event_id, plus whatever Bind added.
func (d *Delivery) Log() *slog.Logger { return d.log }

// Message is the message, for Working during long steps.
func (d *Delivery) Message() Message { return d.message }

// Bind adds fields (e.g. "deposit_id", id) to every later log line, the lifecycle's own included.
func (d *Delivery) Bind(args ...any) { d.log = d.log.With(args...) }

// settle marks the message settled, or refuses a second settle (Node throws).
func (d *Delivery) settle(outcome Outcome) error {
	if d.settledWith != "" {
		return fmt.Errorf("message already settled (%s); can't settle it as %s",
			d.settledWith, outcome)
	}
	d.settledWith = outcome
	return nil
}

// recordTimeout bounds each write of a delivery's outcome (see finish).
const recordTimeout = 5 * time.Second

// finish writes the delivery's outcome on a context of its own: not cancelled with the handler's
// (context.WithoutCancel), since when shutdown cancels a handler (see Consuming.Stop) its outcome
// should still reach the log, and work it has committed should still be acked; but bounded, so a
// database that stops answering can't hold shutdown for ever.
func (d *Delivery) finish(ctx context.Context, outcome Outcome) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return FinishDelivery(ctx, d.pool, d.id, outcome)
}

// record records a stop's or retry's outcome after its signal, best-effort: a failed write is
// logged, never returned (see Message).
func (d *Delivery) record(ctx context.Context, outcome Outcome) {
	if err := d.finish(ctx, outcome); err != nil {
		d.log.Error("could not record the delivery outcome", "err", err, "outcome", outcome)
	}
}

// Done records the outcome (Applied or Duplicate), then acks. If recording fails, the error is
// returned and the lifecycle retries the message (the handler returns the error), whose redelivery
// finds the work done. An error too if the message was already settled, or if the ack couldn't be
// sent (after which it counts as settled: the lifecycle logs the error, and doesn't settle again).
func (d *Delivery) Done(ctx context.Context, outcome Outcome) (Settled, error) {
	if outcome != Applied && outcome != Duplicate {
		return Settled{}, fmt.Errorf("%w: done with outcome %s (only applied or duplicate)",
			errHandlerBug, outcome)
	}
	if d.settledWith != "" {
		return Settled{}, d.settle(outcome) // refused
	}
	if err := d.finish(ctx, outcome); err != nil {
		return Settled{}, err
	}
	if err := d.settle(outcome); err != nil {
		return Settled{}, err
	}
	if err := d.message.Ack(); err != nil {
		return Settled{outcome}, fmt.Errorf("ack: %w", err)
	}
	return Settled{outcome}, nil
}

// Stop terminates the message (never deliver again) with a reason, then records the outcome
// (Invalid or Failed), best-effort.
func (d *Delivery) Stop(ctx context.Context, outcome Outcome, reason string) (Settled, error) {
	if outcome != Invalid && outcome != Failed {
		return Settled{}, fmt.Errorf("%w: stop with outcome %s (only invalid or failed)",
			errHandlerBug, outcome)
	}
	if err := d.settle(outcome); err != nil {
		return Settled{}, err
	}
	termErr := d.message.Term(reason)
	d.record(ctx, outcome)
	if termErr != nil {
		return Settled{outcome}, fmt.Errorf("term: %w", termErr)
	}
	return Settled{outcome}, nil
}

// RetryLater naks the message, to be delivered again after the backoff for this delivery attempt,
// then records Failed, best-effort.
func (d *Delivery) RetryLater(ctx context.Context) (Settled, error) {
	return d.RetryAfter(ctx, d.retry.Delay(d.message.DeliveryAttempt()))
}

// RetryAfter is RetryLater with a delay of the handler's choosing (Node: retryLater(delayMs)).
func (d *Delivery) RetryAfter(ctx context.Context, delay time.Duration) (Settled, error) {
	if err := d.settle(Failed); err != nil {
		return Settled{}, err
	}
	nakErr := d.message.Nak(delay)
	d.record(ctx, Failed)
	if nakErr != nil {
		return Settled{Failed}, fmt.Errorf("nak: %w", nakErr)
	}
	return Settled{Failed}, nil
}

// errHandlerBug marks a handler's misuse of its Delivery (settling with the wrong outcome, or
// returning without settling): our bug whatever the consumer's IsBug says, so never retried.
var errHandlerBug = errors.New("handler bug")

// errNotSettled is a handler that returned no error and no Settled.
var errNotSettled = fmt.Errorf("%w: it returned without settling the message", errHandlerBug)

// Deliver runs one message through the lifecycle and returns the outcome it was settled with.
//
// A method on the generic type: it uses the type's E, so the handler's event is typed.
func (l Lifecycle[E]) Deliver(ctx context.Context, m Message, handle Handler[E]) Outcome {
	// Log the delivery before anything else, with the event_id if it can be read at all.
	peeked := peekEventID(m.Body())
	var msgID any // JSON null for a message without one, as Node logs
	if id := m.NATSMsgID(); id != "" {
		msgID = id
	}
	log := l.Logger.With("nats_msg_id", msgID, "delivery_attempt", m.DeliveryAttempt())
	if peeked != nil {
		log = log.With("event_id", *peeked)
	}

	id, err := StartDelivery(ctx, l.Pool, NewDelivery{
		Consumer:        l.Consumer,
		EventID:         peeked,
		NATSMsgID:       m.NATSMsgID(),
		DeliveryAttempt: m.DeliveryAttempt(),
	})
	if err != nil {
		// Can't even record it (e.g. the database is down): try again later. A nak that can't be
		// sent leaves the message to be redelivered when its ack_wait runs out, the same end.
		log.Error("could not record delivery; will retry", "err", err)
		_ = m.Nak(l.Retry.Delay(m.DeliveryAttempt()))
		return Failed
	}
	d := &Delivery{log: log, message: m, pool: l.Pool, retry: l.Retry, id: id}

	settled, err := l.run(ctx, d, handle)
	if err == nil && settled.outcome == "" {
		err = errNotSettled
	}
	if err == nil {
		return d.settledWith
	}
	// Already settled (e.g. an error after Done, or a second settle): never signal twice.
	if d.settledWith != "" {
		d.log.Error("error after the message was settled; not settling it again", "err", err)
		return d.settledWith
	}
	if errors.Is(err, errHandlerBug) || l.IsBug(err) {
		// A bug, not a transient failure: retrying can't fix it, so don't loop on it. The entity
		// stays where it stopped (reconciliation shows it); someone has to look.
		logging.Fatal(ctx, d.log, "invariant violated; terminating the message",
			"err", err, "alert", true)
		return d.settleAgain(d.Stop(ctx, Failed, "invariant violation"))
	}
	// Transient (e.g. the database went away mid-way). Committed steps stay committed; the
	// redelivery carries on from there.
	message := l.TransientFailureMessage
	if message == "" {
		message = "processing failed; will retry"
	}
	d.log.Error(message, "err", err)
	return d.settleAgain(d.RetryLater(ctx))
}

// PanicError is a panic in a handler (or a decode function), recovered by the lifecycle and
// handled as an error the handler returned: classified by IsBug, as Node's withDeliveryLog
// classifies whatever a handler throws (a TypeError included). Stack is where it panicked.
type PanicError struct {
	Value any
	Stack string
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// run decodes the event and runs the handler. An invalid message is terminated: redelivering it
// can't fix it.
//
// A panic becomes a *PanicError (the named results let the deferred function set them): recover
// only works in a deferred call, and returns the panic's value, nil if there was none.
func (l Lifecycle[E]) run(
	ctx context.Context,
	d *Delivery,
	handle Handler[E],
) (settled Settled, err error) {
	defer func() {
		if r := recover(); r != nil {
			settled, err = Settled{}, &PanicError{Value: r, Stack: string(debug.Stack())}
		}
	}()
	event, err := l.Decode(d.message.Body())
	if err != nil {
		d.log.Error(fmt.Sprintf("invalid %s event; terminating it", l.EventType), "err", err)
		return d.Stop(ctx, Invalid, "invalid "+l.EventType+" event")
	}
	return handle(ctx, event, d)
}

// settleAgain is the outcome of the lifecycle's own settle (Stop or RetryLater, taking their two
// results as its arguments), a signal that couldn't be sent logged as Node's catch-all would.
func (d *Delivery) settleAgain(s Settled, err error) Outcome {
	if err != nil {
		d.log.Error("unhandled error while handling a message", "err", err)
	}
	return s.outcome
}
