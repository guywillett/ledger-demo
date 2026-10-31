package consumer_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/backoff"
	"github.com/guywillett/ledger-demo/go/internal/consumer"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The delivery lifecycle with fake messages against the test database: every settle path and its
// order, settling twice, errors after settling, and the lifecycle's own paths (invalid messages, an
// unrecorded delivery, bugs and transient failures). Node tests these through the deposit-worker's
// handler (test/deposit-worker/handler.test.ts) and test/consumer/delivery-log.test.ts; here the
// shared layer is tested on its own, with a test event.

var backoffForTests = backoff.Backoff{Base: time.Second, Max: 8 * time.Second}

// Short names for the handler signatures below (type aliases: the same types).
type (
	dlv     = consumer.Delivery
	settled = consumer.Settled
)

func ok(s consumer.Settled, err error) consumer.Settled {
	if err != nil {
		panic(err)
	}
	return s
}

func TestDoneRecordsTheOutcomeThenAcks(t *testing.T) {
	pool := testdb.Pool(t)
	body, eventID := eventBody(1)
	m := message(body)
	m.attempt = 2
	var atAck string
	m.onSignal = func(string) { atAck = outcomeNow(t, pool, m.msgID) }

	outcome := lifecycle(pool, discard()).Deliver(t.Context(), m,
		func(ctx context.Context, e testEvent, d *dlv) (settled, error) {
			if e.N != 1 || e.EventID != eventID {
				t.Errorf("event = %+v", e)
			}
			return d.Done(ctx, consumer.Applied)
		})

	if outcome != consumer.Applied || !slices.Equal(m.signals(), []string{"ack"}) {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	// Recorded before the ack went out.
	if atAck != "applied" {
		t.Errorf("outcome when acking = %q, want applied", atAck)
	}
	rows := deliveriesByMsgID(t, pool, m.msgID)
	if len(rows) != 1 || rows[0].Outcome != "applied" || rows[0].EventID == nil ||
		*rows[0].EventID != eventID.UUID().String() || rows[0].DeliveryAttempt != 2 ||
		rows[0].Consumer != "deposit-worker" || !rows[0].Completed {
		t.Errorf("rows = %+v", rows)
	}
}

func TestStopAndRetryLaterSignalFirstThenRecord(t *testing.T) {
	pool := testdb.Pool(t)
	tests := []struct {
		name    string
		handle  consumer.Handler[testEvent]
		signal  string
		outcome consumer.Outcome
	}{
		{"stop", func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.Stop(ctx, consumer.Invalid, "no such deposit")
		}, "term", consumer.Invalid},
		{"retry later", func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.RetryLater(ctx)
		}, "nak", consumer.Failed},
	}
	for _, tt := range tests {
		body, _ := eventBody(1)
		m := message(body)
		var atSignal string
		m.onSignal = func(string) { atSignal = outcomeNow(t, pool, m.msgID) }
		outcome := lifecycle(pool, discard()).Deliver(t.Context(), m, tt.handle)
		if outcome != tt.outcome || !slices.Equal(m.signals(), []string{tt.signal}) {
			t.Errorf("%s: outcome %s, signals %v", tt.name, outcome, m.signals())
		}
		if atSignal != "in_progress" {
			t.Errorf("%s: outcome when signalling = %q, want in_progress", tt.name, atSignal)
		}
		if got := outcomeNow(t, pool, m.msgID); got != string(tt.outcome) {
			t.Errorf("%s: recorded %q", tt.name, got)
		}
	}
}

func TestRetryLaterUsesTheBackoffForTheAttemptAndRetryAfterItsOwnDelay(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	m.attempt = 3
	lifecycle(pool, discard()).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.RetryLater(ctx)
		})
	if m.nakDelay != 4*time.Second { // 1 s doubling: attempt 3
		t.Errorf("RetryLater delay = %v", m.nakDelay)
	}

	m = message(body)
	lifecycle(pool, discard()).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.RetryAfter(ctx, 1500*time.Millisecond)
		})
	if m.nakDelay != 1500*time.Millisecond || outcomeNow(t, pool, m.msgID) != "failed" {
		t.Errorf("RetryAfter delay = %v, outcome %s", m.nakDelay, outcomeNow(t, pool, m.msgID))
	}
}

// Node's delivery-log.test.ts: a second settle is refused, nothing is signalled twice, and the
// error after settling is logged.
func TestSettlesOnceASecondSettleIsRefusedAndNothingIsSignalledTwice(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	logs, logger := newCapture(t)
	var second error
	outcome := lifecycle(pool, logger).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			ok(d.Done(ctx, consumer.Applied))
			var s consumer.Settled
			s, second = d.Stop(ctx, consumer.Failed, "too late")
			return s, second
		})
	if outcome != consumer.Applied || !slices.Equal(m.signals(), []string{"ack"}) {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	if second == nil || second.Error() != "message already settled (applied); can't settle it as failed" {
		t.Errorf("second settle: %v", second)
	}
	line := logs.one(t, "error after the message was settled; not settling it again")
	if line["level"] != "error" {
		t.Errorf("line = %v", line)
	}
	// Every pair of settles is refused the same way.
	for _, second := range []func(context.Context, *consumer.Delivery) (consumer.Settled, error){
		func(ctx context.Context, d *dlv) (settled, error) {
			return d.Done(ctx, consumer.Duplicate)
		},
		func(ctx context.Context, d *dlv) (settled, error) { return d.RetryLater(ctx) },
	} {
		m := message(body)
		lifecycle(pool, discard()).Deliver(t.Context(), m,
			func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
				ok(d.RetryAfter(ctx, time.Second))
				return second(ctx, d)
			})
		if !slices.Equal(m.signals(), []string{"nak"}) || outcomeNow(t, pool, m.msgID) != "failed" {
			t.Errorf("signals %v, outcome %s", m.signals(), outcomeNow(t, pool, m.msgID))
		}
	}
}

func TestAnErrorAfterSettlingIsLoggedNotSettledAgain(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	logs, logger := newCapture(t)
	outcome := lifecycle(pool, logger).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			s := ok(d.Stop(ctx, consumer.Invalid, "bad"))
			return s, errors.New("follow-up work failed")
		})
	if outcome != consumer.Invalid || !slices.Equal(m.signals(), []string{"term"}) {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	logs.one(t, "error after the message was settled; not settling it again")
}

// A signal that can't be sent (the connection closed) settles the message all the same: never
// signalled again.
func TestASignalThatCantBeSentStillSettles(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	m.failSend = errors.New("nats: connection closed")
	logs, logger := newCapture(t)
	outcome := lifecycle(pool, logger).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.Done(ctx, consumer.Applied)
		})
	if outcome != consumer.Applied || !slices.Equal(m.signals(), []string{"ack"}) ||
		outcomeNow(t, pool, m.msgID) != "applied" {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	logs.one(t, "error after the message was settled; not settling it again")
}

func TestTerminatesAMessageThatIsNotJSONLoggingTheDeliveryWithoutAnEventID(t *testing.T) {
	pool := testdb.Pool(t)
	m := message("{not json")
	logs, logger := newCapture(t)
	outcome := lifecycle(pool, logger).Deliver(t.Context(), m, unreachable(t))
	if outcome != consumer.Invalid || !slices.Equal(m.signals(), []string{"term"}) ||
		m.reason != "invalid test.event event" {
		t.Errorf("outcome %s, signals %v, reason %q", outcome, m.signals(), m.reason)
	}
	rows := deliveriesByMsgID(t, pool, m.msgID)
	if len(rows) != 1 || rows[0].Outcome != "invalid" || rows[0].EventID != nil {
		t.Errorf("rows = %+v", rows)
	}
	line := logs.one(t, "invalid test.event event; terminating it")
	if _, has := line["event_id"]; has || line["level"] != "error" {
		t.Errorf("line = %v", line)
	}
}

func TestTerminatesAnInvalidEventKeepingItsEventIDInTheLog(t *testing.T) {
	pool := testdb.Pool(t)
	_, eventID := eventBody(1)
	m := message(`{"event_id":"` + eventID.TypeID() + `","n":"one"}`)
	logs, logger := newCapture(t)
	outcome := lifecycle(pool, logger).Deliver(t.Context(), m, unreachable(t))
	if outcome != consumer.Invalid || !slices.Equal(m.signals(), []string{"term"}) {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	rows := deliveriesByMsgID(t, pool, m.msgID)
	if len(rows) != 1 || rows[0].EventID == nil || *rows[0].EventID != eventID.UUID().String() {
		t.Errorf("rows = %+v", rows)
	}
	if line := logs.one(t, "invalid test.event event; terminating it"); line["event_id"] != eventID.TypeID() {
		t.Errorf("line = %v", line)
	}
}

// Every lifecycle line carries the delivery's fields, and whatever Bind added.
func TestLogLinesCarryTheDeliveryFieldsAndBind(t *testing.T) {
	pool := testdb.Pool(t)
	body, eventID := eventBody(1)
	m := message(body)
	m.msgID = ""
	m.attempt = 4
	logs, logger := newCapture(t)
	lifecycle(pool, logger).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			d.Bind("deposit_id", "dep_test")
			d.Log().Info("handler line")
			return consumer.Settled{}, errors.New("database went away")
		})
	for _, msg := range []string{"handler line", "processing failed; will retry"} {
		line := logs.one(t, msg)
		if v, has := line["nats_msg_id"]; !has || v != nil {
			t.Errorf("%s: nats_msg_id = %v (want null)", msg, v)
		}
		if line["delivery_attempt"] != 4.0 || line["event_id"] != eventID.TypeID() ||
			line["deposit_id"] != "dep_test" {
			t.Errorf("%s: line = %v", msg, line)
		}
	}
}

func TestRetriesATransientFailure(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	logs, logger := newCapture(t)
	l := lifecycle(pool, logger)
	l.TransientFailureMessage = "submission failed; will retry"
	outcome := l.Deliver(t.Context(), m,
		func(context.Context, testEvent, *dlv) (settled, error) {
			return consumer.Settled{}, errors.New("database went away")
		})
	if outcome != consumer.Failed || !slices.Equal(m.signals(), []string{"nak"}) ||
		m.nakDelay != time.Second || outcomeNow(t, pool, m.msgID) != "failed" {
		t.Errorf("outcome %s, signals %v, delay %v", outcome, m.signals(), m.nakDelay)
	}
	if line := logs.one(t, "submission failed; will retry"); line["level"] != "error" {
		t.Errorf("line = %v", line)
	}
}

// A bug terminates rather than retrying, with a fatal alert line; so does a handler that misuses
// its Delivery (the zero Settled, or a wrong outcome), whatever IsBug says.
func TestTerminatesABugWithAFatalAlert(t *testing.T) {
	pool := testdb.Pool(t)
	for name, handle := range map[string]consumer.Handler[testEvent]{
		"an IsBug error": func(context.Context, testEvent, *dlv) (settled, error) {
			return consumer.Settled{}, errBug
		},
		"the zero Settled": func(context.Context, testEvent, *dlv) (settled, error) {
			return consumer.Settled{}, nil
		},
		"done with a stop's outcome": func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.Done(ctx, consumer.Invalid)
		},
	} {
		body, _ := eventBody(1)
		m := message(body)
		logs, logger := newCapture(t)
		outcome := lifecycle(pool, logger).Deliver(t.Context(), m, handle)
		if outcome != consumer.Failed || !slices.Equal(m.signals(), []string{"term"}) ||
			m.reason != "invariant violation" || outcomeNow(t, pool, m.msgID) != "failed" {
			t.Errorf("%s: outcome %s, signals %v, reason %q", name, outcome, m.signals(), m.reason)
		}
		line := logs.one(t, "invariant violated; terminating the message")
		if line["level"] != "fatal" || line["alert"] != true {
			t.Errorf("%s: line = %v", name, line)
		}
	}
}

// Node's "naks without throwing when the delivery cannot even be recorded".
func TestNaksWhenTheDeliveryCantEvenBeRecorded(t *testing.T) {
	unreachable := unreachablePool(t)
	body, _ := eventBody(1)
	m := message(body)
	m.attempt = 2
	logs, logger := newCapture(t)
	outcome := lifecycle(unreachable, logger).Deliver(t.Context(), m,
		func(context.Context, testEvent, *dlv) (settled, error) {
			t.Error("the handler ran")
			return consumer.Settled{}, nil
		})
	if outcome != consumer.Failed || !slices.Equal(m.signals(), []string{"nak"}) || m.nakDelay != 2*time.Second {
		t.Errorf("outcome %s, signals %v, delay %v", outcome, m.signals(), m.nakDelay)
	}
	logs.one(t, "could not record delivery; will retry")
}

// Node's signal vs record case: a stop holds even when its outcome can't be recorded.
func TestTerminatesAnInvalidMessageEvenWhenItsOutcomeCantBeRecorded(t *testing.T) {
	pool := testdb.Pool(t)
	m := message("not json")
	logs, logger := newCapture(t)
	outcome := lifecycle(failingFinish{pool}, logger).Deliver(t.Context(), m, unreachable(t))
	if outcome != consumer.Invalid || !slices.Equal(m.signals(), []string{"term"}) {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	if line := logs.one(t, "could not record the delivery outcome"); line["outcome"] != "invalid" {
		t.Errorf("line = %v", line)
	}
	// The trace a missed write leaves: the row still in progress.
	if got := outcomeNow(t, pool, m.msgID); got != "in_progress" {
		t.Errorf("row outcome %q", got)
	}
}

// Done records first: if it can't, nothing is acked, and the message is retried (its redelivery
// finds the work done).
func TestDoneThatCantRecordRetriesInsteadOfAcking(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	logs, logger := newCapture(t)
	outcome := lifecycle(failingFinish{pool}, logger).Deliver(t.Context(), m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			return d.Done(ctx, consumer.Applied)
		})
	if outcome != consumer.Failed || !slices.Equal(m.signals(), []string{"nak"}) {
		t.Errorf("outcome %s, signals %v", outcome, m.signals())
	}
	logs.one(t, "processing failed; will retry")
}

// The delivery log on its own: a delivery finishes once.
func TestADeliveryFinishesOnce(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	id, err := consumer.StartDelivery(ctx, pool, consumer.NewDelivery{
		Consumer: consumer.Submitter, NATSMsgID: "once-" + time.Now().String(), DeliveryAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.FinishDelivery(ctx, pool, id, consumer.Invalid); err != nil {
		t.Fatal(err)
	}
	err = consumer.FinishDelivery(ctx, pool, id, consumer.Applied)
	want := "delivery " + strconv.FormatInt(int64(id), 10) +
		" is not in progress; outcome applied not recorded"
	if err == nil || err.Error() != want {
		t.Errorf("second finish: %v", err)
	}
}

func unreachable(t *testing.T) consumer.Handler[testEvent] {
	return func(context.Context, testEvent, *dlv) (settled, error) {
		t.Error("the handler ran for an invalid message")
		return consumer.Settled{}, nil
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// unreachablePool is a pool on a port nothing listens on.
func unreachablePool(t *testing.T) db.Querier {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://ledger:ledger@localhost:1/ledger_go_test?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A panic in the handler goes through the lifecycle like an error the handler returned (Node: what
// a handler throws, caught by withDeliveryLog): retried unless IsBug says it's a bug.
func TestAPanicInTheHandlerIsClassifiedLikeAnError(t *testing.T) {
	pool := testdb.Pool(t)
	panicking := func(context.Context, testEvent, *dlv) (settled, error) {
		panic("deposit is nil") // as a nil dereference would
	}

	body, eventID := eventBody(1)
	m := message(body)
	logs, logger := newCapture(t)
	outcome := lifecycle(pool, logger).Deliver(t.Context(), m, panicking)
	if outcome != consumer.Failed || !slices.Equal(m.signals(), []string{"nak"}) ||
		outcomeNow(t, pool, m.msgID) != "failed" {
		t.Errorf("transient: outcome %s, signals %v", outcome, m.signals())
	}
	line := logs.one(t, "processing failed; will retry")
	err, _ := line["err"].(map[string]any)
	if line["event_id"] != eventID.TypeID() || err["type"] != "*consumer.PanicError" ||
		err["message"] != "panic: deposit is nil" {
		t.Errorf("line = %v", line)
	}

	// A consumer whose IsBug counts panics as bugs terminates instead.
	m = message(body)
	logs, logger = newCapture(t)
	l := lifecycle(pool, logger)
	l.IsBug = func(err error) bool {
		var p *consumer.PanicError
		return errors.As(err, &p) && strings.Contains(p.Stack, "lifecycle_test.go")
	}
	if outcome := l.Deliver(t.Context(), m, panicking); outcome != consumer.Failed ||
		!slices.Equal(m.signals(), []string{"term"}) {
		t.Errorf("bug: outcome %s, signals %v", outcome, m.signals())
	}
	logs.one(t, "invariant violated; terminating the message")
}

// Work committed just as shutdown cancels the handler is still recorded and acked: Done's write
// isn't cancelled with the handler's context.
func TestDoneAfterTheHandlersContextIsCancelledStillRecordsAndAcks(t *testing.T) {
	pool := testdb.Pool(t)
	body, _ := eventBody(1)
	m := message(body)
	ctx, cancel := context.WithCancel(t.Context())
	outcome := lifecycle(pool, discard()).Deliver(ctx, m,
		func(ctx context.Context, _ testEvent, d *dlv) (settled, error) {
			cancel() // the grace period ends after the work committed
			return d.Done(ctx, consumer.Applied)
		})
	if outcome != consumer.Applied || !slices.Equal(m.signals(), []string{"ack"}) ||
		outcomeNow(t, pool, m.msgID) != "applied" {
		t.Errorf("outcome %s, signals %v, recorded %s", outcome, m.signals(), outcomeNow(t, pool, m.msgID))
	}
}
