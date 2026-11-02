// Command deposit-worker books, reviews and posts each deposit: it pulls deposit.created messages
// from the durable deposit-worker consumer on DEPOSITS (created by nats-setup from nats/consumers/,
// never by the process: decision 40) and handles up to DEPOSIT_WORKER_CONCURRENCY at once. Several
// instances can share the consumer: JetStream gives each message to one of them. The process
// lifecycle (connect, pull loop, signals, graceful shutdown, exit codes) is consumer.Run's, shared
// with the submitter. Node: node/src/deposit-worker/main.ts and config.ts.
package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/compliance"
	"github.com/guywillett/ledger-demo/go/internal/config"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/depositworker"
)

// workingInterval: Working at least this often during a review, well under the consumer's 10 s
// ack_wait (nats/consumers/DEPOSITS/deposit-worker.json).
const workingInterval = 3 * time.Second

// workerConfig is the deposit-worker's environment, with Node's names, defaults and bounds:
// consumer.BaseConfig embedded (DATABASE_URL, LOG_LEVEL, NATS_URL, DEPOSIT_WORKER_CONCURRENCY), and
// its own two.
type workerConfig struct {
	consumer.BaseConfig
	// How long the pseudo compliance review takes: 0 to 60 s, default 2 s.
	ReviewDelay time.Duration
	// The probability of approving a deposit (the rest are rejected): 0 to 1, default 0.8.
	ApproveRate float64
}

func loadConfig(p *config.Parser) workerConfig {
	return workerConfig{
		BaseConfig:  consumer.ParseBase(p, "DEPOSIT_WORKER_CONCURRENCY"),
		ReviewDelay: time.Duration(p.Int("REVIEW_DELAY_MS", 2000, 0, 60_000)) * time.Millisecond,
		ApproveRate: p.Float("APPROVE_RATE", 0.8, 0, 1),
	}
}

// process is the deposit-worker as consumer.Run runs it.
var process = consumer.Process[workerConfig]{
	Service:    "deposit-worker",
	Stream:     broker.StreamDeposits,
	Consumer:   consumer.DepositWorker,
	LoadConfig: loadConfig,
	NewHandler: func(
		cfg workerConfig,
		pool *pgxpool.Pool,
		logger *slog.Logger,
	) func(context.Context, consumer.Message) {
		deps := depositworker.Deps{
			Pool:   pool,
			Logger: logger,
			Decide: func() compliance.Decision {
				return compliance.DecideRandomly(cfg.ApproveRate, rand.Float64)
			},
			ReviewDelay:     cfg.ReviewDelay,
			WorkingInterval: workingInterval,
			Retry:           consumer.DeliveryRetry,
		}
		return func(ctx context.Context, m consumer.Message) { depositworker.Handle(ctx, deps, m) }
	},
}

func main() {
	if err := consumer.Run(process); err != nil {
		os.Exit(1)
	}
}
