package api_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// GET /accounts/{account_id}/withdrawals against the test database: the cases of
// node/test/api/withdrawals.test.ts ("GET /accounts/:account_id/withdrawals"). Every response is
// checked against the contract (get).

const withdrawalStatusMessage = `Invalid option: expected one of "held"|"submitted"|"executed"|"failed"`

func withdrawalsPath(account ids.AccountID, query string) string {
	path := "/accounts/" + account.TypeID() + "/withdrawals"
	if query != "" {
		path += "?" + query
	}
	return path
}

func TestWithdrawalsListsTheAccountsNewestFirstAsTheSingleReadShowsThem(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	other := fundedAccount(t, a.pool, 1000)
	first := a.withdrawalIn(t, account, withdrawals.StatusExecuted)
	a.withdrawalIn(t, other, withdrawals.StatusHeld)
	second := a.withdrawalIn(t, account, withdrawals.StatusHeld)

	r := a.get(t, withdrawalsPath(account.ID, ""))
	if got := listedIDs(t, r); r.status != 200 || !slices.Equal(got, []any{second, first}) {
		t.Errorf("got %d %v, want [second first]", r.status, got)
	}
	single := a.get(t, withdrawalPath(account.ID, second, ""))
	if want := `{"data":[` + single.body + `,`; len(r.body) < len(want) || r.body[:len(want)] != want {
		t.Errorf("list %s\ndoesn't start with the single read's body %s", r.body, single.body)
	}
	if r.json(t)["next_before"] != nil {
		t.Errorf("next_before = %v, want null", r.json(t)["next_before"])
	}
}

func TestWithdrawalsEmptyListAndUnknownAccount(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	if r := a.get(t, withdrawalsPath(account.ID, "")); r.body != `{"data":[],"next_before":null}` {
		t.Errorf("empty: %d %s", r.status, r.body)
	}
	r := a.get(t, withdrawalsPath(ids.New[ids.Account](), ""))
	if r.status != 404 || r.json(t)["type"] != "urn:ledger-demo:problem:account-not-found" {
		t.Errorf("unknown account: %d %s", r.status, r.body)
	}
}

func TestWithdrawalsPagesWithLimitAndBefore(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	var newestFirst []any
	for range 5 {
		newestFirst = slices.Insert(newestFirst, 0, any(a.withdrawalIn(t, account, withdrawals.StatusHeld)))
	}
	seen, cursors := walkWithdrawals(t, a, account.ID, "limit=2")
	if !slices.Equal(seen, newestFirst) {
		t.Errorf("seen = %v\nwant   %v", seen, newestFirst)
	}
	if want := []any{newestFirst[1], newestFirst[3], nil}; !slices.Equal(cursors, want) {
		t.Errorf("cursors = %v, want %v", cursors, want)
	}
}

// walkWithdrawals reads every page of the account's list with the query, following next_before,
// and returns the IDs seen and the cursors.
func walkWithdrawals(t *testing.T, a testAPI, account ids.AccountID, query string) (seen, cursors []any) {
	t.Helper()
	next := query
	for {
		page := a.get(t, withdrawalsPath(account, next))
		seen = append(seen, listedIDs(t, page)...)
		cursor := page.json(t)["next_before"]
		cursors = append(cursors, cursor)
		if cursor == nil {
			return seen, cursors
		}
		next = query + "&before=" + cursor.(string)
	}
}

func TestWithdrawalsIncludesEachHistory(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	failed := a.withdrawalIn(t, account, withdrawals.StatusFailed)
	r := a.get(t, withdrawalsPath(account.ID, "include=history"))
	if got := listedIDs(t, r); len(got) != 1 {
		t.Fatalf("got %v, want one withdrawal", got)
	}
	item := objects(t, r.json(t), "data")[0]
	var rows [][3]any
	for _, h := range objects(t, item, "history") {
		rows = append(rows, [3]any{h["from_status"], h["to_status"], h["reason"]})
	}
	want := [][3]any{{nil, "held", nil}, {"held", "failed", "rejected by the beneficiary bank"}}
	if item["id"] != failed || !slices.Equal(rows, want) {
		t.Errorf("item %v, history %v; want %v", item["id"], rows, want)
	}
	// The same history as the single read's.
	single := a.get(t, withdrawalPath(account.ID, failed, "include=history"))
	if want := `{"data":[` + single.body + `],"next_before":null}`; r.body != want {
		t.Errorf("list %s\nwant      %s", r.body, want)
	}
}

func TestWithdrawalsFilterByStatus(t *testing.T) {
	a := newTestAPI(t)
	for _, status := range []withdrawals.Status{
		withdrawals.StatusHeld, withdrawals.StatusSubmitted, withdrawals.StatusExecuted, withdrawals.StatusFailed,
	} {
		account := fundedAccount(t, a.pool, 1000)
		byStatus := map[withdrawals.Status]string{}
		for _, s := range []withdrawals.Status{
			withdrawals.StatusHeld, withdrawals.StatusSubmitted, withdrawals.StatusExecuted, withdrawals.StatusFailed,
		} {
			byStatus[s] = a.withdrawalIn(t, account, s)
		}
		r := a.get(t, withdrawalsPath(account.ID, "status="+string(status)))
		if got := listedIDs(t, r); !slices.Equal(got, []any{byStatus[status]}) {
			t.Errorf("status=%s: got %d %v, want %v", status, r.status, got, byStatus[status])
		}
	}
}

// limit and before apply within the filtered list; next_before is the last withdrawal of the
// filtered page, so following it skips the withdrawals the filter leaves out.
func TestWithdrawalsFilterPagesWithItsOwnCursors(t *testing.T) {
	a := newTestAPI(t)
	account := fundedAccount(t, a.pool, 1000)
	var newestFirst []any
	for range 5 {
		newestFirst = slices.Insert(newestFirst, 0, any(a.withdrawalIn(t, account, withdrawals.StatusExecuted)))
		a.withdrawalIn(t, account, withdrawals.StatusHeld) // between them
	}
	seen, cursors := walkWithdrawals(t, a, account.ID, "status=executed&limit=2")
	if !slices.Equal(seen, newestFirst) {
		t.Errorf("seen = %v\nwant   %v", seen, newestFirst)
	}
	if want := []any{newestFirst[1], newestFirst[3], nil}; !slices.Equal(cursors, want) {
		t.Errorf("cursors = %v, want %v", cursors, want)
	}
}

func TestWithdrawalsInvalidQueryIs400(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	tests := []struct{ query, path, message string }{
		{"limit=0", "limit", "Too small: expected number to be >=1"},
		{"limit=201", "limit", "Too big: expected number to be <=200"},
		{"limit=lots", "limit", "Invalid input: expected number, received NaN"},
		{"before=" + ids.New[ids.Deposit]().TypeID(), "before", "must be a wd_ ID"},
		{"before=", "before", "must be a wd_ ID"},
		{"include=everything", "include", `Invalid input: expected "history"`},
		{"status=nope", "status", withdrawalStatusMessage},
		{"status=", "status", withdrawalStatusMessage},
		{"status=HELD", "status", withdrawalStatusMessage},
		{"status=held&status=failed", "status", withdrawalStatusMessage},
	}
	for _, tt := range tests {
		r := a.get(t, withdrawalsPath(account.ID, tt.query))
		want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
			`"errors":[{"path":"` + tt.path + `","message":` + strconv.Quote(tt.message) + `}]}`
		if r.status != 400 || r.body != want {
			t.Errorf("%s: got %d %s\nwant %s", tt.query, r.status, r.body, want)
		}
	}
}

// Every problem at once, in the contract's parameter order (decision 68): byte for byte Node's
// answer (withdrawals.test.ts, "reports every problem at once, in the contract's order").
func TestWithdrawalsReportsEveryProblemInTheContractsOrder(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, "/accounts/nope/withdrawals?status=nope&include=x&before=nope&limit=0")
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,"errors":[` +
		`{"path":"account_id","message":"must be a acct_ ID"},` +
		`{"path":"limit","message":"Too small: expected number to be >=1"},` +
		`{"path":"before","message":"must be a wd_ ID"},` +
		`{"path":"include","message":"Invalid input: expected \"history\""},` +
		`{"path":"status","message":` + strconv.Quote(withdrawalStatusMessage) + `}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}

func TestWithdrawalsIgnoresUnknownQueryParameters(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	if r := a.get(t, withdrawalsPath(account.ID, "sort=asc&network_transaction_id=x")); r.status != 200 {
		t.Errorf("got %d %s", r.status, r.body)
	}
}
