// Command api is the HTTP API: the client API and the provider webhooks. It needs only Postgres:
// events go to the outbox, and the relay publishes them. Node: node/src/api/main.ts.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/api"
	"github.com/guywillett/ledger-demo/go/internal/config"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// apiConfig is the API's environment, with Node's names and defaults (node/src/api/config.ts).
type apiConfig struct {
	config.Shared
	Port int
}

func loadConfig() (apiConfig, error) {
	p := config.NewParser(os.LookupEnv)
	cfg := apiConfig{
		Shared: config.ParseShared(p),
		Port:   p.Int("PORT", 3000, 1, 65535),
	}
	return cfg, p.Err()
}

// shutdownGrace is how long in-flight requests get to finish after SIGTERM: inside compose's
// default 10 s stop timeout, after which it sends SIGKILL, with room to close the pool.
const shutdownGrace = 5 * time.Second

func main() {
	// os.Exit skips deferred calls, so run does the work (its defers run when it returns) and main
	// only turns its result into the exit code.
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	level := cfg.LogLevel
	if err != nil {
		level = "info" // LOG_LEVEL itself may be what's wrong
	}
	logger, logErr := logging.New(os.Stdout, "api", level)
	if logErr != nil {
		logger, _ = logging.New(os.Stdout, "api", "info")
	}
	// One structured line instead of a raw error or stack trace, then exit 1 (the container is
	// restarted), as Node's "startup failed".
	fail := func(err error) error {
		logging.Fatal(context.Background(), logger, "startup failed", "err", err)
		return err
	}
	if err != nil {
		return fail(err)
	}

	// signal.NotifyContext returns a context that is cancelled when one of the signals arrives:
	// docker compose stop sends SIGTERM, Ctrl-C sends SIGINT. Cancellation is how Go asks
	// long-running work to stop (context.Context, first parameter of anything that does I/O).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(err)
	}
	defer pool.Close()
	// Deferred calls run last-in first-out: cancelRequests runs before pool.Close, so no request
	// is left holding a connection Close would wait for.
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	server := &http.Server{
		Handler: api.NewHandler(pool, logger),
		// Timeouts. net/http has none by default, so one slow or idle client could hold a
		// connection (a goroutine and a file descriptor) open for ever.
		//
		// ReadHeaderTimeout: the request line and headers, a few hundred bytes, must arrive within
		// 10 s. Without it a client sending a byte at a time (slowloris) keeps the connection
		// open; gosec flags its absence. Node's default (headersTimeout) is 60 s.
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout: the whole request, body included (at most 1 MiB, Node's body limit).
		ReadTimeout: 30 * time.Second,
		// WriteTimeout: a deadline on writing the response, from the end of the headers. It cuts the
		// connection, but doesn't stop the handler or its query: that's the request deadline
		// (api.NewHandler, 30 s), which answers 500. This is longer, so that 500 can still be sent.
		WriteTimeout: 35 * time.Second,
		// IdleTimeout: how long a keep-alive connection waits for its next request, as Fastify's
		// keepAliveTimeout (72 s, longer than common load balancers' 60 s idle timeout, so they
		// close first and never send a request on a connection the server is closing).
		IdleTimeout: 72 * time.Second,
		// The server's own errors (a TLS handshake, a malformed request) through the same logger.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
		// Every request's context derives from this one, cancelled if the shutdown grace period
		// runs out (below): the requests still running are then cancelled, and so are their
		// queries, so the pool can close.
		BaseContext: func(net.Listener) context.Context { return requests },
	}

	// Listen first, so a port already in use is a startup failure, then serve in a goroutine:
	// Serve blocks until the server stops.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.Port)))
	if err != nil {
		return fail(err)
	}
	logger.Info("server listening", "address", listener.Addr().String())
	served := make(chan error, 1) // buffered: Serve's goroutine never blocks on sending its result
	go func() { served <- server.Serve(listener) }()

	// select waits for whichever happens first: a signal, or the server stopping by itself.
	select {
	case err := <-served:
		return fail(err)
	case <-ctx.Done():
	}
	logger.Info("shutting down", "reason", context.Cause(ctx).Error())

	// Shutdown stops accepting connections and waits for in-flight requests, up to the grace
	// period; a new context, since ctx is already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// The grace period ran out with requests still running: cancel them (and their queries),
		// rather than let pool.Close wait for them until compose sends SIGKILL.
		cancelRequests()
		logger.Error("shutdown failed", "err", err)
		return err
	}
	// Serve returns ErrServerClosed once Shutdown is called: the expected end, not a failure.
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "err", err)
		return err
	}
	return nil // the deferred pool.Close() runs now, after the last request
}
