package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/contracttest"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// The seeded demo accounts (db/migrations/…_seed_demo_accounts.sql): their IBANs and the UUIDs a
// notification for them must be mapped to.
var (
	aliceIBAN = "FR7612345000010000000000193"
	bobIBAN   = "FR7612345000010000000000290"
	carolIBAN = "FR7612345000010000000000387"
	seedIDs   = map[string]string{
		aliceIBAN: "01a14c4e-e000-7f4f-896e-f8f54adff3bd",
		bobIBAN:   "01a14c4e-e001-7331-bab2-75411dbbe55d",
		carolIBAN: "01a14c4e-e002-71c0-a116-c28cf6a1c175",
	}
)

// foreignIBAN is a valid SEPA IBAN that is none of our accounts.
const foreignIBAN = "DE75512108001245126199"

const incomingPayments = "/provider/incoming-payments"

// newNetworkTransactionID is a fresh network ID, so tests never collide.
func newNetworkTransactionID() string {
	return "go-test-" + ids.New[ids.Event]().TypeID()
}

// notificationBody is a valid notification for alice, with overrides; an override of nil removes
// the field.
func notificationBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"network_transaction_id": newNetworkTransactionID(),
		"creditor_iban":          aliceIBAN,
		"amount_minor":           "1250",
		"currency":               "EUR",
		"network":                "SEPA",
	}
	for key, value := range overrides {
		if value == nil {
			delete(body, key)
		} else {
			body[key] = value
		}
	}
	return body
}

// newTestIBAN is a valid French IBAN no account has: random digits, with check digits computed by
// ISO 13616 (98 minus the number BBAN + "FR00", letters as numbers, mod 97).
func newTestIBAN() string {
	bban := fmt.Sprintf("%023d", rand.Int64N(1e18))   // 23 digits (leading zeros), FR's length less 4
	n, _ := new(big.Int).SetString(bban+"152700", 10) // F=15 R=27 00
	check := 98 - new(big.Int).Mod(n, big.NewInt(97)).Int64()
	return fmt.Sprintf("FR%02d%s", check, bban)
}

// postNotification POSTs body (a map, sent as JSON, or a string, sent as it is) to the webhook, with
// Content-Type application/json unless header says otherwise, and checks the response against the
// contract.
func (a testAPI) postNotification(t *testing.T, body any, header http.Header) response {
	t.Helper()
	var payload string
	switch b := body.(type) {
	case string:
		payload = b
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		payload = string(data)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, incomingPayments, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		req.Header[name] = values
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r := response{status: resp.StatusCode, header: resp.Header, body: string(data)}
	contracttest.Check(t, req, &http.Response{
		StatusCode: r.status, Header: r.header, Body: io.NopCloser(strings.NewReader(r.body)),
	})
	return r
}

// storedDeposit is a deposits row as the tests look at it.
type storedDeposit struct {
	ID                string
	AccountID         *string
	Status            string
	CreditorIBAN      string
	ProviderAccountID *string
	Amount            int64
	NotificationCount int
	CreatedAt         time.Time
}

// depositByNetworkID reads the deposit stored under a network transaction ID; false if none.
func depositByNetworkID(t *testing.T, pool *pgxpool.Pool, ntx any) (storedDeposit, bool) {
	t.Helper()
	var d storedDeposit
	err := pool.QueryRow(context.Background(), `
SELECT id::text, account_id::text, status, creditor_iban, provider_account_id, amount_minor,
       notification_count, created_at
FROM deposits WHERE network = 'SEPA' AND network_transaction_id = $1`, ntx).
		Scan(&d.ID, &d.AccountID, &d.Status, &d.CreditorIBAN, &d.ProviderAccountID, &d.Amount,
			&d.NotificationCount, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedDeposit{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return d, true
}

func mustDeposit(t *testing.T, pool *pgxpool.Pool, ntx any) storedDeposit {
	t.Helper()
	d, ok := depositByNetworkID(t, pool, ntx)
	if !ok {
		t.Fatalf("no deposit stored for %v", ntx)
	}
	return d
}

func countOf(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func historyCount(t *testing.T, pool *pgxpool.Pool, depositID string) int {
	return countOf(t, pool, `SELECT count(*)::int FROM deposit_status_history WHERE deposit_id = $1`, depositID)
}

func outboxCount(t *testing.T, pool *pgxpool.Pool, depositID string) int {
	return countOf(t, pool, `SELECT count(*)::int FROM outbox WHERE entity_id = $1`, depositID)
}

// outboxRow is an outbox row as the tests look at it.
type outboxRow struct {
	ID, EntityType, EntityID, Subject, MsgID string
	Attempts                                 int
	PublishedAt                              *time.Time
	Payload                                  []byte
}

func outboxRowsFor(t *testing.T, pool *pgxpool.Pool, depositID string) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT id::text, entity_type, entity_id::text, subject, msg_id, attempts, published_at, payload
FROM outbox WHERE entity_id = $1 ORDER BY id`, depositID)
	if err != nil {
		t.Fatal(err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outboxRow, error) {
		var r outboxRow
		err := row.Scan(&r.ID, &r.EntityType, &r.EntityID, &r.Subject, &r.MsgID, &r.Attempts, &r.PublishedAt, &r.Payload)
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// conflict is a deposit_notification_conflicts row.
type conflict struct {
	DifferingFields []string
	Amount          int64
	CreditorIBAN    string
}

func conflictsOf(t *testing.T, pool *pgxpool.Pool, depositID string) []conflict {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT differing_fields, amount_minor, creditor_iban FROM deposit_notification_conflicts
WHERE deposit_id = $1 ORDER BY id`, depositID)
	if err != nil {
		t.Fatal(err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (conflict, error) {
		var c conflict
		err := row.Scan(&c.DifferingFields, &c.Amount, &c.CreditorIBAN)
		return c, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// logLine finds the last log line with this network ID and message (the log is in order, so the
// most recent).
func (a testAPI) logLine(t *testing.T, ntx any, msg string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range a.logLines(t) {
		if line["network_transaction_id"] == ntx && line["msg"] == msg {
			found = line
		}
	}
	return found
}

// postFramed POSTs a raw body with exactly the given headers (no default Content-Type: a header
// given as nil is left out), for the framing cases, and checks the response against the contract.
func (a testAPI) postFramed(t *testing.T, body string, header http.Header) response {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, incomingPayments, strings.NewReader(body))
	for name, values := range header {
		if values != nil {
			req.Header[name] = values
		}
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r := response{status: resp.StatusCode, header: resp.Header, body: string(data)}
	contracttest.Check(t, req, &http.Response{
		StatusCode: r.status, Header: r.header, Body: io.NopCloser(strings.NewReader(r.body)),
	})
	return r
}

// idFromUUID turns a UUID read from the database into its ID type (its Scan, as pgx would).
func idFromUUID[K ids.Kind](t *testing.T, uuid string) ids.ID[K] {
	t.Helper()
	var id ids.ID[K]
	if err := id.Scan(uuid); err != nil {
		t.Fatal(err)
	}
	return id
}
