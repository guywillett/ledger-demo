// Command submitter sends each held withdrawal to the provider: it pulls withdrawal.held messages
// from the durable submitter consumer on WITHDRAWALS (created by nats-setup, never by the process:
// decision 40) and handles up to SUBMITTER_CONCURRENCY at once. Several instances can share the
// consumer: JetStream gives each message to one of them. An unfinished submission is redelivered on
// shutdown, and asks again with the same key. The process lifecycle is consumer.Run's, shared with
// the deposit-worker. Node: node/src/submitter/main.ts and config.ts.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/config"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/provider"
	"github.com/guywillett/ledger-demo/go/internal/submitter"
)

// submitterConfig is the submitter's environment, with Node's names, defaults and bounds:
// consumer.BaseConfig embedded (DATABASE_URL, LOG_LEVEL, NATS_URL, SUBMITTER_CONCURRENCY), and its
// own two.
type submitterConfig struct {
	consumer.BaseConfig
	// The provider's base URL (provider-sim in compose).
	ProviderURL string
	// A provider call that hasn't answered by then is abandoned: its outcome is unknown, and the
	// message is redelivered later to try again with the same key. 100 ms to 120 s, default 5 s;
	// also checked against the consumer's ack_wait at startup (checkTimeoutFitsAckWait).
	ProviderTimeout time.Duration
}

func loadConfig(p *config.Parser) submitterConfig {
	return submitterConfig{
		BaseConfig:  consumer.ParseBase(p, "SUBMITTER_CONCURRENCY"),
		ProviderURL: p.URL("PROVIDER_URL"),
		ProviderTimeout: time.Duration(p.Int("PROVIDER_TIMEOUT_MS", 5000, 100, 120_000)) *
			time.Millisecond,
	}
}

// A provider call must end well inside the submitter consumer's ack_wait: at most this share of
// it. The ack_wait comes from NATS at startup, not a copy here, so changing
// nats/consumers/WITHDRAWALS/submitter.json can't silently outdate the check.
const maxShareOfAckWait = 2.0 / 3

// serverDefaultAckWait is NATS's ack_wait when a consumer doesn't set one.
const serverDefaultAckWait = 30 * time.Second

// checkTimeoutFitsAckWait fails startup unless a provider call fits well inside the consumer's
// ack_wait (Node's assertTimeoutFitsAckWait, with its message). A call that outlived its message's
// lease would see the message redelivered to another instance while it is still waiting: a second
// call with the same key, at the same time. Harmless to the books (the provider's idempotency and
// the conditional transitions decide), but some providers answer a concurrent request with the same
// key with a 409, which the submitter treats as a bug.
func checkTimeoutFitsAckWait(timeout, ackWait time.Duration) error {
	limit := int64(float64(ackWait.Milliseconds()) * maxShareOfAckWait) // Math.floor, as Node
	if timeout.Milliseconds() > limit {
		return fmt.Errorf("PROVIDER_TIMEOUT_MS (%d ms) must be at most %d ms, two thirds of the "+
			"submitter consumer's ack_wait (%d ms): a call that outlives its message's lease would "+
			"see the message redelivered to another instance, and the withdrawal sent again, while "+
			"it is still waiting", timeout.Milliseconds(), limit, ackWait.Milliseconds())
	}
	return nil
}

// startupCheck reads the submitter consumer's real ack_wait from NATS (an error if the consumer
// doesn't exist) and checks the provider timeout against it.
func startupCheck(ctx context.Context, conn *broker.Conn, cfg submitterConfig) error {
	js, err := jetstream.New(conn.NATS())
	if err != nil {
		return err
	}
	c, err := js.Consumer(ctx, broker.StreamWithdrawals, string(consumer.Submitter))
	if err != nil {
		return fmt.Errorf("submitter consumer: %w", err)
	}
	ackWait := c.CachedInfo().Config.AckWait
	if ackWait == 0 { // unset: the server's default
		ackWait = serverDefaultAckWait
	}
	return checkTimeoutFitsAckWait(cfg.ProviderTimeout, ackWait)
}

// process is the submitter as consumer.Run runs it.
var process = consumer.Process[submitterConfig]{
	Service:      "submitter",
	Stream:       broker.StreamWithdrawals,
	Consumer:     consumer.Submitter,
	LoadConfig:   loadConfig,
	StartupCheck: startupCheck,
	NewHandler: func(
		cfg submitterConfig,
		pool *pgxpool.Pool,
		logger *slog.Logger,
	) func(context.Context, consumer.Message) {
		// The URL was validated with the configuration, so NewClient can't fail here; checked all
		// the same, as a startup panic (a programmer error) if it ever does.
		client, err := provider.NewClient(cfg.ProviderURL, cfg.ProviderTimeout, cfg.Concurrency)
		if err != nil {
			panic(err)
		}
		deps := submitter.Deps{
			Pool:     pool,
			Logger:   logger,
			Provider: client,
			Retry:    consumer.DeliveryRetry,
		}
		return func(ctx context.Context, m consumer.Message) { submitter.Handle(ctx, deps, m) }
	},
	StartedFields: func(cfg submitterConfig) []any {
		return []any{"provider_timeout_ms", cfg.ProviderTimeout.Milliseconds()}
	},
}

func main() {
	if err := consumer.Run(process); err != nil {
		os.Exit(1)
	}
}
