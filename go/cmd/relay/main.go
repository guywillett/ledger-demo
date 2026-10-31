// Command relay publishes the outbox to JetStream (internal/relay). Several instances can run at
// once: claims are disjoint (FOR UPDATE SKIP LOCKED) and leased. Node: node/src/relay/main.ts and
// config.ts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/config"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/relay"
)

// relayConfig is the relay's environment, with Node's names, defaults and bounds.
//
// Defaults: a batch of 10 under a 30 s lease. The lease must outlast a batch's worst case, every
// publish timing out one after another: 10 × broker.PublishTimeout (2 s) = 20 s, plus leaseMargin
// for the database work in between = 25 s. The lease is also how long a crashed relay holds back
// the events it had claimed, so it stays short, and the batch size gives way instead.
type relayConfig struct {
	config.Shared
	NATS config.NATS
	// How many events one claim takes: 1 to 1000.
	BatchSize int
	// How long a claim holds its events: 1 s to 60 s. At most 60 s: a crashed relay's events are
	// published again when its lease expires, and the stream drops that repeat only within its
	// duplicate_window (120 s, nats/streams/DEPOSITS.json).
	Lease time.Duration
}

// leaseMargin is the time a batch spends on top of its publish timeouts, which the lease must also
// cover: the mark UPDATE after each publish and any wait for a pool connection.
const leaseMargin = 5 * time.Second

func loadConfig(lookup config.Lookup) (relayConfig, error) {
	p := config.NewParser(lookup)
	cfg := relayConfig{
		Shared:    config.ParseShared(p),
		NATS:      config.ParseNATS(p),
		BatchSize: p.Int("RELAY_BATCH_SIZE", 10, 1, 1000),
		Lease:     time.Duration(p.Int("RELAY_LEASE_MS", 30_000, 1000, 60_000)) * time.Millisecond,
	}
	worstBatch := time.Duration(cfg.BatchSize)*broker.PublishTimeout + leaseMargin
	if p.Err() == nil && cfg.Lease < worstBatch {
		p.Fail("RELAY_LEASE_MS", fmt.Sprintf("must be at least RELAY_BATCH_SIZE × %d ms "+
			"(the publish timeout) + %d ms (database work): a batch whose publishes all time out "+
			"would otherwise outlast its lease, and another relay would claim and publish the same "+
			"events while this one is still on them",
			broker.PublishTimeout.Milliseconds(), leaseMargin.Milliseconds()))
	}
	return cfg, p.Err()
}

// errNATSClosed is the shutdown's cause when the NATS connection closes for good on its own.
var errNATSClosed = errors.New("nats connection closed")

func main() {
	// os.Exit skips deferred calls, so run does the work and main turns its result into the exit
	// code (as in cmd/api).
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.LookupEnv)
	level := cfg.LogLevel
	if err != nil {
		level = "info" // LOG_LEVEL itself may be what's wrong
	}
	logger, logErr := logging.New(os.Stdout, "relay", level)
	if logErr != nil {
		logger, _ = logging.New(os.Stdout, "relay", "info")
	}
	fail := func(err error) error {
		logging.Fatal(context.Background(), logger, "startup failed", "err", err)
		return err
	}
	if err != nil {
		return fail(err)
	}

	// Cancelled by SIGTERM (docker compose stop) or SIGINT (Ctrl-C), as in cmd/api.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	conn, err := broker.Connect(cfg.NATS.URL, "ledger-demo-relay", logger)
	if err != nil {
		return fail(err)
	}
	for _, stream := range broker.Streams {
		if err := conn.AssertStreamExists(ctx, stream); err != nil {
			conn.NATS().Close() // nothing to drain yet
			return fail(err)
		}
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		conn.NATS().Close()
		return fail(err)
	}
	// Deferred, so it runs last: after the loop has stopped and NATS has been closed.
	defer pool.Close()
	publisher, err := broker.NewPublisher(conn, broker.PublishTimeout)
	if err != nil {
		conn.NATS().Close()
		return fail(err)
	}

	deps := relay.Deps{
		Pool:      pool,
		Publisher: publisher,
		Logger:    logger,
		BatchSize: cfg.BatchSize,
		Lease:     cfg.Lease,
		// A method value: Delay bound to PublishRetry, a func(int) time.Duration (Node passes an
		// arrow function, (attempts) => retryDelay(PUBLISH_RETRY, attempts)).
		RetryDelay: relay.PublishRetry.Delay,
	}

	// The loop's context: cancelled by a signal (through ctx, its parent) or, with errNATSClosed
	// as its cause, when the NATS connection closes for good outside a shutdown. Outside a
	// shutdown the connection only closes on a fatal error, and every publish would fail from
	// then on: shut down, and exit with a failing code so the container is restarted (as Node).
	polling, stopPolling := context.WithCancelCause(ctx)
	defer stopPolling(nil)
	// A goroutine waits for whichever comes first and logs the shutdown when it starts, while the
	// batch in progress may still be running; the WaitGroup lets run wait for it before closing
	// NATS (whose closing would otherwise look unexpected to it).
	var watcher sync.WaitGroup
	watcher.Go(func() {
		select {
		case <-conn.Closed():
			logging.Fatal(context.Background(), logger, "nats connection closed unexpectedly",
				"err", conn.LastError())
			stopPolling(errNATSClosed)
		case <-polling.Done():
		}
		// Shutdown begins: from here a second SIGTERM or SIGINT has its default effect and ends
		// the process at once (the batch in progress is then published again once its leases
		// expire), as Node's handlers, registered with process.once, allow. stop unregisters the
		// signal context, which would otherwise keep catching signals until run returns; the
		// deferred stop above is for the early returns, and calling it twice is harmless.
		stop()
		logger.Info("shutting down", "reason", context.Cause(polling).Error())
	})

	logger.Info("relay started", "batch_size", cfg.BatchSize, "lease_ms", cfg.Lease.Milliseconds())
	// Returns once polling is cancelled and the batch in progress has finished and marked its
	// events (see relay.Poll).
	relay.Poll(polling, deps)
	watcher.Wait()

	// Then drain NATS (giving up if it is unreachable), and the deferred pool.Close runs last.
	if !conn.Close(broker.DrainTimeout) {
		logger.Warn("nats connection closed without draining")
	}
	if errors.Is(context.Cause(polling), errNATSClosed) {
		return errNATSClosed
	}
	return nil
}
