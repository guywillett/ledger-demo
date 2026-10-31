package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
)

// The pull loop shared by the processes that consume a durable JetStream consumer: handles up to
// Concurrency messages at once. Several instances can share the consumer: JetStream gives each
// message to one of them. Node: consumer/consume.ts.
//
// Messages are pulled only when there's a free slot (fetch exactly the free slots), so every
// message a process holds is being handled: its handler starts, and with it any Working signals,
// as soon as it arrives. (A prefetching consumer would hold messages in a buffer with their
// ack_wait running and nobody signalling, and redeliver them if the handlers ahead take longer
// than ack_wait.)

// ConsumeOptions configures the pull loop. A zero duration, backoff or count means its default (Go
// style: the zero value is useful), so a caller sets only what it changes.
type ConsumeOptions struct {
	Stream      string
	Consumer    Name
	Concurrency int
	Logger      *slog.Logger
	// Handle handles one message, acking, naking or terminating it itself (Lifecycle.Deliver). It
	// runs on its own goroutine, with a context cancelled only if shutdown's grace period runs out
	// (see Stop). A panic is logged as a last resort; handlers deal with their own errors.
	Handle func(ctx context.Context, m Message)
	// An idle fetch long-polls about this long on the server before ending empty (default 5 s);
	// the loop then fetches again. A message arriving meanwhile is delivered at once.
	FetchExpires time.Duration
	// After a failed fetch: wait 1 s, doubling up to 30 s, before the next one (default).
	FetchRetry backoff.Backoff
	// A missing consumer or stream is a configuration problem (nats-setup not run, or the consumer
	// deleted): after this many attempts in a row (default 5), the loop ends with an error.
	MaxMissingAttempts int
	// On Stop, how long in-flight messages get to finish (default 5 s).
	ShutdownGrace time.Duration
}

func (o *ConsumeOptions) setDefaults() {
	if o.FetchExpires == 0 {
		o.FetchExpires = 5 * time.Second
	}
	if o.FetchRetry == (backoff.Backoff{}) {
		o.FetchRetry = backoff.Backoff{Base: time.Second, Max: 30 * time.Second}
	}
	if o.MaxMissingAttempts == 0 {
		o.MaxMissingAttempts = 5
	}
	if o.ShutdownGrace == 0 {
		o.ShutdownGrace = 5 * time.Second
	}
}

// Consuming is a running pull loop (Node: Consuming).
type Consuming struct {
	o        ConsumeOptions
	consumer jetstream.Consumer

	// slots is the semaphore: a buffered channel with a place per slot. Sending takes a slot
	// (blocking while all Concurrency places are taken), receiving gives one back. Go has no
	// semaphore type in its standard library; a buffered channel is the usual one.
	slots chan struct{}
	// inFlight counts running handlers (for logs), handlers waits for them.
	inFlight atomic.Int64
	handlers sync.WaitGroup

	// stopping is cancelled by Stop: it ends a waiting fetch (its context), a retry wait and a
	// wait for a free slot, all at once.
	stopping    context.Context
	stopPulling context.CancelFunc
	// handlerCtx is the handlers' context, cancelled only when the grace period runs out.
	handlerCtx     context.Context
	cancelHandlers context.CancelFunc

	done     chan struct{} // closed when the loop has ended
	err      error         // why it ended, set before done is closed
	stopOnce sync.Once
}

// StartConsuming looks up the consumer (an error if it doesn't exist: a startup failure for the
// caller; it is bound, never created, decision 40), then starts the loop on its own goroutine.
func StartConsuming(
	ctx context.Context,
	js jetstream.JetStream,
	o ConsumeOptions,
) (*Consuming, error) {
	o.setDefaults()
	if o.Concurrency < 1 || o.Handle == nil || o.Logger == nil {
		return nil, fmt.Errorf("%s consumer: Concurrency (at least 1), Handle and Logger are required",
			o.Consumer)
	}
	consumer, err := js.Consumer(ctx, o.Stream, string(o.Consumer))
	if err != nil {
		return nil, fmt.Errorf("%s consumer on stream %s: %w", o.Consumer, o.Stream, err)
	}
	c := &Consuming{
		o:        o,
		consumer: consumer,
		slots:    make(chan struct{}, o.Concurrency),
		done:     make(chan struct{}),
	}
	// Neither context descends from ctx: the loop and its handlers stop only through Stop, never
	// because the caller's context (e.g. the signal's) ends. WithoutCancel keeps ctx's values.
	c.stopping, c.stopPulling = context.WithCancel(context.WithoutCancel(ctx))
	c.handlerCtx, c.cancelHandlers = context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		c.err = c.loop()
		close(c.done)
	}()
	return c, nil
}

// Done is closed when the loop has ended: after Stop, or with an error (Err) if the consumer stays
// missing.
func (c *Consuming) Done() <-chan struct{} { return c.done }

// Err is why the loop ended: nil after Stop. Read it once Done is closed.
func (c *Consuming) Err() error { return c.err }

// InFlight is how many messages are being handled right now.
func (c *Consuming) InFlight() int { return int(c.inFlight.Load()) }

// loop fetches exactly as many messages as there are free slots and starts a handler for each as
// it arrives, without waiting for it; when every slot is busy, it waits for one to free up instead
// of fetching. Returns nil after Stop, an error if the consumer stays missing.
func (c *Consuming) loop() error {
	failures, missing := 0, 0
	for c.stopping.Err() == nil {
		free, ok := c.takeFreeSlots()
		if !ok {
			return nil
		}
		received, err := c.fetch(free)
		for range free - received { // the slots no message came for
			<-c.slots
		}
		if err == nil {
			failures, missing = 0, 0
			continue
		}
		if c.stopping.Err() != nil {
			continue // a fetch Stop ended: the loop's condition ends the loop, without an error
		}
		if c.consumerMissing() {
			missing++
			c.o.Logger.Error(fmt.Sprintf("%s consumer or its stream not found", c.o.Consumer),
				"err", err, "attempt", missing)
			if missing >= c.o.MaxMissingAttempts {
				return fmt.Errorf("%s consumer not found after %d attempts: %w",
					c.o.Consumer, missing, err)
			}
		} else {
			missing = 0
			failures++
			c.o.Logger.Warn("fetch failed; retrying", "err", err, "attempt", failures)
		}
		backoff.Pause(c.stopping, c.o.FetchRetry.Delay(failures+missing))
	}
	return nil
}

// takeFreeSlots takes one slot, waiting while all are busy (or until Stop: ok false), then every
// other free one without waiting, and returns how many it took.
func (c *Consuming) takeFreeSlots() (n int, ok bool) {
	select {
	case c.slots <- struct{}{}:
	case <-c.stopping.Done():
		return 0, false
	}
	n = 1
	for n < c.o.Concurrency {
		select {
		case c.slots <- struct{}{}:
			n++
		default: // none free: fetch for those taken
			return n, true
		}
	}
	return n, true
}

// fetch pulls up to n messages and dispatches each as it arrives, returning how many came. A fetch
// that ends empty when it expires, or because Stop ended it, is no error.
//
// Its context ends it: cancelled by Stop at once, or by its deadline. nats.go derives the server's
// expiry from that deadline (10% earlier, at most 1 s), so the server normally ends the fetch
// first, and allows FetchContext or FetchMaxWait, not both. The idle heartbeat (the server's
// signal that the connection is alive during a long poll) is 2/5 of the expiry: Node uses half,
// but nats.go requires an expiry of at least twice the heartbeat, and the expiry it sends is
// shortened.
func (c *Consuming) fetch(n int) (received int, err error) {
	ctx, cancel := context.WithTimeout(c.stopping, c.o.FetchExpires)
	defer cancel()
	batch, err := c.consumer.Fetch(n,
		jetstream.FetchContext(ctx), jetstream.FetchHeartbeat(c.o.FetchExpires*2/5))
	if err != nil {
		return 0, err
	}
	// Messages is a channel that nats.go closes when the fetch ends; range receives until then.
	for msg := range batch.Messages() {
		received++
		c.dispatch(msg)
	}
	if err := batch.Error(); err != nil && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) {
		return received, err
	}
	return received, nil
}

// dispatch handles a message on a goroutine of its own, which holds one of the slots taken for the
// fetch and gives it back when the handler returns. A goroutine per message is the Go way (they
// are cheap: a few KB of stack each); the semaphore bounds how many run.
func (c *Consuming) dispatch(m jetstream.Msg) {
	c.inFlight.Add(1)
	c.handlers.Add(1)
	go func() {
		defer func() {
			c.inFlight.Add(-1)
			<-c.slots
			c.handlers.Done()
		}()
		defer func() {
			// The last resort: Lifecycle.Deliver recovers a handler's panic and settles the message
			// itself, so this catches only a panic outside it, which would otherwise crash the
			// process. Its message isn't settled, so JetStream redelivers it when its ack_wait runs
			// out (Node: the rejection logged).
			if r := recover(); r != nil {
				c.o.Logger.Error("unhandled error while handling a message",
					"err", fmt.Errorf("panic: %v", r))
			}
		}()
		c.o.Handle(c.handlerCtx, Delivered(m))
	}()
}

// consumerMissing tells a missing consumer or stream (a configuration problem) from anything else
// after a failed fetch (transient, e.g. NATS reconnecting, in which case this lookup fails too).
func (c *Consuming) consumerMissing() bool {
	ctx, cancel := context.WithTimeout(c.stopping, 5*time.Second)
	defer cancel()
	_, err := c.consumer.Info(ctx)
	return errors.Is(err, jetstream.ErrConsumerNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound)
}

// Stop stops pulling (ends a waiting fetch at once, cuts short a retry wait), then waits up to the
// grace period for in-flight handlers. Safe to call more than once, and from several goroutines:
// sync.Once runs stop once, and later callers wait for it to finish.
//
// When the grace period runs out, the handlers still running have their context cancelled, and
// Stop waits for them to return. Node can't do this (a promise can't be cancelled from outside),
// so its unfinished handlers run on while the process exits around them. Here a cancelled handler's
// query is cancelled and its transaction rolls back: the same state a crash leaves, reached
// cleanly, with the connection returned to the pool before it closes. The handler then settles as
// any transient failure (a nak with the retry delay, recorded), so the message is redelivered
// after that delay rather than after its ack_wait. Handlers must return once their context is
// cancelled: every one of their I/O calls takes it, and the lifecycle's own writes of the outcome
// are bounded (recordTimeout). There is no second limit on this wait: pgxpool's Close waits for
// every connection to come back anyway, so a limit here would only move a hang there (a database
// that stops answering is a known gap, with compose's SIGKILL as the backstop).
func (c *Consuming) Stop() {
	c.stopOnce.Do(func() {
		c.stopPulling()
		<-c.done // no more dispatches after this, so waiting on handlers is safe
		finished := make(chan struct{})
		go func() {
			c.handlers.Wait()
			close(finished)
		}()
		timer := time.NewTimer(c.o.ShutdownGrace)
		defer timer.Stop()
		select {
		case <-finished:
			return
		case <-timer.C:
		}
		c.o.Logger.Warn("unfinished messages will be redelivered", "in_flight", c.InFlight())
		c.cancelHandlers()
		<-finished
	})
}
