package api_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// GET /accounts/{account_id}/transactions against the test database: the cases of
// node/test/api/transactions.test.ts. Every response is checked against the contract (get).

const transactionCursorMessage = "must be a dep_ or wd_ ID"

func transactionsPath(account ids.AccountID, query string) string {
	path := "/accounts/" + account.TypeID() + "/transactions"
	if query != "" {
		path += "?" + query
	}
	return path
}

// interleavedAccount: an account with its funding deposit, then a deposit, a withdrawal, a deposit
// and a withdrawal. Returns the account and its items' TypeIDs newest first.
func (a testAPI) interleavedAccount(t *testing.T) (testdb.Account, []any) {
	t.Helper()
	account := fundedAccount(t, a.pool, 1000)
	funding := listedIDs(t, a.get(t, depositsPath(account.ID, "")))
	created := []any{funding[0]}
	for range 2 {
		created = append(created, testdb.NewDeposit(t, a.pool, account, 100).TypeID())
		created = append(created, a.withdrawalIn(t, account, withdrawals.StatusHeld))
	}
	slices.Reverse(created)
	return account, created
}

func kindOf(id any) string {
	if s, _ := id.(string); strings.HasPrefix(s, "dep_") {
		return "deposit"
	}
	return "withdrawal"
}

func TestTransactionsListsDepositsAndWithdrawalsTogetherNewestFirst(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	r := a.get(t, transactionsPath(account.ID, ""))
	var got, want [][2]any
	for _, item := range objects(t, r.json(t), "data") {
		got = append(got, [2]any{item["type"], item["id"]})
	}
	for _, id := range newestFirst {
		want = append(want, [2]any{kindOf(id), id})
	}
	if r.status != 200 || !slices.Equal(got, want) || r.json(t)["next_before"] != nil {
		t.Errorf("got %d %v next %v\nwant %v, null", r.status, got, r.json(t)["next_before"], want)
	}
}

// Each item is exactly its own endpoint's body, and the list's body is those bodies in order.
func TestTransactionsGivesEachItemAsItsOwnEndpointDoesByteForByte(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	var bodies []string
	for _, id := range newestFirst {
		var own response
		if kindOf(id) == "deposit" {
			own = a.get(t, depositPath(account.ID, idFromDepositTypeID(t, id.(string)), ""))
		} else {
			own = a.get(t, withdrawalPath(account.ID, id.(string), ""))
		}
		bodies = append(bodies, own.body)
	}
	r := a.get(t, transactionsPath(account.ID, ""))
	if want := `{"data":[` + strings.Join(bodies, ",") + `],"next_before":null}`; r.body != want {
		t.Errorf("list %s\nwant %s", r.body, want)
	}
}

func idFromDepositTypeID(t *testing.T, typeID string) ids.DepositID {
	t.Helper()
	id, err := ids.FromTypeID[ids.Deposit](typeID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// walkTransactions reads every page with the query, following next_before: the IDs and cursors.
func walkTransactions(t *testing.T, a testAPI, account ids.AccountID, query string) (seen, cursors []any) {
	t.Helper()
	next := query
	for range 20 {
		p := a.get(t, transactionsPath(account, next))
		seen = append(seen, listedIDs(t, p)...)
		cursor := p.json(t)["next_before"]
		cursors = append(cursors, cursor)
		if cursor == nil {
			return seen, cursors
		}
		next = query + "&before=" + cursor.(string)
	}
	t.Fatal("more than 20 pages")
	return nil, nil
}

// limit 1: every page ends on one kind and the next begins on the other; cursors of both kinds.
func TestTransactionsPagesAcrossKindsWithACursorOfEitherKind(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	seen, cursors := walkTransactions(t, a, account.ID, "limit=1")
	if !slices.Equal(seen, newestFirst) {
		t.Errorf("seen %v\nwant %v", seen, newestFirst)
	}
	if want := append(slices.Clone(newestFirst[:len(newestFirst)-1]), nil); !slices.Equal(cursors, want) {
		t.Errorf("cursors %v\nwant    %v", cursors, want)
	}
}

// limit 2 over [W, D] [W, D] [D]: a page ending on a deposit, the next beginning on a withdrawal.
func TestTransactionsPagesInTwos(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	seen, cursors := walkTransactions(t, a, account.ID, "limit=2")
	if want := []any{newestFirst[1], newestFirst[3], nil}; !slices.Equal(seen, newestFirst) || !slices.Equal(cursors, want) {
		t.Errorf("seen %v cursors %v\nwant %v, %v", seen, cursors, newestFirst, want)
	}
}

// limit + 1 detection: exactly limit items is one page with no next; one fewer leaves one over.
func TestTransactionsEndsExactlyAtThePageBoundary(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	n := len(newestFirst)
	exact := a.get(t, transactionsPath(account.ID, "limit="+strconv.Itoa(n)))
	if len(listedIDs(t, exact)) != n || exact.json(t)["next_before"] != nil {
		t.Errorf("limit %d: %s", n, exact.body)
	}
	short := a.get(t, transactionsPath(account.ID, "limit="+strconv.Itoa(n-1)))
	if next := short.json(t)["next_before"]; next != newestFirst[n-2] {
		t.Errorf("limit %d: next %v, want %v", n-1, next, newestFirst[n-2])
	}
}

// A withdrawal needs funds, so the one-kind account the API can produce is deposits only.
func TestTransactionsListsAnAccountWithDepositsOnly(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	first := testdb.NewDeposit(t, a.pool, account, 100).TypeID()
	second := testdb.NewDeposit(t, a.pool, account, 100).TypeID()
	if got := listedIDs(t, a.get(t, transactionsPath(account.ID, ""))); !slices.Equal(got, []any{second, first}) {
		t.Errorf("got %v, want [second first]", got)
	}
}

func TestTransactionsEmptyListAndUnknownAccount(t *testing.T) {
	a := newTestAPI(t)
	if r := a.get(t, transactionsPath(testdb.NewAccount(t, a.pool).ID, "")); r.status != 200 || r.body != `{"data":[],"next_before":null}` {
		t.Errorf("empty: %d %s", r.status, r.body)
	}
	r := a.get(t, transactionsPath(ids.New[ids.Account](), ""))
	if r.status != 404 || r.json(t)["type"] != "urn:ledger-demo:problem:account-not-found" {
		t.Errorf("unknown account: %d %s", r.status, r.body)
	}
}

// Decision 66: a cursor is a position in time. Another account's newer item as before: every item
// of this account.
func TestTransactionsAcceptsABeforeOfAnotherAccount(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	later := testdb.NewDeposit(t, a.pool, testdb.NewAccount(t, a.pool), 100).TypeID()
	if got := listedIDs(t, a.get(t, transactionsPath(account.ID, "before="+later))); !slices.Equal(got, newestFirst) {
		t.Errorf("got %v\nwant %v", got, newestFirst)
	}
}

func TestTransactionsInvalidBeforeIs400(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	tests := []struct{ query, message string }{
		{"before=" + ids.New[ids.Account]().TypeID(), transactionCursorMessage},
		{"before=nope", transactionCursorMessage},
		{"before=", transactionCursorMessage},
		{"before=dep_x", transactionCursorMessage},
		{"before=" + strings.ToUpper(ids.New[ids.Deposit]().TypeID()), transactionCursorMessage},
		{"before=" + ids.New[ids.Deposit]().TypeID() + "&before=" + ids.New[ids.Withdrawal]().TypeID(),
			"Invalid input: expected string, received array"},
	}
	for _, tt := range tests {
		r := a.get(t, transactionsPath(account.ID, tt.query))
		want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
			`"errors":[{"path":"before","message":` + strconv.Quote(tt.message) + `}]}`
		if r.status != 400 || r.body != want {
			t.Errorf("%s: got %d %s\nwant %s", tt.query, r.status, r.body, want)
		}
	}
}

// Every problem at once, path then query (decision 68): byte for byte Node's answer.
func TestTransactionsReportsEveryProblemAtOnce(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, "/accounts/nope/transactions?before=nope&limit=0")
	want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,"errors":[` +
		`{"path":"account_id","message":"must be a acct_ ID"},` +
		`{"path":"limit","message":"Too small: expected number to be >=1"},` +
		`{"path":"before","message":"must be a dep_ or wd_ ID"}]}`
	if r.status != 400 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}

// No include and no filters here (decision 67): unknown parameters, ignored.
func TestTransactionsIgnoresIncludeAndTheFilters(t *testing.T) {
	a := newTestAPI(t)
	account, newestFirst := a.interleavedAccount(t)
	r := a.get(t, transactionsPath(account.ID, "include=history&status=held&include=nope"))
	if got := listedIDs(t, r); r.status != 200 || !slices.Equal(got, newestFirst) {
		t.Errorf("got %d %v\nwant %v", r.status, got, newestFirst)
	}
	if strings.Contains(r.body, `"history"`) {
		t.Errorf("a history in %s", r.body)
	}
}
