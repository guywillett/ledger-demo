package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// GET /accounts/{account_id} and /balance against the test database: the cases of
// node/test/api/accounts.test.ts. Every response is checked against the contract (get). The
// deposits are in deposits_test.go.

func accountPath(account ids.AccountID) string {
	return "/accounts/" + account.TypeID()
}

// createdAt is the account's created_at as the API writes it (Node's toISOString: milliseconds,
// truncated, in UTC), read from the database.
func createdAt(t *testing.T, pool *pgxpool.Pool, account ids.AccountID) string {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(), `SELECT created_at FROM accounts WHERE id = $1`, account).
		Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Byte for byte Node's body, in this order: id, name, iban, created_at.
func TestAccountReturnsASeededAccount(t *testing.T) {
	a := newTestAPI(t)
	alice, err := ids.FromTypeID[ids.Account]("acct_01m564xr00fx7rjvqryn5dzwxx")
	if err != nil {
		t.Fatal(err)
	}
	r := a.get(t, accountPath(alice))
	want := `{"id":"acct_01m564xr00fx7rjvqryn5dzwxx","name":"alice","iban":"FR7612345000010000000000193",` +
		`"created_at":"` + createdAt(t, a.pool, alice) + `"}`
	if r.status != 200 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestAccountReturnsATestAccount(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	// testdb.InsertAccount names it after its ID: "go-test-" and the ID's suffix, upper case.
	name := "go-test-" + strings.ToUpper(strings.TrimPrefix(account.ID.TypeID(), "acct_"))
	r := a.get(t, accountPath(account.ID))
	want := `{"id":"` + account.ID.TypeID() + `","name":"` + name + `","iban":"` + account.IBAN + `",` +
		`"created_at":"` + createdAt(t, a.pool, account.ID) + `"}`
	if r.status != 200 || r.body != want {
		t.Errorf("got %d %s\nwant    %s", r.status, r.body, want)
	}
}

func TestAccountUnknownIs404(t *testing.T) {
	a := newTestAPI(t)
	r := a.get(t, accountPath(ids.New[ids.Account]()))
	want := `{"type":"urn:ledger-demo:problem:account-not-found","title":"Account not found","status":404}`
	if r.status != 404 || r.body != want {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

func TestAccountInvalidIDIs400(t *testing.T) {
	a := newTestAPI(t)
	for _, id := range []string{"acct_garbage", ids.New[ids.Deposit]().TypeID(), "alice"} {
		r := a.get(t, "/accounts/"+id)
		want := `{"type":"urn:ledger-demo:problem:invalid-request","title":"Invalid request","status":400,` +
			`"errors":[{"path":"account_id","message":"must be a acct_ ID"}]}`
		if r.status != 400 || r.body != want {
			t.Errorf("%s: got %d %s", id, r.status, r.body)
		}
	}
}

// HEAD: 200, the GET's headers (Content-Length included), no body. Through a real server, as
// TestHeadAnswersTheHeadersOnly.
func TestAccountAnswersHEADWithTheHeadersOnly(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	get := a.get(t, accountPath(account.ID))
	server := httptest.NewServer(a.handler)
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+accountPath(account.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(body) != 0 || resp.ContentLength != int64(len(get.body)) ||
		resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("HEAD = %d, Content-Length %d (GET's body is %d), Content-Type %q, body %q",
			resp.StatusCode, resp.ContentLength, len(get.body), resp.Header.Get("Content-Type"), body)
	}
}

func balancePath(account ids.AccountID) string {
	return "/accounts/" + account.TypeID() + "/balance"
}

func TestBalanceStartsAtZero(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	r := a.get(t, balancePath(account.ID))
	if r.status != 200 {
		t.Fatalf("status = %d", r.status)
	}
	// Byte for byte Node's body: account_id first (slice 4, step 2).
	if want := `{"account_id":"` + account.ID.TypeID() + `","pending_in":"0","available":"0","held":"0","currency":"EUR"}`; r.body != want {
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
