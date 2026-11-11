package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ingestion"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/telemetry"
	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// ledger_transitions_total (slice 5 decision 88), through telemetry.Setup's real views: the cases of
// node/test/telemetry/transitions.telemetry.test.ts.

var (
	readerOnce sync.Once
	reader     *sdkmetric.ManualReader
)

// meterReader sets telemetry up once for the package, with an in-memory metric reader, and returns
// it: once, because the global meter provider delegates an instrument made before it to the first
// provider registered only.
func meterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	readerOnce.Do(func() {
		reader = sdkmetric.NewManualReader()
		if _, err := telemetry.Setup(context.Background(), telemetry.Config{
			Service:      "deposit-worker",
			Logs:         "otlp",
			SpanExporter: tracetest.NewInMemoryExporter(),
			MetricReader: reader,
			LogExporter:  &telemetrytest.LogExporter{},
		}); err != nil {
			t.Fatal(err)
		}
	})
	return reader
}

type step struct{ entity, from, to string }

// count is the counter's value for one step now; 0 before its first count. Fatal if the counter has
// the wrong unit or attributes.
func count(t *testing.T, s step) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := meterReader(t).Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "ledger_transitions_total" {
				continue
			}
			if m.Unit != "{transition}" {
				t.Fatalf("unit %q, want {transition}", m.Unit)
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("data %T, want an int64 counter", m.Data)
			}
			for _, point := range sum.DataPoints {
				get := func(key attribute.Key) string { v, _ := point.Attributes.Value(key); return v.AsString() }
				if get("entity") == s.entity && get("from") == s.from && get("to") == s.to {
					if impl := get("implementation"); impl != "go" || point.Attributes.Len() != 4 {
						t.Fatalf("attributes %v, want entity, from, to and implementation go", point.Attributes)
					}
					return point.Value
				}
			}
		}
	}
	return 0
}

// counted is how much the counter for s grows while fn runs.
func counted(t *testing.T, s step, fn func()) int64 {
	t.Helper()
	before := count(t, s)
	fn()
	return count(t, s) - before
}

var (
	deposited = step{"deposit", "none", "created"}
	booked    = step{"deposit", "created", "received"}
)

func book(t *testing.T, deposit ids.DepositID) ledger.Result {
	t.Helper()
	result, err := db.InTxValue(context.Background(), testdb.Pool(t), func(tx db.Tx) (ledger.Result, error) {
		return ledger.Book(context.Background(), tx, deposit)
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func notify(t *testing.T, account testdb.Account, ntx string) ingestion.Result {
	t.Helper()
	result, err := db.InTxValue(context.Background(), testdb.Pool(t), func(tx db.Tx) (ingestion.Result, error) {
		return ingestion.RecordNotification(context.Background(), tx, ingestion.Notification{
			CreditorIBAN:         sepa.IBAN(account.IBAN),
			Amount:               1250,
			Currency:             "EUR",
			Network:              "SEPA",
			NetworkTransactionID: ntx,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestACreationCountsFromNoneOnceAndADuplicateNotificationNothing(t *testing.T) {
	meterReader(t)
	account := testdb.NewAccount(t, testdb.Pool(t))
	ntx := "go-metric-test-" + ids.New[ids.Event]().TypeID()
	if n := counted(t, deposited, func() { notify(t, account, ntx) }); n != 1 {
		t.Errorf("a new deposit counted %d, want 1", n)
	}
	if n := counted(t, deposited, func() {
		if r := notify(t, account, ntx); r.Kind != ingestion.Duplicate {
			t.Errorf("kind %v, want a duplicate", r.Kind)
		}
	}); n != 0 {
		t.Errorf("a duplicate notification counted %d, want 0", n)
	}
}

func TestAnAppliedTransitionCountsOnce(t *testing.T) {
	meterReader(t)
	pool := testdb.Pool(t)
	deposit := testdb.NewDeposit(t, pool, testdb.NewAccount(t, pool), 1250)
	if n := counted(t, booked, func() { book(t, deposit) }); n != 1 {
		t.Errorf("counted %d, want 1", n)
	}
}

// A duplicate delivery: the transition already happened, so it isn't applied, and isn't counted.
func TestASkippedTransitionCountsNothing(t *testing.T) {
	meterReader(t)
	pool := testdb.Pool(t)
	deposit := testdb.NewDeposit(t, pool, testdb.NewAccount(t, pool), 1250)
	book(t, deposit)
	if n := counted(t, booked, func() {
		if r := book(t, deposit); r != ledger.Skipped {
			t.Errorf("result %v, want skipped", r)
		}
	}); n != 0 {
		t.Errorf("counted %d, want 0", n)
	}
}

func TestATransactionRolledBackAfterATransitionCountsNothing(t *testing.T) {
	meterReader(t)
	pool := testdb.Pool(t)
	deposit := testdb.NewDeposit(t, pool, testdb.NewAccount(t, pool), 1250)
	rolledBack := errors.New("rolled back")
	if n := counted(t, booked, func() {
		err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
			if r, err := ledger.Book(context.Background(), tx, deposit); err != nil || r != ledger.Applied {
				t.Errorf("book = %v, %v", r, err)
			}
			return rolledBack
		})
		if !errors.Is(err, rolledBack) {
			t.Errorf("err %v", err)
		}
	}); n != 0 {
		t.Errorf("counted %d, want 0", n)
	}
}

func TestAWithdrawalCountsItsCreationAndEachStepFromTheStatusItLeft(t *testing.T) {
	meterReader(t)
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	var withdrawal ids.WithdrawalID
	if n := counted(t, step{"withdrawal", "none", "held"}, func() {
		withdrawal = newWithdrawal(t, pool, account, 400)
	}); n != 1 {
		t.Errorf("creation counted %d, want 1", n)
	}
	if n := counted(t, step{"withdrawal", "held", "submitted"}, func() {
		_, err := db.InTxValue(context.Background(), pool, func(tx db.Tx) (ledger.Result, error) {
			return ledger.MarkSubmitted(context.Background(), tx, withdrawal, "tr_1")
		})
		if err != nil {
			t.Error(err)
		}
	}); n != 1 {
		t.Errorf("held -> submitted counted %d, want 1", n)
	}
}
