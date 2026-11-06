package api_test

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// GET /accounts/{account_id}/deposits and /deposits/{deposit_id} against the test database: the
// cases of node/test/api/deposits.test.ts. Every response is checked against the contract (get).

func depositsPath(account ids.AccountID, query string) string {
	path := "/accounts/" + account.TypeID() + "/deposits"
	if query != "" {
		path += "?" + query
	}
	return path
}

// depositRows returns the response's deposits' (id, amount_minor, status).
func depositRows(t *testing.T, r response) [][3]any {
	t.Helper()
	var rows [][3]any
	for _, d := range objects(t, r.json(t), "data") {
		rows = append(rows, [3]any{d["id"], d["amount_minor"], d["status"]})
	}
	return rows
}

func TestDepositsListsThisAccountsDepositsNewestFirst(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	other := testdb.NewAccount(t, a.pool)
	first := testdb.NewDeposit(t, a.pool, account, 100)
	testdb.NewDeposit(t, a.pool, other, 999)
	second := testdb.NewDeposit(t, a.pool, account, 200)
	process(t, a.pool, first, deposits.Approve, "")

	r := a.get(t, depositsPath(account.ID, ""))
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
	if _, ok := body["data"].([]any)[0].(map[string]any)["history"]; ok {
		t.Error("history present without include=history")
	}
	if body["next_before"] != nil {
		t.Errorf("next_before = %v, want null", body["next_before"])
	}
}

func TestDepositsEmptyPageAndUnknownAccount(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	if r := a.get(t, depositsPath(account.ID, "")); r.body != `{"data":[],"next_before":null}` {
		t.Errorf("empty page = %s", r.body)
	}
	if r := a.get(t, depositsPath(ids.New[ids.Account](), "")); r.status != 404 {
		t.Errorf("unknown account: status = %d", r.status)
	}
}

func TestDepositsPagesWithLimitAndBefore(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	var newestFirst []any
	for range 5 {
		newestFirst = slices.Insert(newestFirst, 0, any(testdb.NewDeposit(t, a.pool, account, 1250).TypeID()))
	}

	var seen, cursors []any
	query := "limit=2"
	for {
		page := a.get(t, depositsPath(account.ID, query)).json(t)
		for _, d := range objects(t, page, "data") {
			seen = append(seen, d["id"])
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

func TestDepositsIncludesHistoryWithoutReasons(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	approved := testdb.NewDeposit(t, a.pool, account, 1250)
	rejected := testdb.NewDeposit(t, a.pool, account, 1250)
	process(t, a.pool, approved, deposits.Approve, "")
	process(t, a.pool, rejected, deposits.Reject, "sanctions screening hit")

	histories := map[any][]map[string]any{}
	for _, d := range objects(t, a.get(t, depositsPath(account.ID, "include=history")).json(t), "data") {
		histories[d["id"]] = objects(t, d, "history")
	}
	var pairs [][2]any
	for _, h := range histories[approved.TypeID()] {
		pairs = append(pairs, [2]any{h["from_status"], h["to_status"]})
	}
	want := [][2]any{
		{nil, "created"}, {"created", "received"}, {"received", "under_review"},
		{"under_review", "approved"}, {"approved", "credited"},
	}
	if !slices.Equal(pairs, want) {
		t.Errorf("approved history = %v", pairs)
	}
	// The compliance decision's reason is internal (slice 4 decision 76): null in the history.
	if len(histories[rejected.TypeID()]) < 4 {
		t.Fatalf("rejected history = %v, want its decision 4th", histories[rejected.TypeID()])
	}
	decision := histories[rejected.TypeID()][3]
	if decision["from_status"] != "under_review" || decision["to_status"] != "rejected" ||
		decision["reason"] != nil {
		t.Errorf("rejected decision = %v, want its reason null", decision)
	}
	// Node's timestamp format (decision 44).
	if at, _ := decision["at"].(string); len(at) != len("2026-10-30T09:21:52.130Z") || !strings.HasSuffix(at, "Z") {
		t.Errorf("at = %q, want milliseconds in UTC", at)
	}
}

func TestDepositsInvalidQueryIs400(t *testing.T) {
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
		r := a.get(t, depositsPath(account.ID, tt.query))
		errs, _ := r.json(t)["errors"].([]any)
		if r.status != 400 || len(errs) != 1 || errs[0].(map[string]any)["path"] != tt.path {
			t.Errorf("%s: got %d %s, want 400 at %s", tt.query, r.status, r.body, tt.path)
		}
	}
}

// before given twice: Node's parser makes it an array, which typeIdSchema (a z.string()) refuses
// with Zod's message for it, not the TypeID's.
func TestDepositsRepeatedBeforeIsZodsArrayMessage(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	r := a.get(t, depositsPath(account.ID, "before="+ids.New[ids.Deposit]().TypeID()+"&before="+ids.New[ids.Deposit]().TypeID()))
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
		`"errors":[{"path":"before","message":"Invalid input: expected string, received array"}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s\nwant %s", r.status, r.body, want)
	}
}

// Zod's message for each kind of bad limit, as Node's API answers (measured): only the path is part
// of the contract, but the messages are Zod's too, so a client sees the same either way.
func TestDepositsLimitMessagesAreZods(t *testing.T) {
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
		r := a.get(t, depositsPath(account.ID, "limit="+tt.limit))
		var messages []any
		for _, e := range objects(t, r.json(t), "errors") {
			messages = append(messages, e["message"])
		}
		if !slices.Equal(messages, tt.messages) {
			t.Errorf("limit=%s: messages %q\nwant %q", tt.limit, messages, tt.messages)
		}
	}
}

// Every problem in the query is reported, in the contract's parameter order (limit, before,
// include; slice 4 decision 68), whatever order the query sends them in.
func TestDepositsReportsEveryQueryProblem(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	r := a.get(t, depositsPath(account.ID, "include=x&limit=0"))
	var paths []any
	for _, e := range objects(t, r.json(t), "errors") {
		paths = append(paths, e["path"])
	}
	if !slices.Equal(paths, []any{"limit", "include"}) {
		t.Errorf("error paths = %v, want [limit include]", paths)
	}
}

// Every problem at once, the path then the query (slice 4 decision 68): byte for byte Node's
// answer (node/test/api/deposits.test.ts, "reports every problem at once").
func TestDepositsReportsThePathAndTheQueryProblemsTogether(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, "/accounts/nope/deposits?include=everything&before=nope&limit=0")
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
		`"errors":[{"path":"account_id","message":"must be a acct_ ID"},` +
		`{"path":"limit","message":"Too small: expected number to be >=1"},` +
		`{"path":"before","message":"must be a dep_ ID"},` +
		`{"path":"include","message":"Invalid input: expected \"history\""}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}

// limit is read as JavaScript's Number() reads it, so these are all valid, as in Node.
func TestDepositsLimitIsReadAsNodeReadsIt(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	for _, query := range []string{
		"limit=%202", "limit=+2", "limit=2%0A", "limit=0x10", "limit=0b11", "limit=1e2", "limit=.5e1",
		"limit=2.0", "limit=%32",
	} {
		if r := a.get(t, depositsPath(account.ID, query)); r.status != 200 {
			t.Errorf("%s: status = %d %s, want 200", query, r.status, r.body)
		}
	}
}

func TestDepositsIgnoresUnknownQueryParameters(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	for _, query := range []string{"sort=asc", "LIMIT=999", "%zz=1", "=x", "limit=2&&"} {
		if r := a.get(t, depositsPath(account.ID, query)); r.status != 200 {
			t.Errorf("%s: status = %d", query, r.status)
		}
	}
}

func depositPath(account ids.AccountID, deposit ids.DepositID, query string) string {
	path := "/accounts/" + account.TypeID() + "/deposits/" + deposit.TypeID()
	if query != "" {
		path += "?" + query
	}
	return path
}

// storedTimes are the deposit's created_at and updated_at as the API writes them (Node's
// toISOString: milliseconds, truncated, in UTC), read from the database.
func storedTimes(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID) (string, string) {
	t.Helper()
	var created, updated time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT created_at, updated_at FROM deposits WHERE id = $1`, deposit).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	const iso = "2006-01-02T15:04:05.000Z"
	return created.UTC().Format(iso), updated.UTC().Format(iso)
}

// depositBody is a deposit's representation, byte for byte Node's: type first, no
// provider_account_id, failure_reason after status. testdb.NewDeposit stores network_transaction_id
// "go-test-" and the deposit's TypeID, and the account's IBAN as creditor_iban.
func depositBody(t *testing.T, pool *pgxpool.Pool, account testdb.Account, deposit ids.DepositID,
	amount, status, failureReason string) string {
	t.Helper()
	created, updated := storedTimes(t, pool, deposit)
	return `{"type":"deposit","id":"` + deposit.TypeID() + `","account_id":"` + account.ID.TypeID() +
		`","creditor_iban":"` + account.IBAN + `","amount_minor":"` + amount +
		`","currency":"EUR","network":"SEPA","network_transaction_id":"go-test-` + deposit.TypeID() +
		`","status":"` + status + `","failure_reason":` + failureReason +
		`,"notification_count":1,"created_at":"` + created + `","updated_at":"` + updated + `"}`
}

// The list's representation, byte for byte Node's (deposits.test.ts, "returns the deposit"): the
// same as the single deposit's.
func TestDepositsListsEachDepositInItsFullRepresentation(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, account, 1250)
	r := a.get(t, depositsPath(account.ID, ""))
	want := `{"data":[` + depositBody(t, a.pool, account, deposit, "1250", "created", "null") +
		`],"next_before":null}`
	if r.status != 200 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}

// failureReasonCases: a deposit taken to each status, and the failure_reason it shows (slice 4
// decisions 69 and 76): one neutral text for both failure statuses, whatever the internal reason;
// null for the others.
var failureReasonCases = []struct {
	status        string
	setUp         func(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID)
	failureReason any
}{
	{"created", func(*testing.T, *pgxpool.Pool, ids.DepositID) {}, nil},
	{"approved", func(t *testing.T, pool *pgxpool.Pool, d ids.DepositID) {
		decideOnly(t, pool, d, deposits.Approve, "")
	}, nil},
	{"credited", func(t *testing.T, pool *pgxpool.Pool, d ids.DepositID) {
		process(t, pool, d, deposits.Approve, "")
	}, nil},
	{"rejected", func(t *testing.T, pool *pgxpool.Pool, d ids.DepositID) {
		decideOnly(t, pool, d, deposits.Reject, "sanctions screening hit")
	}, "returned to the sender"},
	{"returned", func(t *testing.T, pool *pgxpool.Pool, d ids.DepositID) {
		process(t, pool, d, deposits.Reject, "sanctions screening hit")
	}, "returned to the sender"},
}

func TestDepositsListGivesEachStatusItsFailureReason(t *testing.T) {
	a := newTestAPI(t)
	for _, tt := range failureReasonCases {
		account := testdb.NewAccount(t, a.pool)
		tt.setUp(t, a.pool, testdb.NewDeposit(t, a.pool, account, 1250))
		r := a.get(t, depositsPath(account.ID, ""))
		item := objects(t, r.json(t), "data")[0]
		if item["status"] != tt.status || item["failure_reason"] != tt.failureReason {
			t.Errorf("%s: status %v, failure_reason %v; want %v", tt.status, item["status"],
				item["failure_reason"], tt.failureReason)
		}
		if strings.Contains(r.body, "sanctions") {
			t.Errorf("%s: the internal reason reached the client: %s", tt.status, r.body)
		}
	}
}

// The single deposit, byte for byte Node's.
func TestDepositReturnsTheDeposit(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, account, 1250)
	r := a.get(t, depositPath(account.ID, deposit, ""))
	want := depositBody(t, a.pool, account, deposit, "1250", "created", "null")
	if r.status != 200 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestDepositIsTheSameRepresentationAsInTheList(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, account, 1250)
	process(t, a.pool, deposit, deposits.Reject, "sanctions screening hit")
	single := a.get(t, depositPath(account.ID, deposit, "include=history"))
	list := a.get(t, depositsPath(account.ID, "include=history"))
	if want := `{"data":[` + single.body + `],"next_before":null}`; single.status != 200 || list.body != want {
		t.Errorf("single %d %s\nlist      %s", single.status, single.body, list.body)
	}
}

// Its history, every reason null (slice 4 decision 76), the compliance decision's included.
func TestDepositIncludesItsHistoryWithoutReasons(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, account, 1250)
	process(t, a.pool, deposit, deposits.Reject, "sanctions screening hit")
	r := a.get(t, depositPath(account.ID, deposit, "include=history"))
	history, _ := r.json(t)["history"].([]any)
	var rows [][3]any
	for _, h := range history {
		m := h.(map[string]any)
		rows = append(rows, [3]any{m["from_status"], m["to_status"], m["reason"]})
	}
	want := [][3]any{
		{nil, "created", nil}, {"created", "received", nil}, {"received", "under_review", nil},
		{"under_review", "rejected", nil}, {"rejected", "returned", nil},
	}
	if r.status != 200 || !slices.Equal(rows, want) {
		t.Errorf("got %d, history %v\nwant %v", r.status, rows, want)
	}
	if strings.Contains(r.body, "sanctions") {
		t.Errorf("the internal reason reached the client: %s", r.body)
	}
}

func TestDepositGivesEachStatusItsFailureReason(t *testing.T) {
	a := newTestAPI(t)
	for _, tt := range failureReasonCases {
		account := testdb.NewAccount(t, a.pool)
		deposit := testdb.NewDeposit(t, a.pool, account, 1250)
		tt.setUp(t, a.pool, deposit)
		r := a.get(t, depositPath(account.ID, deposit, ""))
		body := r.json(t)
		if r.status != 200 || body["status"] != tt.status || body["failure_reason"] != tt.failureReason {
			t.Errorf("%s: got %d %s; want failure_reason %v", tt.status, r.status, r.body, tt.failureReason)
		}
	}
}

// The single read has no list, so the list's limit and before are unknown parameters to it:
// ignored, as Node ignores them, not validated.
func TestDepositIgnoresTheListsQueryParameters(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, account, 1250)
	for _, query := range []string{"limit=0", "before=nope", "limit=lots&before=nope"} {
		if r := a.get(t, depositPath(account.ID, deposit, query)); r.status != 200 {
			t.Errorf("%s: got %d %s, want 200", query, r.status, r.body)
		}
	}
}

const depositNotFoundBody = `{"type":"urn:ledger-demo:problem:deposit-not-found","title":"Deposit not found","status":404}`

// The account in the path is a scope (slice 4 decision 64), decided by the one query: another
// account's deposit, a real deposit under an unknown account and an unknown deposit all answer
// deposit-not-found.
func TestDepositOfAnotherAccountIsNotFound(t *testing.T) {
	a := newTestAPI(t)
	owner := testdb.NewAccount(t, a.pool)
	other := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, owner, 1250)
	if r := a.get(t, depositPath(other.ID, deposit, "")); r.status != 404 || r.body != depositNotFoundBody {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

func TestDepositUnderAnUnknownAccountIsNotFound(t *testing.T) {
	a := newTestAPI(t)
	owner := testdb.NewAccount(t, a.pool)
	deposit := testdb.NewDeposit(t, a.pool, owner, 1250)
	if r := a.get(t, depositPath(ids.New[ids.Account](), deposit, "")); r.status != 404 || r.body != depositNotFoundBody {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

func TestDepositUnknownIsNotFound(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	if r := a.get(t, depositPath(account.ID, ids.New[ids.Deposit](), "")); r.status != 404 || r.body != depositNotFoundBody {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

// Both IDs and the query at once, in the contract's order (slice 4 decision 68): byte for byte
// Node's answer (deposits.test.ts, "reports both invalid IDs and an invalid include at once").
func TestDepositReportsBothInvalidIDsAndAnInvalidIncludeAtOnce(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, "/accounts/nope/deposits/"+ids.New[ids.Withdrawal]().TypeID()+"?include=everything")
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
		`"errors":[{"path":"account_id","message":"must be a acct_ ID"},` +
		`{"path":"deposit_id","message":"must be a dep_ ID"},` +
		`{"path":"include","message":"Invalid input: expected \"history\""}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}

// The list's filters (slice 4 decision 70): exact matches, combined with each other and with
// pagination. The cases of node/test/api/deposits.test.ts ("GET /accounts/:id/deposits
// filtered").

const statusMessage = `Invalid option: expected one of "created"|"received"|"under_review"|"approved"|"rejected"|"credited"|"returned"`

// accountWithStatuses: an account with a deposit in each of three statuses, newest last.
func accountWithStatuses(t *testing.T, pool *pgxpool.Pool) (testdb.Account, map[string]ids.DepositID) {
	t.Helper()
	account := testdb.NewAccount(t, pool)
	credited := testdb.NewDeposit(t, pool, account, 1250)
	process(t, pool, credited, deposits.Approve, "")
	returned := testdb.NewDeposit(t, pool, account, 1250)
	process(t, pool, returned, deposits.Reject, "sanctions screening hit")
	created := testdb.NewDeposit(t, pool, account, 1250)
	return account, map[string]ids.DepositID{"created": created, "credited": credited, "returned": returned}
}

// listedIDs are the IDs of a list response's items. Fatal (to this test only, not a panic that
// would stop the package's other tests) if the response isn't a list of objects (see objects).
func listedIDs(t *testing.T, r response) []any {
	t.Helper()
	var found []any
	for _, item := range objects(t, r.json(t), "data") {
		found = append(found, item["id"])
	}
	return found
}

func TestDepositsFilterByStatus(t *testing.T) {
	a := newTestAPI(t)
	for _, status := range []string{"created", "credited", "returned"} {
		account, byStatus := accountWithStatuses(t, a.pool)
		r := a.get(t, depositsPath(account.ID, "status="+status))
		if got, want := listedIDs(t, r), []any{byStatus[status].TypeID()}; r.status != 200 || !slices.Equal(got, want) {
			t.Errorf("status=%s: got %d %v, want %v", status, r.status, got, want)
		}
	}
}

func TestDepositsFilterByAValidStatusNoneIsInIsEmpty(t *testing.T) {
	a := newTestAPI(t)
	account, _ := accountWithStatuses(t, a.pool)
	if r := a.get(t, depositsPath(account.ID, "status=under_review")); r.body != `{"data":[],"next_before":null}` {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

func TestDepositsFilterByNetworkTransactionIDExactly(t *testing.T) {
	a := newTestAPI(t)
	account, byStatus := accountWithStatuses(t, a.pool)
	ntx := "go-test-" + byStatus["credited"].TypeID() // as testdb.NewDeposit stores it
	r := a.get(t, depositsPath(account.ID, "network_transaction_id="+ntx))
	if got, want := listedIDs(t, r), []any{byStatus["credited"].TypeID()}; !slices.Equal(got, want) {
		t.Errorf("got %d %v, want %v", r.status, got, want)
	}
	// A prefix of it matches nothing: exact, not a search.
	prefix := a.get(t, depositsPath(account.ID, "network_transaction_id="+ntx[:len(ntx)-1]))
	if prefix.body != `{"data":[],"next_before":null}` {
		t.Errorf("prefix: got %d %s", prefix.status, prefix.body)
	}
	// Another account's deposit isn't found by it.
	other := testdb.NewAccount(t, a.pool)
	if r := a.get(t, depositsPath(other.ID, "network_transaction_id="+ntx)); r.body != `{"data":[],"next_before":null}` {
		t.Errorf("another account: got %d %s", r.status, r.body)
	}
}

func TestDepositsFilterCombinesBoth(t *testing.T) {
	a := newTestAPI(t)
	account, byStatus := accountWithStatuses(t, a.pool)
	query := "network_transaction_id=go-test-" + byStatus["credited"].TypeID() + "&status="
	if got := listedIDs(t, a.get(t, depositsPath(account.ID, query+"credited"))); !slices.Equal(got, []any{byStatus["credited"].TypeID()}) {
		t.Errorf("matching status: %v", got)
	}
	if got := listedIDs(t, a.get(t, depositsPath(account.ID, query+"returned"))); len(got) != 0 {
		t.Errorf("other status: %v, want none", got)
	}
}

// limit and before apply within the filtered list; next_before is the last deposit of the
// filtered page, so following it skips the deposits the filter leaves out.
func TestDepositsFilterPagesWithItsOwnCursors(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	var newestFirst []any
	for range 5 {
		deposit := testdb.NewDeposit(t, a.pool, account, 1250)
		process(t, a.pool, deposit, deposits.Approve, "")
		newestFirst = slices.Insert(newestFirst, 0, any(deposit.TypeID()))
		testdb.NewDeposit(t, a.pool, account, 1250) // left created, between them
	}

	var seen, cursors []any
	query := "status=credited&limit=2"
	for {
		page := a.get(t, depositsPath(account.ID, query))
		seen = append(seen, listedIDs(t, page)...)
		next := page.json(t)["next_before"]
		cursors = append(cursors, next)
		if next == nil {
			break
		}
		query = "status=credited&limit=2&before=" + next.(string)
	}
	if !slices.Equal(seen, newestFirst) {
		t.Errorf("seen = %v\nwant   %v", seen, newestFirst)
	}
	if want := []any{newestFirst[1], newestFirst[3], nil}; !slices.Equal(cursors, want) {
		t.Errorf("cursors = %v, want %v", cursors, want)
	}
}

func TestDepositsFilterInvalidIs400(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	tests := []struct{ query, path, message string }{
		{"status=unmatched", "status", statusMessage},
		{"status=nope", "status", statusMessage},
		{"status=", "status", statusMessage},
		{"status=created&status=credited", "status", statusMessage},
		{"network_transaction_id=", "network_transaction_id", "Too small: expected string to have >=1 characters"},
		{"network_transaction_id=" + strings.Repeat("x", 256), "network_transaction_id", "Too big: expected string to have <=255 characters"},
		{"network_transaction_id=a&network_transaction_id=b", "network_transaction_id", "Invalid input: expected string, received array"},
	}
	for _, tt := range tests {
		r := a.get(t, depositsPath(account.ID, tt.query))
		want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
			`"errors":[{"path":"` + tt.path + `","message":` + strconv.Quote(tt.message) + `}]}`
		if r.status != 400 || r.body != want {
			t.Errorf("%s: got %d %s\nwant %s", tt.query, r.status, r.body, want)
		}
	}
}

// 255 characters counted as Zod counts them, in code points: 255 emoji are valid.
func TestDepositsFilterAcceptsA255CodePointNetworkTransactionID(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	r := a.get(t, depositsPath(account.ID, "network_transaction_id="+url.QueryEscape(strings.Repeat("😀", 255))))
	if r.status != 200 {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

// Every problem at once, in the contract's parameter order (decision 68): byte for byte Node's
// answer (deposits.test.ts, "reports every problem at once, in the contract's order").
func TestDepositsFilterReportsEveryProblemInTheContractsOrder(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, "/accounts/nope/deposits?network_transaction_id=&status=nope&include=x&before=nope&limit=0")
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,"errors":[` +
		`{"path":"account_id","message":"must be a acct_ ID"},` +
		`{"path":"limit","message":"Too small: expected number to be >=1"},` +
		`{"path":"before","message":"must be a dep_ ID"},` +
		`{"path":"include","message":"Invalid input: expected \"history\""},` +
		`{"path":"status","message":` + strconv.Quote(statusMessage) + `},` +
		`{"path":"network_transaction_id","message":"Too small: expected string to have >=1 characters"}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}
