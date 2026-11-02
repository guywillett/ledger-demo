package api_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// GET /accounts/{account_id}/balance and /transactions against the test database: the cases of
// node/test/api/accounts.test.ts. Every response is checked against the contract (get).

func balancePath(account ids.AccountID) string {
	return "/accounts/" + account.TypeID() + "/balance"
}

func transactionsPath(account ids.AccountID, query string) string {
	path := "/accounts/" + account.TypeID() + "/transactions"
	if query != "" {
		path += "?" + query
	}
	return path
}

func TestBalanceStartsAtZero(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	r := a.get(t, balancePath(account.ID))
	if r.status != 200 {
		t.Fatalf("status = %d", r.status)
	}
	// Byte for byte Node's body.
	if want := `{"pending_in":"0","available":"0","held":"0","currency":"EUR"}`; r.body != want {
		t.Errorf("body = %s\nwant   %s", r.body, want)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestBalanceShowsPendingThenAvailable(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, account, 1250)

	book(t, a.pool, deposit)
	if got := a.get(t, balancePath(account.ID)).json(t); got["pending_in"] != "1250" || got["available"] != "0" {
		t.Errorf("after booking: %v", got)
	}
	// The rest of the lifecycle, approved (Book again skips).
	process(t, a.pool, deposit, deposits.Approve, "")
	if got := a.get(t, balancePath(account.ID)).json(t); got["pending_in"] != "0" || got["available"] != "1250" {
		t.Errorf("after crediting: %v", got)
	}
}

func TestBalanceKeepsAmountsBeyond2To53Exact(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	book(t, a.pool, testdb.NewDeposit(t, a.pool, account, 9007199254740993))
	if got := a.get(t, balancePath(account.ID)).json(t)["pending_in"]; got != "9007199254740993" {
		t.Errorf("pending_in = %v", got)
	}
}

func TestBalanceUnknownAccountIs404(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, balancePath(ids.New[ids.Account]()))
	if r.status != 404 || r.json(t)["type"] != "urn:ledger-demo:problem:account-not-found" {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

func TestBalanceInvalidAccountIDIs400(t *testing.T) {
	a := newTestAPI(t)
	for _, id := range []string{"acct_garbage", ids.New[ids.Deposit]().TypeID(), "alice"} {
		r := a.get(t, "/accounts/"+id+"/balance")
		want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
			`"errors":[{"path":"account_id","message":"must be a acct_ ID"}]}`
		if r.status != 400 || r.body != want {
			t.Errorf("%s: got %d %s", id, r.status, r.body)
		}
		if ct := r.header.Get("Content-Type"); ct != "application/problem+json; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", id, ct)
		}
	}
}

// depositIDs returns the response's deposits' (id, amount_minor, status).
func depositRows(t *testing.T, r response) [][3]any {
	t.Helper()
	var rows [][3]any
	for _, d := range r.json(t)["deposits"].([]any) {
		m := d.(map[string]any)
		rows = append(rows, [3]any{m["id"], m["amount_minor"], m["status"]})
	}
	return rows
}

func TestTransactionsListsThisAccountsDepositsNewestFirst(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	other := testdb.NewAccount(t, a.pool)
	first := testdb.NewDeposit(t, a.pool, account, 100)
	testdb.NewDeposit(t, a.pool, other, 999)
	second := testdb.NewDeposit(t, a.pool, account, 200)
	process(t, a.pool, first, deposits.Approve, "")

	r := a.get(t, transactionsPath(account.ID, ""))
	if r.status != 200 {
		t.Fatalf("status = %d", r.status)
	}
	want := [][3]any{
		{second.TypeID(), "200", "created"},
		{first.TypeID(), "100", "credited"},
	}
	if got := depositRows(t, r); !slices.Equal(got, want) {
		t.Errorf("deposits = %v\nwant       %v", got, want)
	}
	body := r.json(t)
	if _, ok := body["deposits"].([]any)[0].(map[string]any)["history"]; ok {
		t.Error("history present without include=history")
	}
	if body["next_before"] != nil {
		t.Errorf("next_before = %v, want null", body["next_before"])
	}
}

func TestTransactionsEmptyPageAndUnknownAccount(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	if r := a.get(t, transactionsPath(account.ID, "")); r.body != `{"deposits":[],"next_before":null}` {
		t.Errorf("empty page = %s", r.body)
	}
	if r := a.get(t, transactionsPath(ids.New[ids.Account](), "")); r.status != 404 {
		t.Errorf("unknown account: status = %d", r.status)
	}
}

func TestTransactionsPagesWithLimitAndBefore(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	var newestFirst []any
	for range 5 {
		newestFirst = slices.Insert(newestFirst, 0, any(testdb.NewDeposit(t, a.pool, account, 1250).TypeID()))
	}

	var seen, cursors []any
	query := "limit=2"
	for {
		page := a.get(t, transactionsPath(account.ID, query)).json(t)
		for _, d := range page["deposits"].([]any) {
			seen = append(seen, d.(map[string]any)["id"])
		}
		next := page["next_before"]
		cursors = append(cursors, next)
		if next == nil {
			break // null on the last page
		}
		query = "limit=2&before=" + next.(string)
	}
	if !slices.Equal(seen, newestFirst) {
		t.Errorf("seen = %v\nwant   %v", seen, newestFirst)
	}
	if want := []any{newestFirst[1], newestFirst[3], nil}; !slices.Equal(cursors, want) {
		t.Errorf("cursors = %v, want %v", cursors, want)
	}
}

func TestTransactionsIncludesHistory(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	approved := testdb.NewDeposit(t, a.pool, account, 1250)
	rejected := testdb.NewDeposit(t, a.pool, account, 1250)
	process(t, a.pool, approved, deposits.Approve, "")
	process(t, a.pool, rejected, deposits.Reject, "sanctions screening hit")

	histories := map[any][]any{}
	for _, d := range a.get(t, transactionsPath(account.ID, "include=history")).json(t)["deposits"].([]any) {
		m := d.(map[string]any)
		histories[m["id"]] = m["history"].([]any)
	}
	var pairs [][2]any
	for _, h := range histories[approved.TypeID()] {
		m := h.(map[string]any)
		pairs = append(pairs, [2]any{m["from_status"], m["to_status"]})
	}
	want := [][2]any{
		{nil, "created"}, {"created", "received"}, {"received", "under_review"},
		{"under_review", "approved"}, {"approved", "credited"},
	}
	if !slices.Equal(pairs, want) {
		t.Errorf("approved history = %v", pairs)
	}
	decision := histories[rejected.TypeID()][3].(map[string]any)
	if decision["from_status"] != "under_review" || decision["to_status"] != "rejected" ||
		decision["reason"] != "sanctions screening hit" {
		t.Errorf("rejected decision = %v", decision)
	}
	// Node's timestamp format (decision 44).
	if at, _ := decision["at"].(string); len(at) != len("2026-10-30T09:21:52.130Z") || !strings.HasSuffix(at, "Z") {
		t.Errorf("at = %q, want milliseconds in UTC", at)
	}
}

func TestTransactionsInvalidQueryIs400(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	tests := []struct{ query, path string }{
		{"limit=0", "limit"},
		{"limit=201", "limit"},
		{"limit=lots", "limit"},
		{"before=" + ids.New[ids.Account]().TypeID(), "before"},
		{"include=everything", "include"},
		// Measured against Node's API (see parseQuery and jsNumber):
		{"limit=", "limit"},          // Number("") is 0
		{"limit=2.5", "limit"},       // not a whole number
		{"limit=-0", "limit"},        // too small
		{"limit=Infinity", "limit"},  // not a whole number
		{"limit=2&limit=3", "limit"}, // given twice
		{"include=history&include=history", "include"},
		{"limit=2;x", "limit"}, // ; is part of the value, not a separator
		{"limit=%zz", "limit"}, // a bad escape is kept as written
		{"before=", "before"},
	}
	for _, tt := range tests {
		r := a.get(t, transactionsPath(account.ID, tt.query))
		errs, _ := r.json(t)["errors"].([]any)
		if r.status != 400 || len(errs) != 1 || errs[0].(map[string]any)["path"] != tt.path {
			t.Errorf("%s: got %d %s, want 400 at %s", tt.query, r.status, r.body, tt.path)
		}
	}
}

// Zod's message for each kind of bad limit, as Node's API answers (measured): only the path is part
// of the contract, but the messages are Zod's too, so a client sees the same either way.
func TestTransactionsLimitMessagesAreZods(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	tests := []struct {
		limit    string
		messages []any
	}{
		{"lots", []any{"Invalid input: expected number, received NaN"}},
		{"Infinity", []any{"Invalid input: expected number, received Infinity"}},
		{"-Infinity", []any{"Invalid input: expected number, received -Infinity"}},
		{"1e400", []any{"Invalid input: expected number, received Infinity"}},
		// Not whole: that one error, even out of bounds.
		{"2.5", []any{"Invalid input: expected int, received number"}},
		{"201.5", []any{"Invalid input: expected int, received number"}},
		{"-2.5", []any{"Invalid input: expected int, received number"}},
		// Outside JavaScript's safe range: the int error, then the bound.
		{"1e300", []any{"Too big: expected int to be <=9007199254740991", "Too big: expected number to be <=200"}},
		{"9007199254740992", []any{"Too big: expected int to be <=9007199254740991", "Too big: expected number to be <=200"}},
		{"0xFFFFFFFFFFFFFFFFFFFF", []any{"Too big: expected int to be <=9007199254740991", "Too big: expected number to be <=200"}},
		{"-1e300", []any{"Too small: expected int to be >=-9007199254740991", "Too small: expected number to be >=1"}},
		// Within it: the bound alone.
		{"9007199254740991", []any{"Too big: expected number to be <=200"}},
		{"201", []any{"Too big: expected number to be <=200"}},
		{"0", []any{"Too small: expected number to be >=1"}},
		{"-5", []any{"Too small: expected number to be >=1"}},
	}
	for _, tt := range tests {
		r := a.get(t, transactionsPath(account.ID, "limit="+tt.limit))
		var messages []any
		for _, e := range r.json(t)["errors"].([]any) {
			messages = append(messages, e.(map[string]any)["message"])
		}
		if !slices.Equal(messages, tt.messages) {
			t.Errorf("limit=%s: messages %q\nwant %q", tt.limit, messages, tt.messages)
		}
	}
}

// Every problem in the query is reported, in the schema's order (include, limit, before), as Zod's.
func TestTransactionsReportsEveryQueryProblem(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	r := a.get(t, transactionsPath(account.ID, "limit=0&include=x"))
	var paths []any
	for _, e := range r.json(t)["errors"].([]any) {
		paths = append(paths, e.(map[string]any)["path"])
	}
	if !slices.Equal(paths, []any{"include", "limit"}) {
		t.Errorf("error paths = %v, want [include limit]", paths)
	}
}

// limit is read as JavaScript's Number() reads it, so these are all valid, as in Node.
func TestTransactionsLimitIsReadAsNodeReadsIt(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	for _, query := range []string{
		"limit=%202", "limit=+2", "limit=2%0A", "limit=0x10", "limit=0b11", "limit=1e2", "limit=.5e1",
		"limit=2.0", "limit=%32",
	} {
		if r := a.get(t, transactionsPath(account.ID, query)); r.status != 200 {
			t.Errorf("%s: status = %d %s, want 200", query, r.status, r.body)
		}
	}
}

func TestTransactionsIgnoresUnknownQueryParameters(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	for _, query := range []string{"sort=asc", "LIMIT=999", "%zz=1", "=x", "limit=2&&"} {
		if r := a.get(t, transactionsPath(account.ID, query)); r.status != 200 {
			t.Errorf("%s: status = %d", query, r.status)
		}
	}
}
