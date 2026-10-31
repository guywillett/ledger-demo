// Package broker is our use of NATS JetStream, the message broker: connecting (with Node's options
// and connection-event logging), the startup check that the streams exist, publishing events, and
// closing with a drain timeout. Node: node/src/shared/nats.ts.
//
// Named broker, not nats: it imports nats.go's packages, nats and jetstream, and a package of ours
// named after either would hide that import inside this package's own files (Go has one namespace
// for package names in a file) and read ambiguously everywhere else (nats.Connect: theirs or
// ours?). broker names the role, not the product.
//
// A foundation package (decision 57): it knows streams and subjects, not events or the outbox.
package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The streams, created by the nats-setup compose service from nats/streams/*.json, not by the
// processes (nats/setup.sh).
const (
	StreamDeposits    = "DEPOSITS"
	StreamWithdrawals = "WITHDRAWALS"
)

// Streams lists every stream, for the startup check.
var Streams = []string{StreamDeposits, StreamWithdrawals}

// StreamForSubject is the stream a subject is published to: one stream per entity, subjects
// <entity>.<status>. An unknown subject is a bug (an event nobody routed), an error that the relay
// records as that event's publish failure; its text is Node's (it is stored in outbox.last_error).
func StreamForSubject(subject string) (string, error) {
	switch {
	case strings.HasPrefix(subject, "deposit."):
		return StreamDeposits, nil
	case strings.HasPrefix(subject, "withdrawal."):
		return StreamWithdrawals, nil
	}
	return "", fmt.Errorf("no stream for subject %s", subject)
}

const (
	// PublishTimeout is how long a publish waits for the stream's acknowledgement before it
	// counts as failed.
	PublishTimeout = 2 * time.Second
	// DrainTimeout is how long Close lets the connection drain on shutdown.
	DrainTimeout = 5 * time.Second
)

// Conn is a NATS connection with a channel that is closed when the connection closes for good.
type Conn struct {
	nc     *nats.Conn
	closed chan struct{}
}

// Connect connects to url, as Node's connectNats: name shown in the server's connection list
// (nats server report connections, /connz), reconnecting for as long as the process runs
// (publishes fail, and are reported, meanwhile), and every connection event logged. A server
// that can't be reached at all fails the connect, as in Node (neither retries the first connect).
//
// nats.go reports connection events through callbacks given as options, each called on the
// connection's own goroutine; Node iterates nc.status(). The levels and nats_status values are
// Node's, mapped from the callbacks (nats.go has no ping event, and reports a stale connection as
// a disconnect with ErrStaleConnection, logged as both).
func Connect(url, name string, logger *slog.Logger) (*Conn, error) {
	c := &Conn{closed: make(chan struct{})}
	var once sync.Once
	problem := func(status string, err error) {
		args := []any{"nats_status", status}
		if err != nil {
			args = append(args, "err", err)
		}
		logger.Warn("nats connection problem", args...)
	}
	status := func(level slog.Level, status string) {
		logger.Log(context.Background(), level, "nats connection status", "nats_status", status)
	}
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1), // -1: for ever
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			// nats.go also calls this when the connection is closed on purpose (Close, or the
			// end of a drain); Node reports only the close then, and so does this.
			if nc.IsClosed() {
				return
			}
			if errors.Is(err, nats.ErrStaleConnection) {
				problem("staleConnection", nil)
			}
			problem("disconnect", err)
		}),
		nats.ReconnectHandler(func(*nats.Conn) { status(slog.LevelInfo, "reconnect") }),
		nats.ReconnectErrHandler(func(*nats.Conn, error) {
			status(slog.LevelDebug, "reconnecting")
		}),
		nats.LameDuckModeHandler(func(*nats.Conn) { status(slog.LevelDebug, "ldm") }),
		nats.DiscoveredServersHandler(func(*nats.Conn) { status(slog.LevelDebug, "update") }),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errors.Is(err, nats.ErrSlowConsumer) {
				problem("slowConsumer", nil)
				return
			}
			logger.Error("nats server error", "nats_status", "error", "err", err)
		}),
		nats.ClosedHandler(func(*nats.Conn) {
			status(slog.LevelInfo, "close")
			// close(ch) wakes every receiver, now and later: the channel is the "closed" signal.
			// Once, because closing a closed channel panics.
			once.Do(func() { close(c.closed) })
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS: %w", err)
	}
	c.nc = nc
	return c, nil
}

// Closed is closed when the connection has closed for good: after Close, or on a fatal error
// outside a shutdown (Node's nc.closed()). Receiving from it (<-c.Closed()) waits until then.
func (c *Conn) Closed() <-chan struct{} { return c.closed }

// LastError is the error that closed the connection, if any.
func (c *Conn) LastError() error { return c.nc.LastError() }

// NATS is the underlying connection, for what this package doesn't wrap (the tests read messages
// back from the streams with it).
func (c *Conn) NATS() *nats.Conn { return c.nc }

// AssertStreamExists fails fast at startup if a stream the process needs doesn't exist
// (nats-setup not run).
func (c *Conn) AssertStreamExists(ctx context.Context, stream string) error {
	js, err := jetstream.New(c.nc)
	if err != nil {
		return err
	}
	if _, err := js.Stream(ctx, stream); err != nil {
		return fmt.Errorf("JetStream stream %s not found; run the nats-setup service: %w",
			stream, err)
	}
	return nil
}

// Close closes the connection on shutdown, as Node's closeNats: Drain first, which lets pending
// publishes and in-flight messages finish, but only for timeout. If NATS is unreachable the
// connection is reconnecting (nats.go's Drain then closes it at once) or would never finish
// draining, so after the timeout it is closed outright. drained is false if it was closed without
// draining.
//
// nats.go's Drain only starts draining and returns; the connection closes when it is done, which
// the closed channel reports. Closed before the timeout means drained, as Node's closeNats counts
// drain() settling in time (LastError can't tell: it may still hold an earlier server error).
func (c *Conn) Close(timeout time.Duration) (drained bool) {
	if err := c.nc.Drain(); err != nil { // already closed, or reconnecting (and now closed)
		c.nc.Close()
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.closed:
		return true
	case <-timer.C:
		c.nc.Close()
		return false
	}
}

// PublishResult is the stream's acknowledgement: the stream it landed in, its sequence number
// there, and whether the stream already held its message ID (a repeat within the duplicate window,
// so it stored nothing new).
type PublishResult struct {
	Stream    string
	Seq       uint64
	Duplicate bool
}

// Publisher publishes events to JetStream, each to its subject's stream (StreamForSubject).
type Publisher struct {
	js      jetstream.JetStream
	timeout time.Duration
}

// NewPublisher publishes on c, each publish waiting at most timeout for its acknowledgement.
func NewPublisher(c *Conn, timeout time.Duration) (*Publisher, error) {
	js, err := jetstream.New(c.nc)
	if err != nil {
		return nil, err
	}
	return &Publisher{js: js, timeout: timeout}, nil
}

// Publish publishes payload (JSON) to subject and waits for the stream to acknowledge it. msgID
// becomes the Nats-Msg-Id header, which the stream uses to drop repeats within its duplicate
// window.
//
// The timeout is a deadline on the context (Node passes timeout: 2000): the publish gives up when
// either it or the caller's context runs out.
//
// While disconnected, nats.go keeps outgoing messages in its reconnect buffer and sends them once it
// reconnects, even a publish that has meanwhile timed out here; nats.js drops it. So after an
// outage the stream may already hold an event the relay recorded as failed, and the relay's retry
// is then acknowledged as a duplicate (stream_duplicate: true, measured), where Node's is a first
// publish. The msg_id makes the two equivalent: the stream stores the event once either way.
func (p *Publisher) Publish(
	ctx context.Context,
	subject string,
	payload []byte,
	msgID string,
) (PublishResult, error) {
	stream, err := StreamForSubject(subject)
	if err != nil {
		return PublishResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	ack, err := p.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: payload},
		jetstream.WithMsgID(msgID),
		// The server rejects the message unless it lands in this stream, so a subject that lands
		// in another stream, or in none, fails loudly instead of being stored elsewhere.
		jetstream.WithExpectStream(stream),
		// One attempt, as Node's js.publish (retries: 1). nats.go retries twice by default when no
		// stream answers; the relay's own retry (markFailed, then a later batch) covers that.
		jetstream.WithRetryAttempts(0),
	)
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Stream: ack.Stream, Seq: ack.Sequence, Duplicate: ack.Duplicate}, nil
}
