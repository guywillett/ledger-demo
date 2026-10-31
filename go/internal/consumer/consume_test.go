package consumer_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/broker"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// The pull loop against the test NATS (compose nats-test). Node tests these behaviours only end
// to end (test/e2e/deposit-worker.e2e.ts); here the loop is tested on its own, with handlers that
// block, take long, ignore their message or panic.
//
// Kept apart from everything else on the test NATS: a stream of these tests' own,
// GO_CONSUMER_TEST (subjects go-consumer-test.>, in memory), never DEPOSITS or WITHDRAWALS, so
// neither Node's tests nor the relay tests' consumers or purges ever see these messages; and each
// test creates its own consumer on a subject of its own, deleted when the test ends. Decision 40
// (consumers are created by nats-setup, never by the processes) is about production: here the
// test is the setup.

const (
	testStream        = "GO_CONSUMER_TEST"
	testSubjectPrefix = "go-consumer-test."
)

// testConsumer is a JetStream consumer of the test's own on the test stream, and what it needs to
// publish to it.
type testConsumer struct {
	js      jetstream.JetStream
	name    consumer.Name
	subject string
}

func newTestConsumer(t *testing.T, ackWait time.Duration) testConsumer {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		url = "nats://localhost:4223"
	}
	conn, err := broker.Connect(url, "ledger-demo-go-test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("test NATS unreachable (is `docker compose up -d` running?): %v", err)
	}
	t.Cleanup(func() { conn.Close(broker.DrainTimeout) })
	js, err := jetstream.New(conn.NATS())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     testStream,
		Subjects: []string{testSubjectPrefix + ">"},
		Storage:  jetstream.MemoryStorage,
		MaxAge:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := strings.ToLower(strings.TrimPrefix(ids.New[ids.Event]().TypeID(), "evt_"))
	tc := testConsumer{js: js, name: consumer.Name("go-test-" + token), subject: testSubjectPrefix + token}
	_, err = js.CreateConsumer(ctx, testStream, jetstream.ConsumerConfig{
		Durable:       string(tc.name),
		FilterSubject: tc.subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteConsumer(context.Background(), testStream, string(tc.name)) })
	return tc
}

func (tc testConsumer) publish(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		if _, err := tc.js.Publish(context.Background(), tc.subject, []byte{byte('0' + i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func (tc testConsumer) info(t *testing.T) *jetstream.ConsumerInfo {
	t.Helper()
	c, err := tc.js.Consumer(context.Background(), testStream, string(tc.name))
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func (tc testConsumer) start(t *testing.T, o consumer.ConsumeOptions) *consumer.Consuming {
	t.Helper()
	o.Stream, o.Consumer = testStream, tc.name
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c, err := consumer.StartConsuming(context.Background(), tc.js, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	return c
}

// eventually waits until cond holds, failing the test after timeout.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// handled records each message a handler took, with its delivery attempt.
type handled struct {
	mu       sync.Mutex
	bodies   []string
	attempts []int
}

func (h *handled) add(m consumer.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, string(m.Body()))
	h.attempts = append(h.attempts, m.DeliveryAttempt())
}

func (h *handled) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func TestFetchesOnlyAsManyMessagesAsThereAreFreeSlots(t *testing.T) {
	tc := newTestConsumer(t, 30*time.Second)
	tc.publish(t, 5)
	var started handled
	release := make(chan struct{})
	tc.start(t, consumer.ConsumeOptions{
		Concurrency: 2,
		Handle: func(_ context.Context, m consumer.Message) {
			started.add(m)
			<-release
			_ = m.Ack()
		},
	})

	eventually(t, 5*time.Second, "two handlers", func() bool { return started.count() == 2 })
	time.Sleep(300 * time.Millisecond) // time for a wrongly prefetched message to arrive
	// The client holds only the two it is working on; the other three wait on the server.
	if info := tc.info(t); info.NumAckPending != 2 || info.NumPending != 3 || started.count() != 2 {
		t.Errorf("ack pending %d, pending %d, started %d",
			info.NumAckPending, info.NumPending, started.count())
	}

	close(release)
	eventually(t, 10*time.Second, "all five", func() bool { return started.count() == 5 })
	eventually(t, 5*time.Second, "all acked", func() bool { return tc.info(t).NumAckPending == 0 })
	for i, a := range started.attempts {
		if a != 1 {
			t.Errorf("message %s delivered %d times", started.bodies[i], a)
		}
	}
}

// Node's "holds only messages it is working on, so a long review never causes a redelivery": more
// messages than slots, each taking longer than ack_wait (signalling Working meanwhile). The ones
// waiting for a slot wait on the server, where no ack_wait runs, so none is redelivered.
func TestALongHandlerNeverCausesARedelivery(t *testing.T) {
	tc := newTestConsumer(t, time.Second)
	tc.publish(t, 4)
	var done handled
	tc.start(t, consumer.ConsumeOptions{
		Concurrency: 2,
		Handle: func(_ context.Context, m consumer.Message) {
			for range 3 { // 1.2 s in all, longer than ack_wait
				_ = m.Working()
				time.Sleep(400 * time.Millisecond)
			}
			done.add(m)
			_ = m.Ack()
		},
	})
	eventually(t, 15*time.Second, "all four", func() bool { return done.count() == 4 })
	time.Sleep(1500 * time.Millisecond) // past an ack_wait, for a redelivery to show
	// JetStream counts every delivery, redeliveries included, in the consumer's sequence.
	if info := tc.info(t); info.Delivered.Consumer != 4 || done.count() != 4 {
		t.Errorf("deliveries %d, handled %d (%v)", info.Delivered.Consumer, done.count(), done.attempts)
	}
}

func TestStopEndsAWaitingFetchAtOnce(t *testing.T) {
	tc := newTestConsumer(t, 30*time.Second)
	c := tc.start(t, consumer.ConsumeOptions{Concurrency: 2, Handle: func(context.Context, consumer.Message) {}})
	time.Sleep(200 * time.Millisecond) // the loop is now long-polling (5 s)
	start := time.Now()
	c.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Stop took %v", elapsed)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("the loop hadn't ended")
	}
	if c.Err() != nil {
		t.Errorf("Err = %v", c.Err())
	}
}

func TestStopWaitsForInFlightMessagesWithinTheGracePeriod(t *testing.T) {
	tc := newTestConsumer(t, 30*time.Second)
	tc.publish(t, 1)
	logs, logger := newCapture(t)
	var started atomic.Bool
	c := tc.start(t, consumer.ConsumeOptions{
		Concurrency:   2,
		Logger:        logger,
		ShutdownGrace: 3 * time.Second,
		Handle: func(_ context.Context, m consumer.Message) {
			started.Store(true)
			time.Sleep(300 * time.Millisecond)
			_ = m.Ack()
		},
	})
	eventually(t, 5*time.Second, "the handler", started.Load)
	c.Stop()
	if c.InFlight() != 0 {
		t.Errorf("in flight after Stop: %d", c.InFlight())
	}
	eventually(t, 5*time.Second, "the ack", func() bool { return tc.info(t).NumAckPending == 0 })
	if lines := logs.lines(t, "unfinished messages will be redelivered"); len(lines) != 0 {
		t.Errorf("lines = %v", lines)
	}
}

// After the grace period, a handler still running has its context cancelled (Node can't), returns,
// and its unsettled message is redelivered.
func TestAfterTheGracePeriodHandlersAreCancelledAndTheirMessagesRedelivered(t *testing.T) {
	tc := newTestConsumer(t, time.Second)
	tc.publish(t, 1)
	logs, logger := newCapture(t)
	var started, cancelled atomic.Bool
	c := tc.start(t, consumer.ConsumeOptions{
		Concurrency:   1,
		Logger:        logger,
		ShutdownGrace: 200 * time.Millisecond,
		Handle: func(ctx context.Context, _ consumer.Message) {
			started.Store(true)
			<-ctx.Done() // a handler whose every call takes the context returns once it ends
			cancelled.Store(true)
		},
	})
	eventually(t, 5*time.Second, "the handler", started.Load)
	start := time.Now()
	c.Stop()
	if elapsed := time.Since(start); elapsed > 2*time.Second || !cancelled.Load() {
		t.Errorf("Stop took %v, handler cancelled: %v", elapsed, cancelled.Load())
	}
	line := logs.one(t, "unfinished messages will be redelivered")
	if line["level"] != "warn" || line["in_flight"] != 1.0 {
		t.Errorf("line = %v", line)
	}

	// The message was never settled: delivered again, to the next consumer process.
	var again handled
	tc.start(t, consumer.ConsumeOptions{Concurrency: 1, Handle: func(_ context.Context, m consumer.Message) {
		again.add(m)
		_ = m.Ack()
	}})
	eventually(t, 10*time.Second, "the redelivery", func() bool { return again.count() == 1 })
	if again.attempts[0] != 2 {
		t.Errorf("attempt %d, want 2", again.attempts[0])
	}
}

func TestAMissingConsumerFailsAtStartAndEndsTheLoopAfterRepeatedAttempts(t *testing.T) {
	tc := newTestConsumer(t, 30*time.Second)
	_, err := consumer.StartConsuming(context.Background(), tc.js, consumer.ConsumeOptions{
		Stream: testStream, Consumer: "go-test-no-such-consumer", Concurrency: 1,
		Logger: slog.New(slog.DiscardHandler), Handle: func(context.Context, consumer.Message) {},
	})
	if err == nil {
		t.Fatal("started on a consumer that doesn't exist")
	}

	logs, logger := newCapture(t)
	c := tc.start(t, consumer.ConsumeOptions{
		Concurrency:        1,
		Logger:             logger,
		FetchExpires:       time.Second,
		FetchRetry:         backoff.Backoff{Base: 50 * time.Millisecond, Max: 100 * time.Millisecond},
		MaxMissingAttempts: 2,
		Handle:             func(context.Context, consumer.Message) {},
	})
	time.Sleep(200 * time.Millisecond)
	if err := tc.js.DeleteConsumer(context.Background(), testStream, string(tc.name)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the loop didn't end")
	}
	want := string(tc.name) + " consumer not found after 2 attempts"
	if c.Err() == nil || !strings.HasPrefix(c.Err().Error(), want) {
		t.Errorf("Err = %v", c.Err())
	}
	lines := logs.lines(t, string(tc.name)+" consumer or its stream not found")
	if len(lines) != 2 || lines[0]["attempt"] != 1.0 || lines[1]["attempt"] != 2.0 ||
		lines[0]["level"] != "error" {
		t.Errorf("lines = %v", lines)
	}
}

func TestAPanickingHandlerIsLoggedAndFreesItsSlot(t *testing.T) {
	tc := newTestConsumer(t, 30*time.Second)
	tc.publish(t, 2)
	logs, logger := newCapture(t)
	var calls atomic.Int32
	var acked atomic.Bool
	tc.start(t, consumer.ConsumeOptions{
		Concurrency: 1,
		Logger:      logger,
		Handle: func(_ context.Context, m consumer.Message) {
			if calls.Add(1) == 1 {
				panic("handler bug")
			}
			_ = m.Ack()
			acked.Store(true)
		},
	})
	eventually(t, 10*time.Second, "the second message", acked.Load)
	line := logs.one(t, "unhandled error while handling a message")
	if err, _ := line["err"].(map[string]any); err["message"] != "panic: handler bug" {
		t.Errorf("line = %v", line)
	}
}

// A zero Concurrency would make the semaphore unbuffered and the loop never fetch: refused.
func TestStartConsumingRefusesUnusableOptions(t *testing.T) {
	tc := newTestConsumer(t, 30*time.Second)
	_, err := consumer.StartConsuming(context.Background(), tc.js, consumer.ConsumeOptions{
		Stream: testStream, Consumer: tc.name, Logger: slog.New(slog.DiscardHandler),
		Handle: func(context.Context, consumer.Message) {},
	})
	if err == nil || !strings.Contains(err.Error(), "Concurrency (at least 1)") {
		t.Errorf("err = %v", err)
	}
}
