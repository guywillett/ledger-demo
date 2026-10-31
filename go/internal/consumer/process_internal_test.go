package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/config"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Run, the process runner, against the test database and NATS: its startup failures, and a whole
// run from start to a signal's shutdown. In the package itself (not consumer_test) to capture the
// logs through Process's unexported logOutput. The stream and consumers are these tests' own, as
// in consume_test.go.

// syncBuffer is a log destination safe for the process's goroutines and the test's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			found = append(found, m)
		}
	}
	return found
}

func (b *syncBuffer) line(msg string) map[string]any {
	for _, l := range b.lines() {
		if l["msg"] == msg {
			return l
		}
	}
	return nil
}

// testConfig is a process's own config: BaseConfig embedded, plus a field of its own.
type testConfig struct {
	BaseConfig
	Greeting string
}

// setEnv points the process at the test services (t.Setenv restores the variables after).
func setEnv(t *testing.T) string {
	t.Helper()
	natsURL := os.Getenv("TEST_NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4223"
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://ledger:ledger@localhost:5432/ledger_go_test?sslmode=disable"
	}
	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("LOG_LEVEL", "info")
	return natsURL
}

// testProcessConsumer creates the test stream and a consumer of the test's own, returning the
// JetStream context, its name and its subject.
func testProcessConsumer(t *testing.T, natsURL string) (jetstream.JetStream, Name, string) {
	t.Helper()
	conn, err := broker.Connect(natsURL, "ledger-demo-go-test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(broker.DrainTimeout) })
	js, err := jetstream.New(conn.NATS())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: "GO_CONSUMER_TEST", Subjects: []string{"go-consumer-test.>"},
		Storage: jetstream.MemoryStorage, MaxAge: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := strings.ToLower(strings.TrimPrefix(ids.New[ids.Event]().TypeID(), "evt_"))
	name, subject := Name("go-test-"+token), "go-consumer-test."+token
	_, err = js.CreateConsumer(ctx, "GO_CONSUMER_TEST", jetstream.ConsumerConfig{
		Durable: string(name), FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), "GO_CONSUMER_TEST", string(name)) })
	return js, name, subject
}

func testProcess(
	consumer Name,
	out *syncBuffer,
	handle func(context.Context, Message),
) Process[testConfig] {
	return Process[testConfig]{
		Service:  "test-worker",
		Stream:   "GO_CONSUMER_TEST",
		Consumer: consumer,
		LoadConfig: func(p *config.Parser) testConfig {
			return testConfig{BaseConfig: ParseBase(p, "TEST_WORKER_CONCURRENCY"), Greeting: "hello"}
		},
		NewHandler: func(cfg testConfig, _ *pgxpool.Pool, _ *slog.Logger) func(context.Context, Message) {
			if cfg.Greeting != "hello" || cfg.Concurrency != 3 {
				panic("the handler got the wrong config")
			}
			return handle
		},
		StartedFields: func(cfg testConfig) []any { return []any{"greeting", cfg.Greeting} },
		logOutput:     out,
	}
}

func TestRunFailsAtStartupWithAStartupFailedLine(t *testing.T) {
	natsURL := setEnv(t)
	t.Setenv("TEST_WORKER_CONCURRENCY", "3")
	_, name, _ := testProcessConsumer(t, natsURL)
	noop := func(context.Context, Message) {}

	tests := []struct {
		name  string
		setup func(p *Process[testConfig])
		want  string
	}{
		{"a bad config", func(*Process[testConfig]) {
			t.Setenv("TEST_WORKER_CONCURRENCY", "0")
		}, "TEST_WORKER_CONCURRENCY must be a whole number from 1 to 1000"},
		{"a missing consumer", func(p *Process[testConfig]) {
			p.Consumer = "go-test-no-such-consumer"
		}, "go-test-no-such-consumer consumer on stream GO_CONSUMER_TEST"},
		{"a failing startup check", func(p *Process[testConfig]) {
			p.StartupCheck = func(context.Context, *broker.Conn, testConfig) error {
				return errors.New("timeout exceeds ack_wait")
			}
		}, "timeout exceeds ack_wait"},
	}
	for _, tt := range tests {
		t.Setenv("TEST_WORKER_CONCURRENCY", "3")
		out := &syncBuffer{}
		p := testProcess(name, out, noop)
		tt.setup(&p)
		err := Run(p)
		line := out.line("startup failed")
		if err == nil || line == nil || line["level"] != "fatal" {
			t.Errorf("%s: err %v, lines %v", tt.name, err, out.lines())
			continue
		}
		if e, _ := line["err"].(map[string]any); !strings.Contains(e["message"].(string), tt.want) {
			t.Errorf("%s: err = %v", tt.name, line["err"])
		}
	}
}

// A whole run: started, a message handled, then SIGTERM: stopped cleanly (nil, so exit code 0).
func TestRunHandlesMessagesAndShutsDownOnASignal(t *testing.T) {
	natsURL := setEnv(t)
	t.Setenv("TEST_WORKER_CONCURRENCY", "3")
	js, name, subject := testProcessConsumer(t, natsURL)
	out := &syncBuffer{}
	handled := make(chan string, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- Run(testProcess(name, out, func(_ context.Context, m Message) {
			_ = m.Ack()
			handled <- string(m.Body())
		}))
	}()

	// Only once it has started is the signal handler in place (a SIGTERM before would end the
	// test binary itself).
	deadline := time.Now().Add(10 * time.Second)
	for out.line("test-worker started") == nil {
		if time.Now().After(deadline) {
			t.Fatalf("not started: %v", out.lines())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if line := out.line("test-worker started"); line["concurrency"] != 3.0 || line["greeting"] != "hello" {
		t.Errorf("started line = %v", line)
	}
	if _, err := js.Publish(context.Background(), subject, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-handled:
		if body != "hello" {
			t.Errorf("handled %q", body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the message wasn't handled")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run didn't return after SIGTERM")
	}
	line := out.line("shutting down")
	if line == nil || line["in_flight"] != 0.0 || !strings.Contains(line["reason"].(string), "terminated") {
		t.Errorf("shutting down line = %v", line)
	}
	if out.line("nats connection closed without draining") != nil {
		t.Errorf("didn't drain: %v", out.lines())
	}
}
