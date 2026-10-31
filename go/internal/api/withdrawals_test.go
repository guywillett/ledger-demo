package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/contracttest"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// POST /accounts/{account_id}/withdrawals and GET /withdrawals/{withdrawal_id} against the test
// database: the cases of node/test/api/withdrawals.test.ts, and the withdrawal row of
// body-errors.test.ts. Every response is checked against contracts/openapi.yaml (postWithdrawal,
// get), and the outbox payload against its event contract.

const creditorIBAN = "DE75512108001245126199"

// fundedAccount is a new account with amount available, put there the real way: a deposit booked,
// reviewed, approved and credited (Node's fundTestAccount).
func fundedAccount(t *testing.T, pool *pgxpool.Pool, amount money.Amount) testdb.Account {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	fund(t, pool, account, amount)
	return account
}

func fund(t *testing.T, pool *pgxpool.Pool, account testdb.Account, amount money.Amount) {
	t.Helper()
	process(t, pool, testdb.NewDeposit(t, pool, account, amount), deposits.Approve, "")
}

// withdrawalBody is a valid body with overrides; an override of nil removes the field (use
// json.RawMessage("null") for a JSON null).
func withdrawalBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"amount_minor":    "400",
		"currency":        "EUR",
		"creditor_iban":   creditorIBAN,
		"creditor_name":   "Jane Doe",
		"remittance_info": "Rent October",
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	return body
}

// postWithdrawal POSTs a body (a map, as JSON) with the key ("" for no Idempotency-Key header),
// and checks the response against the contract, as Node's postWithdrawal.
// withdraw is postWithdrawal for a test account and withdrawalBody(overrides).
func (a testAPI) withdraw(
	t *testing.T,
	account testdb.Account,
	overrides map[string]any,
	key string,
) response {
	t.Helper()
	return a.postWithdrawal(t, account.ID.TypeID(), withdrawalBody(overrides), key)
}

func (a testAPI) postWithdrawal(
	t *testing.T,
	account string,
	body map[string]any,
	key string,
) response {
	t.Helper()
	header := http.Header{}
	if key != "" {
		header.Set("Idempotency-Key", key)
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	target := "/accounts/" + account + "/withdrawals"
	return a.postRaw(t, target, "application/json", string(data), header)
}

// postRaw POSTs a raw body with a Content-Type and headers, checking the response against the
// contract.
func (a testAPI) postRaw(
	t *testing.T,
	target, contentType, body string,
	header http.Header,
) response {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target,
		strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
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

func newKey() string { return ids.New[ids.Event]().TypeID() } // any unique string the pattern takes

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func withdrawalsOf(t *testing.T, pool *pgxpool.Pool, account testdb.Account) int {
	return count(t, pool, `SELECT count(*)::int FROM withdrawals WHERE account_id = $1`, account.ID)
}

func holdsOf(t *testing.T, pool *pgxpool.Pool, account testdb.Account) int {
	return count(t, pool, `
SELECT count(*)::int FROM balance_movements
WHERE account_id = $1 AND reference_type = 'withdrawal' AND reference_step = 'hold'`, account.ID)
}

// heldEventsOf counts withdrawal.held events about the account (the payload carries its TypeID).
func heldEventsOf(t *testing.T, pool *pgxpool.Pool, accountTypeID string) int {
	return count(t, pool, `
SELECT count(*)::int FROM outbox WHERE subject = 'withdrawal.held' AND payload->>'account_id' = $1`,
		accountTypeID)
}

type balanceRow struct{ pendingIn, available, held int64 }

func balancesOf(t *testing.T, pool *pgxpool.Pool, account testdb.Account) balanceRow {
	t.Helper()
	var b balanceRow
	err := pool.QueryRow(t.Context(), `
SELECT pending_in, available, held FROM account_balances WHERE account_id = $1`, account.ID).
		Scan(&b.pendingIn, &b.available, &b.held)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCreatesTheWithdrawalAndHoldsItsFunds201WithALocationHeader(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	key := newKey()
	r := a.withdraw(t, account, nil, key)
	if r.status != 201 {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	got := r.json(t)
	want := map[string]any{
		"account_id": account.ID.TypeID(), "idempotency_key": key, "amount_minor": "400",
		"currency": "EUR", "creditor_iban": creditorIBAN, "creditor_name": "Jane Doe",
		"remittance_info": "Rent October", "status": "held", "failure_reason": nil,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	id, _ := got["id"].(string)
	if !strings.HasPrefix(id, "wd_") {
		t.Errorf("id = %v", got["id"])
	}
	// Provider identifiers stay at the edge.
	if _, has := got["provider_transfer_id"]; has {
		t.Error("provider_transfer_id in the response")
	}
	if loc := r.header.Get("Location"); loc != "/withdrawals/"+id {
		t.Errorf("Location = %q", loc)
	}
	if b := balancesOf(t, a.pool, account); b != (balanceRow{0, 600, 400}) {
		t.Errorf("balances %+v", b)
	}
	if n := holdsOf(t, a.pool, account); n != 1 {
		t.Errorf("holds %d", n)
	}
	balance := a.get(t, "/accounts/"+account.ID.TypeID()+"/balance")
	if balance.body != `{"pending_in":"0","available":"600","held":"400","currency":"EUR"}` {
		t.Errorf("balance %s", balance.body)
	}
}

func TestTrimsTheTextFieldsAndTreatsAnEmptyOrBlankRemittanceAsNone(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	trimmed := a.postWithdrawal(t, account.ID.TypeID(),
		withdrawalBody(map[string]any{
			"creditor_name": "  Jane Doe ", "remittance_info": " Rent  October ",
		}),
		newKey())
	// Only the ends: the double space inside is kept.
	if got := trimmed.json(t); trimmed.status != 201 || got["creditor_name"] != "Jane Doe" ||
		got["remittance_info"] != "Rent  October" {
		t.Errorf("trimmed: %d %s", trimmed.status, trimmed.body)
	}
	for _, remittance := range []any{nil, json.RawMessage("null"), "", "   "} {
		body := withdrawalBody(map[string]any{"amount_minor": "1", "remittance_info": remittance})
		r := a.postWithdrawal(t, account.ID.TypeID(), body, newKey())
		if r.status != 201 || r.json(t)["remittance_info"] != nil {
			t.Errorf("remittance %#v: %d %s", remittance, r.status, r.body)
		}
	}
}

func TestAnswersARepeatedRequestWithTheSameWithdrawalOne201Then200sOneHold(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	key := newKey()
	var codes []int
	ids := map[any]bool{}
	for range 5 {
		r := a.withdraw(t, account, nil, key)
		codes = append(codes, r.status)
		ids[r.json(t)["id"]] = true
	}
	if !slices.Equal(codes, []int{201, 200, 200, 200, 200}) || len(ids) != 1 {
		t.Errorf("codes %v, ids %v", codes, ids)
	}
	if withdrawalsOf(t, a.pool, account) != 1 || holdsOf(t, a.pool, account) != 1 {
		t.Error("more than one withdrawal or hold")
	}
	if b := balancesOf(t, a.pool, account); b.available != 600 || b.held != 400 {
		t.Errorf("balances %+v", b)
	}
}

func TestTreatsARepeatThatDiffersOnlyInTrimmedWhitespaceOrBlankRemittanceAsTheSame(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	key := newKey()
	first := a.withdraw(t, account, map[string]any{"remittance_info": nil}, key)
	again := a.withdraw(t, account,
		map[string]any{"creditor_name": " Jane Doe ", "remittance_info": ""}, key)
	if first.status != 201 || again.status != 200 {
		t.Errorf("%d %d", first.status, again.status)
	}
}

// Ten requests with one key at once: goroutines through the handler, released together by closing
// a channel (the starting gun), under -race.
func TestCreatesOneWithdrawalFor10ConcurrentRequestsWithOneKey(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	key := newKey()
	start := make(chan struct{})
	responses := make([]response, 10)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Go(func() {
			<-start
			responses[i] = a.withdraw(t, account, nil, key)
		})
	}
	close(start)
	wg.Wait()

	var codes []int
	ids := map[any]bool{}
	for _, r := range responses {
		codes = append(codes, r.status)
		ids[r.json(t)["id"]] = true
	}
	slices.Sort(codes)
	want := []int{200, 200, 200, 200, 200, 200, 200, 200, 200, 201}
	if !slices.Equal(codes, want) || len(ids) != 1 {
		t.Errorf("codes %v, ids %v", codes, ids)
	}
	if withdrawalsOf(t, a.pool, account) != 1 || holdsOf(t, a.pool, account) != 1 ||
		heldEventsOf(t, a.pool, account.ID.TypeID()) != 1 {
		t.Error("not exactly one withdrawal, hold and event")
	}
}

func TestRefusesTheSameKeyWithADifferentDetail409NothingChanged(t *testing.T) {
	a := newTestAPI(t)
	for field, overrides := range map[string]map[string]any{
		"amount_minor":    {"amount_minor": "401"},
		"creditor_iban":   {"creditor_iban": "FR7612345000010000000000193"},
		"creditor_name":   {"creditor_name": "John Doe"},
		"remittance_info": {"remittance_info": "Rent November"},
	} {
		account := fundedAccount(t, a.pool, 1000)
		key := newKey()
		first := a.withdraw(t, account, nil, key)
		conflict := a.withdraw(t, account, overrides, key)
		got := conflict.json(t)
		if conflict.status != 409 ||
			got["type"] != "urn:ledger-demo:problem:idempotency-key-conflict" ||
			!reflect.DeepEqual(got["differing_fields"], []any{field}) {
			t.Errorf("%s: %d %s", field, conflict.status, conflict.body)
		}
		if withdrawalsOf(t, a.pool, account) != 1 {
			t.Errorf("%s: a second withdrawal", field)
		}
		if b := balancesOf(t, a.pool, account); b.available != 600 || b.held != 400 {
			t.Errorf("%s: balances %+v", field, b)
		}
		stored := a.get(t, "/withdrawals/"+first.json(t)["id"].(string))
		if stored.body != first.body {
			t.Errorf("%s: stored %s\nfirst  %s", field, stored.body, first.body)
		}
	}
}

func TestRefusesInsufficientFundsWith422StoringNothingAndTheKeyStaysFree(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 300)
	key := newKey()
	refused := a.withdraw(t, account, nil, key)
	if refused.status != 422 ||
		refused.json(t)["type"] != "urn:ledger-demo:problem:insufficient-funds" {
		t.Fatalf("got %d %s", refused.status, refused.body)
	}
	if withdrawalsOf(t, a.pool, account) != 0 || heldEventsOf(t, a.pool, account.ID.TypeID()) != 0 {
		t.Error("something was stored")
	}
	if b := balancesOf(t, a.pool, account); b != (balanceRow{0, 300, 0}) {
		t.Errorf("balances %+v", b)
	}

	// Decision 24: only a successful request takes a key. Once funded, the same key works.
	fund(t, a.pool, account, 200)
	if retried := a.withdraw(t, account, nil, key); retried.status != 201 {
		t.Errorf("retried: %d %s", retried.status, retried.body)
	}
	if b := balancesOf(t, a.pool, account); b.available != 100 || b.held != 400 {
		t.Errorf("balances %+v", b)
	}
}

func TestLetsExactlyOneOfTwoConcurrentWithdrawalsThatTogetherExceedAvailableThrough(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	start := make(chan struct{})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			<-start
			r := a.withdraw(t, account, map[string]any{"amount_minor": "700"}, newKey())
			codes[i] = r.status
		})
	}
	close(start)
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 422}) {
		t.Errorf("codes %v", codes)
	}
	if withdrawalsOf(t, a.pool, account) != 1 {
		t.Error("not exactly one withdrawal")
	}
	if b := balancesOf(t, a.pool, account); b != (balanceRow{0, 300, 700}) {
		t.Errorf("balances %+v", b)
	}
}

func TestAnswers404ForAnUnknownAccountStoringNothingTheKeyThenWorksOnARealOne(t *testing.T) {
	a := newTestAPI(t)
	unknown := ids.New[ids.Account]().TypeID()
	key := newKey()
	r := a.postWithdrawal(t, unknown, withdrawalBody(nil), key)
	if r.status != 404 || r.json(t)["type"] != "urn:ledger-demo:problem:account-not-found" {
		t.Errorf("got %d %s", r.status, r.body)
	}
	if heldEventsOf(t, a.pool, unknown) != 0 {
		t.Error("an event was stored")
	}
	account := fundedAccount(t, a.pool, 1000)
	if r := a.withdraw(t, account, nil, key); r.status != 201 {
		t.Errorf("on a real account: %d %s", r.status, r.body)
	}
}

func TestAnswers400StoringNothing(t *testing.T) {
	a := newTestAPI(t)
	tests := []struct {
		name      string
		overrides map[string]any
		key       string // "key": a fresh valid one; "": no header
		path      string
	}{
		{"a missing Idempotency-Key", nil, "", "idempotency-key"},
		{"an Idempotency-Key with a space", nil, "has space", "idempotency-key"},
		{"an Idempotency-Key over 255 characters", nil, strings.Repeat("k", 256), "idempotency-key"},
		{"an invalid IBAN", map[string]any{"creditor_iban": "DE00512108001245126199"}, "key", "creditor_iban"},
		{"an accent in the name", map[string]any{"creditor_name": "Zoë Müller"}, "key", "creditor_name"},
		{"'&' in the name", map[string]any{"creditor_name": "Smith & Sons"}, "key", "creditor_name"},
		{"a blank name", map[string]any{"creditor_name": "   "}, "key", "creditor_name"},
		{"a name over 70 characters", map[string]any{"creditor_name": strings.Repeat("a", 71)}, "key", "creditor_name"},
		{"an accent in the remittance", map[string]any{"remittance_info": "Café"}, "key", "remittance_info"},
		{"a misspelt (unknown) field", map[string]any{"remitance_info": "Rent"}, "key", ""},
		{`amount_minor "0"`, map[string]any{"amount_minor": "0"}, "key", "amount_minor"},
		{"amount_minor as a number", map[string]any{"amount_minor": 400}, "key", "amount_minor"},
		{"another currency", map[string]any{"currency": "USD"}, "key", "currency"},
		// Decision 60: above the SEPA Credit Transfer maximum (a 500 before, from an overflow).
		{"amount_minor above the SEPA maximum", map[string]any{"amount_minor": "100000000000"}, "key", "amount_minor"},
		{"the int8 maximum", map[string]any{"amount_minor": "9223372036854775807"}, "key", "amount_minor"},
	}
	for _, tt := range tests {
		account := fundedAccount(t, a.pool, 1000)
		key := tt.key
		if key == "key" {
			key = newKey()
		}
		r := a.withdraw(t, account, tt.overrides, key)
		got := r.json(t)
		if r.status != 400 || got["type"] != "urn:ledger-demo:problem:invalid-request" ||
			!hasErrorAt(got, tt.path) {
			t.Errorf("%s: %d %s", tt.name, r.status, r.body)
		}
		if withdrawalsOf(t, a.pool, account) != 0 {
			t.Errorf("%s: a withdrawal was stored", tt.name)
		}
	}
}

func hasErrorAt(problem map[string]any, path string) bool {
	errs, _ := problem["errors"].([]any)
	for _, e := range errs {
		if e.(map[string]any)["path"] == path {
			return true
		}
	}
	return false
}

func TestReportsEveryProblemAtOnceAccountKeyAndBody(t *testing.T) {
	a := newTestAPI(t)
	r := a.postWithdrawal(t, "acct_bad", withdrawalBody(map[string]any{"creditor_name": ""}), "")
	var paths []any
	for _, e := range r.json(t)["errors"].([]any) {
		paths = append(paths, e.(map[string]any)["path"])
	}
	want := []any{"account_id", "idempotency-key", "creditor_name"}
	if r.status != 400 || !reflect.DeepEqual(paths, want) {
		t.Errorf("got %d %v", r.status, paths)
	}
}

// Go-specific, matching Node: a header sent twice is one value joined with ", " by Node's HTTP
// parser, which then fails the pattern (measured), even when the two are equal.
func TestAnIdempotencyKeySentTwiceIsInvalid(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	data, _ := json.Marshal(withdrawalBody(nil))
	target := "/accounts/" + account.ID.TypeID() + "/withdrawals"
	r := a.postRaw(t, target, "application/json", string(data),
		http.Header{"Idempotency-Key": {"same", "same"}})
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
		`"errors":[{"path":"idempotency-key","message":"must be 1-255 characters from A-Z a-z 0-9 . _ : -"}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

// The withdrawal row of node/test/api/body-errors.test.ts: the client API answers framing errors
// with their exact codes (the webhooks with 400).
func TestAnswersBodyFramingErrorsWithTheirExactCodes(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	target := "/accounts/" + account.ID.TypeID() + "/withdrawals"
	oversized, _ := json.Marshal(map[string]string{"padding": strings.Repeat("x", 1_100_000)})
	for _, tt := range []struct {
		name, contentType, body string
		status                  int
		title, detail           string
	}{
		{"malformed JSON", "application/json", `{"amount_minor": `, 400, "Bad Request",
			"Body is not valid JSON but content-type is set to 'application/json'"},
		{"a non-JSON content type", "text/plain", "hello", 415, "Unsupported Media Type",
			"Unsupported Media Type"},
		{"an oversized body", "application/json", string(oversized), 413, "Payload Too Large",
			"Request body is too large"},
	} {
		header := http.Header{"Idempotency-Key": {newKey()}}
		r := a.postRaw(t, target, tt.contentType, tt.body, header)
		got := r.json(t)
		problemJSON := strings.HasPrefix(r.header.Get("Content-Type"), "application/problem+json")
		if r.status != tt.status || !problemJSON ||
			got["type"] != "about:blank" || got["title"] != tt.title || got["detail"] != tt.detail {
			t.Errorf("%s: %d %s", tt.name, r.status, r.body)
		}
	}
	if line := a.logLine(t, nil, "request rejected"); line == nil || line["level"] != "warn" ||
		line["status_code"] != 413.0 || line["reason"] != "Request body is too large" {
		t.Errorf("last rejected line = %v", line)
	}
}

func TestStoresWithdrawalHeldInTheOutboxInTheSameTransactionAndNothingForARepeat(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	key := newKey()
	r := a.withdraw(t, account, nil, key)
	created := r.json(t)
	wd, err := ids.FromTypeID[ids.Withdrawal](created["id"].(string))
	if err != nil {
		t.Fatal(err)
	}

	rows := outboxRowsFor(t, a.pool, wd.UUID().String())
	if len(rows) != 1 {
		t.Fatalf("%d outbox rows", len(rows))
	}
	row := rows[0]
	if row.EntityType != "withdrawal" || row.EntityID != wd.UUID().String() ||
		row.Subject != "withdrawal.held" || row.MsgID != created["id"].(string)+".held" ||
		row.PublishedAt != nil {
		t.Errorf("row = %+v", row)
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	eventID := idFromUUID[ids.Event](t, row.ID).TypeID()
	want := map[string]any{
		"event_id": eventID, "event_type": "withdrawal.held", "schema_version": 1.0,
		"withdrawal_id": created["id"], "account_id": account.ID.TypeID(), "amount_minor": "400",
		"currency": "EUR", "occurred_at": created["created_at"],
	}
	if !reflect.DeepEqual(payload, want) {
		t.Errorf("payload %v\nwant    %v", payload, want)
	}
	contracttest.CheckEvent(t, row.Payload)
	// No personal data in the event.
	for _, personal := range []string{creditorIBAN, "Jane Doe", "Rent October"} {
		if strings.Contains(string(row.Payload), personal) {
			t.Errorf("the event holds %q", personal)
		}
	}
	// Committed by the transaction that created the withdrawal.
	var same bool
	err = a.pool.QueryRow(t.Context(), `
SELECT w.xmin::text = o.xmin::text FROM withdrawals w JOIN outbox o ON o.entity_id = w.id
WHERE w.id = $1`, wd).Scan(&same)
	if err != nil || !same {
		t.Errorf("not one transaction: %v %v", same, err)
	}

	// A repeat and a conflict store no further event.
	a.withdraw(t, account, nil, key)
	a.withdraw(t, account, map[string]any{"amount_minor": "1"}, key)
	n := len(outboxRowsFor(t, a.pool, wd.UUID().String()))
	if n != 1 || heldEventsOf(t, a.pool, account.ID.TypeID()) != 1 {
		t.Errorf("%d outbox rows after a repeat and a conflict", n)
	}
}

func TestLogsOutcomesWithoutTheNameTheRemittanceTextOrTheFullIBAN(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 500)
	key := newKey()
	a.withdraw(t, account, nil, key)                                 // created
	a.withdraw(t, account, nil, key)                                 // repeat
	a.withdraw(t, account, map[string]any{"amount_minor": "1"}, key) // conflict
	a.withdraw(t, account, nil, newKey())                            // 422

	var domain []map[string]any
	for _, line := range a.logLines(t) {
		if line["request_id"] != nil && line["msg"] != "request completed" {
			domain = append(domain, line)
		}
	}
	var got [][2]any
	for _, l := range domain {
		got = append(got, [2]any{l["level"], l["msg"]})
	}
	want := [][2]any{
		{"info", "withdrawal created"},
		{"info", "repeated withdrawal request"},
		{"warn", "idempotency key reused with different details"},
		{"info", "withdrawal refused: insufficient funds"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %v", got)
	}
	first := domain[0]
	if !strings.HasPrefix(first["withdrawal_id"].(string), "wd_") ||
		!strings.HasPrefix(first["event_id"].(string), "evt_") ||
		first["amount_minor"] != "400" || first["creditor_iban"] != "DE75…6199" ||
		first["account_id"] != account.ID.TypeID() || first["idempotency_key"] != key {
		t.Errorf("created line %v", first)
	}
	if !reflect.DeepEqual(domain[2]["differing_fields"], []any{"amount_minor"}) {
		t.Errorf("conflict line %v", domain[2])
	}
	everything := a.logs.String()
	for _, personal := range []string{creditorIBAN, "Jane Doe", "Rent October"} {
		if strings.Contains(everything, personal) {
			t.Errorf("the logs hold %q", personal)
		}
	}
}

func TestGetReturnsTheWithdrawalAndItsHistoryWithIncludeHistory(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	created := a.withdraw(t, account, nil, newKey())
	url := "/withdrawals/" + created.json(t)["id"].(string)

	plain := a.get(t, url)
	if plain.status != 200 || plain.body != created.body {
		t.Errorf("plain: %d %s", plain.status, plain.body)
	}
	withHistory := a.get(t, url+"?include=history")
	got := withHistory.json(t)
	history, _ := got["history"].([]any)
	if len(history) != 1 {
		t.Fatalf("history %v", got["history"])
	}
	h := history[0].(map[string]any)
	if h["from_status"] != nil || h["to_status"] != "held" || h["reason"] != nil || h["at"] == nil {
		t.Errorf("history %v", h)
	}
	delete(got, "history")
	if !reflect.DeepEqual(got, created.json(t)) {
		t.Errorf("the rest differs: %v", got)
	}
}

func TestGetAnswers404ForAnUnknownWithdrawalAnd400ForAnInvalidID(t *testing.T) {
	a := newTestAPI(t)
	missing := a.get(t, "/withdrawals/"+ids.New[ids.Withdrawal]().TypeID())
	if missing.status != 404 ||
		missing.json(t)["type"] != "urn:ledger-demo:problem:withdrawal-not-found" {
		t.Errorf("missing: %d %s", missing.status, missing.body)
	}
	invalid := a.get(t, "/withdrawals/"+ids.New[ids.Deposit]().TypeID())
	if invalid.status != 400 {
		t.Errorf("invalid: %d %s", invalid.status, invalid.body)
	}
	badInclude := a.get(t, "/withdrawals/"+ids.New[ids.Withdrawal]().TypeID()+"?include=everything")
	if badInclude.status != 400 || !hasErrorAt(badInclude.json(t), "include") {
		t.Errorf("include: %d %s", badInclude.status, badInclude.body)
	}
}

// Node runs its reconciliation here (Node only, decision 50). The Go test checks what it would for
// this account: every bucket equal to its movements, and held equal to the withdrawals still held.
func TestTheAccountStaysConsistentAfterWithdrawalsThroughTheAPI(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	a.withdraw(t, account, map[string]any{"amount_minor": "300"}, "a")
	a.withdraw(t, account, map[string]any{"amount_minor": "300"}, "a")
	a.withdraw(t, account, map[string]any{"amount_minor": "200"}, "b")
	a.withdraw(t, account, map[string]any{"amount_minor": "900"}, "c") // 422

	var mismatches int
	err := a.pool.QueryRow(t.Context(), `
WITH moved AS (
  SELECT b.bucket,
         COALESCE(sum(m.amount_minor) FILTER (WHERE m.to_bucket = b.bucket), 0)
       - COALESCE(sum(m.amount_minor) FILTER (WHERE m.from_bucket = b.bucket), 0) AS net
  FROM (VALUES ('pending_in'), ('available'), ('held')) AS b(bucket)
  LEFT JOIN balance_movements m ON m.account_id = $1
  GROUP BY b.bucket
)
SELECT count(*)::int FROM moved, account_balances ab
WHERE ab.account_id = $1
  AND net <> CASE bucket WHEN 'pending_in' THEN ab.pending_in WHEN 'available' THEN ab.available
                         ELSE ab.held END`, account.ID).Scan(&mismatches)
	if err != nil || mismatches != 0 {
		t.Errorf("%d buckets differ from their movements (%v)", mismatches, err)
	}
	held := count(t, a.pool, `
SELECT (SELECT held FROM account_balances WHERE account_id = $1)::int
     - COALESCE((SELECT sum(amount_minor) FROM withdrawals
                 WHERE account_id = $1 AND status IN ('held', 'submitted')), 0)::int`,
		account.ID)
	if held != 0 || withdrawalsOf(t, a.pool, account) != 2 {
		t.Errorf("held differs from the held withdrawals by %d; %d withdrawals",
			held, withdrawalsOf(t, a.pool, account))
	}
}
