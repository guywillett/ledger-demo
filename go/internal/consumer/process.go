package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/config"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// The process lifecycle shared by every consumer process (the deposit-worker, the submitter):
// load the configuration, connect, check what the process needs, start the pull loop, shut down
// gracefully on a signal, and fail (exit code 1) when something goes wrong for good, so the
// container is restarted. Each cmd/<consumer> only describes its process: what differs between
// consumers is exactly what Process holds. Node: consumer/process.ts.

// BaseConfig is the configuration every consumer process has. A process's own config struct embeds
// it (Go's embedding: its fields and methods become the outer struct's, so cfg.DatabaseURL works,
// and so does cfg.Base()), adding its own fields.
type BaseConfig struct {
	config.Shared
	NATS config.NATS
	// How many messages the process handles at once.
	Concurrency int
}

// Base is the embedded BaseConfig: through embedding, every process's config struct has this
// method, which is what Process's type constraint asks for.
func (c BaseConfig) Base() BaseConfig { return c }

// ParseBase reads DATABASE_URL, LOG_LEVEL, NATS_URL and the process's concurrency variable (1 to
// 1000, default 10, as Node's DEPOSIT_WORKER_CONCURRENCY and SUBMITTER_CONCURRENCY).
func ParseBase(p *config.Parser, concurrencyVar string) BaseConfig {
	return BaseConfig{
		Shared:      config.ParseShared(p),
		NATS:        config.ParseNATS(p),
		Concurrency: p.Int(concurrencyVar, 10, 1, 1000),
	}
}

// Process describes one consumer process. Generic over its config type C, so each callback gets
// the process's own config, typed; the constraint says C must have the Base method, which
// embedding BaseConfig gives it.
type Process[C interface{ Base() BaseConfig }] struct {
	// Names the logger's service, the NATS connection (ledger-demo-<service>) and the
	// "<service> started" line.
	Service  string
	Stream   string
	Consumer Name
	// LoadConfig reads the process's configuration; the parser collects every problem, reported
	// together as a startup failure.
	LoadConfig func(p *config.Parser) C
	// StartupCheck (optional) runs once NATS is connected and the stream exists, before anything
	// else is created; an error is a startup failure.
	StartupCheck func(ctx context.Context, conn *broker.Conn, cfg C) error
	// NewHandler builds the message handler, given the process's pool and logger.
	NewHandler func(cfg C, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context, Message)
	// StartedFields (optional) adds fields to the "<service> started" line (concurrency is always
	// there).
	StartedFields func(cfg C) []any

	// logOutput is where the logs go: os.Stdout, unless a test in this package captures them.
	logOutput io.Writer
}

// Shutdown causes other than a signal, each also logged as fatal and failing the process.
var (
	errNATSClosed = errors.New("nats connection closed")
	errLoopFailed = errors.New("consume loop failed")
)

// Run runs the process until it stops: after a signal's graceful shutdown (nil), or a failure for
// good (an error; the caller exits with code 1). A function rather than a method on Process: Go
// doesn't allow a method to have type parameters of its own, and this keeps the call
// consumer.Run(consumer.Process[cfg]{...}) reading like Node's runConsumerProcess({...}).
func Run[C interface{ Base() BaseConfig }](p Process[C]) error {
	parser := config.NewParser(os.LookupEnv)
	cfg := p.LoadConfig(parser)
	base := cfg.Base()
	cfgErr := parser.Err()
	level := base.LogLevel
	if cfgErr != nil {
		level = "info" // LOG_LEVEL itself may be what's wrong
	}
	out := p.logOutput
	if out == nil {
		out = os.Stdout
	}
	logger, err := logging.New(out, p.Service, level)
	if err != nil {
		logger, _ = logging.New(out, p.Service, "info")
	}
	// Nothing is running yet that a graceful shutdown would protect: log and return at once, as
	// Node's "startup failed" exits.
	fail := func(err error) error {
		logging.Fatal(context.Background(), logger, "startup failed", "err", err)
		return err
	}
	if cfgErr != nil {
		return fail(cfgErr)
	}

	// Cancelled by SIGTERM (docker compose stop) or SIGINT (Ctrl-C), as in cmd/api.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	conn, err := broker.Connect(base.NATS.URL, "ledger-demo-"+p.Service, logger)
	if err != nil {
		return fail(err)
	}
	closeNATS := func() { conn.NATS().Close() } // before anything needs draining
	if err := conn.AssertStreamExists(ctx, p.Stream); err != nil {
		closeNATS()
		return fail(err)
	}
	if p.StartupCheck != nil {
		if err := p.StartupCheck(ctx, conn, cfg); err != nil {
			closeNATS()
			return fail(err)
		}
	}
	pool, err := db.Connect(ctx, base.DatabaseURL)
	if err != nil {
		closeNATS()
		return fail(err)
	}
	// Deferred, so it runs last: after the loop's handlers have returned and NATS has drained.
	defer pool.Close()
	js, err := jetstream.New(conn.NATS())
	if err != nil {
		closeNATS()
		return fail(err)
	}
	// An error if the consumer doesn't exist (nats-setup not run).
	consuming, err := StartConsuming(ctx, js, ConsumeOptions{
		Stream:      p.Stream,
		Consumer:    p.Consumer,
		Concurrency: base.Concurrency,
		Logger:      logger,
		Handle:      p.NewHandler(cfg, pool, logger),
	})
	if err != nil {
		closeNATS()
		return fail(err)
	}

	started := []any{"concurrency", base.Concurrency}
	if p.StartedFields != nil {
		started = append(started, p.StartedFields(cfg)...)
	}
	logger.Info(p.Service+" started", started...)

	// Wait for whichever comes first: a signal, the connection closing for good (outside a
	// shutdown it only closes on a fatal error), or the loop failing (the consumer stayed
	// missing). The last two fail the process, so the container is restarted.
	var cause error
	select {
	case <-ctx.Done():
	case <-conn.Closed():
		logging.Fatal(context.Background(), logger, "nats connection closed unexpectedly",
			"err", conn.LastError())
		cause = errNATSClosed
	case <-consuming.Done():
		logging.Fatal(context.Background(), logger, "consume loop failed", "err", consuming.Err())
		cause = errLoopFailed
	}
	// From here a second SIGTERM or SIGINT gets the default behaviour, ending the process at once,
	// as Node's handlers (process.once) do: stop unregisters the signal context, which would
	// otherwise keep catching signals until Run returns.
	stop()
	reason := context.Cause(ctx)
	if cause != nil {
		reason = cause
	}
	logger.Info("shutting down", "reason", reason.Error(), "in_flight", consuming.InFlight())

	// Stop the loop (in-flight messages get their grace period; see Consuming.Stop), then drain
	// NATS (the handlers' last signals go out with it), then close the pool (deferred).
	consuming.Stop()
	if !conn.Close(broker.DrainTimeout) {
		logger.Warn("nats connection closed without draining")
	}
	return cause
}
