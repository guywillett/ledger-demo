package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/api"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The HTTP behaviour around the endpoints: request IDs, the access log, unknown routes, and how
// paths and methods are matched, each checked against what Node's API (Fastify) actually answers.

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestReusesAWellFormedRequestIDAndEchoesIt(t *testing.T) {
	a := newTestAPI(t)
	_, r := a.do(t, http.MethodGet, "/nope", http.Header{"X-Request-Id": {"provider-req.42"}})
	if got := r.header.Get("X-Request-Id"); got != "provider-req.42" {
		t.Errorf("x-request-id = %q", got)
	}
}

// Too long, or characters outside the allowed set (a header can't hold a newline, but a tab).
func TestReplacesABadRequestID(t *testing.T) {
	a := newTestAPI(t)
	for _, bad := range [][]string{
		{strings.Repeat("x", 65)}, {"has spaces"}, {"has\ttab"}, {"drop;table"},
		{"one", "two"}, // sent twice: Node joins them as "one, two", which the pattern refuses
	} {
		_, r := a.do(t, http.MethodGet, "/nope", http.Header{"X-Request-Id": bad})
		if got := r.header.Get("X-Request-Id"); !uuidPattern.MatchString(got) {
			t.Errorf("%q: x-request-id = %q, want a new UUID", bad, got)
		}
	}
}

// One access line per request, as Node's AccessLog, with the request's ID.
func TestLogsOneAccessLinePerRequest(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	a.do(t, http.MethodGet, transactionsPath(account.ID, "iban=FR7612345000010000000000193&limit=1"),
		http.Header{"X-Request-Id": {"req-1"}})

	lines := a.logLines(t)
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want 1: %v", len(lines), lines)
	}
	line := lines[0]
	want := map[string]any{
		"level":       "info",
		"msg":         "request completed",
		"service":     "api",
		"request_id":  "req-1",
		"method":      "GET",
		"path":        transactionsPath(account.ID, ""), // without the query string
		"status_code": float64(200),
	}
	for key, value := range want {
		if line[key] != value {
			t.Errorf("%s = %v, want %v", key, line[key], value)
		}
	}
	if _, ok := line["response_time_ms"].(float64); !ok {
		t.Errorf("response_time_ms = %v, want a number", line["response_time_ms"])
	}
	if strings.Contains(a.logs.String(), "FR76") {
		t.Error("the query string reached the log")
	}
}

const noSuchRoute = `{"type":"about:blank","title":"Not Found","status":404,"detail":"No such route."}`

// What Node's API answers 404 for, measured against it (its real server, not inject, which
// normalises paths first). Go's ServeMux would answer 405 (with an Allow header) for another
// method, and redirect (301) the unclean paths; NewHandler and onlyCleanPaths make both 404.
func TestAnswers404AsNodeDoes(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	balance := balancePath(account.ID)
	tests := []struct{ method, target string }{
		{http.MethodGet, "/nope?iban=FR7612345000010000000000193"},
		{http.MethodGet, "/accounts/" + account.ID.TypeID()},
		{http.MethodGet, strings.ToUpper("/accounts") + "/" + account.ID.TypeID() + "/balance"},
		{http.MethodPost, balance}, // a known path, another method
		{http.MethodDelete, transactionsPath(account.ID, "")},
		{http.MethodOptions, balance},
		{http.MethodGet, "/" + balance},                                        // //accounts/…
		{http.MethodGet, "/accounts//" + account.ID.TypeID() + "/balance"},     // an empty segment
		{http.MethodGet, balance + "/"},                                        // a trailing slash
		{http.MethodGet, "/accounts/" + account.ID.TypeID() + "/./balance"},    // a dot segment
		{http.MethodGet, "/accounts/x/../" + account.ID.TypeID() + "/balance"}, // a dot-dot segment
	}
	for _, tt := range tests {
		_, r := a.do(t, tt.method, tt.target, nil)
		if r.status != 404 || r.body != noSuchRoute {
			t.Errorf("%s %s: got %d %s", tt.method, tt.target, r.status, r.body)
		}
		if allow, location := r.header.Get("Allow"), r.header.Get("Location"); allow != "" || location != "" {
			t.Errorf("%s %s: Allow %q, Location %q; want neither", tt.method, tt.target, allow, location)
		}
		if strings.Contains(r.body, "FR76") || strings.Contains(r.body, "nope") {
			t.Errorf("%s %s: the body echoes the URL", tt.method, tt.target)
		}
	}
}

// %-escapes in the path are decoded before matching, as in Node: %61 is "a", so this is the same
// request as the plain path. Not contract-checked: the validator matches the raw, escaped path
// against the contract's templates, and bal%61nce isn't balance to it.
func TestDecodesEscapesInThePath(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	escaped := "/accounts/%61" + strings.TrimPrefix(account.ID.TypeID(), "a") + "/bal%61nce"
	_, got := a.do(t, http.MethodGet, escaped, nil)
	plain := a.get(t, balancePath(account.ID))
	if got.status != 200 || got.body != plain.body {
		t.Errorf("escaped path = %d %s, want the plain path's %d %s", got.status, got.body, plain.status, plain.body)
	}
}

// An escaped / or .. belongs to the path value, as in Node: the balance route with an invalid
// account_id (400), not an unclean path (404). Measured against Node's server.
func TestEscapedSlashesAndDotsArePartOfThePathValue(t *testing.T) {
	a := newTestAPI(t)
	for _, target := range []string{"/accounts/acct_x%2F/balance", "/accounts/%2E%2E/balance", "/accounts/acct_x%2E/balance"} {
		_, r := a.do(t, http.MethodGet, target, nil)
		errs, _ := r.json(t)["errors"].([]any)
		if r.status != 400 || len(errs) != 1 || errs[0].(map[string]any)["path"] != "account_id" {
			t.Errorf("%s: got %d %s, want 400 at account_id", target, r.status, r.body)
		}
	}
}

// HEAD on a GET route: 200, the headers (Content-Length included), no body, as Node's real server.
// Through a real server (httptest.NewServer): the recorder keeps a body net/http wouldn't send.
func TestHeadAnswersTheHeadersOnly(t *testing.T) {
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	server := httptest.NewServer(a.handler)
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, server.URL+balancePath(account.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	want := len(`{"pending_in":"0","available":"0","held":"0","currency":"EUR"}`)
	if resp.StatusCode != 200 || len(body) != 0 || resp.ContentLength != int64(want) ||
		resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("HEAD = %d, Content-Length %d, Content-Type %q, body %q; want 200, %d, JSON, none",
			resp.StatusCode, resp.ContentLength, resp.Header.Get("Content-Type"), body, want)
	}
}

// An unexpected error (here, the database is unreachable): a 500 problem, an "unexpected error"
// line, then the access line with status 500, as Node logs it.
func TestAnswersAnUnexpectedErrorWith500(t *testing.T) {
	var logs bytes.Buffer
	logger, err := logging.New(&logs, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	// A pool that can't connect: Connect is lazy, so the first query fails.
	unreachable, err := db.Connect(context.Background(), "postgres://ledger:ledger@127.0.0.1:1/none_test?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer unreachable.Close()
	a := testAPI{handler: api.NewHandler(unreachable, logger), logs: &logs}
	account := testdb.NewAccount(t, testdb.Pool(t))

	// Not contract-checked: the contract documents no 500 for the GET operations (only the POSTs
	// have one), a gap to raise (decision 41), not to fix here.
	_, r := a.do(t, http.MethodGet, balancePath(account.ID), nil)
	want := `{"type":"about:blank","title":"Internal Server Error","status":500}`
	if r.status != 500 || r.body != want {
		t.Errorf("got %d %s", r.status, r.body)
	}

	var messages []any
	for _, line := range a.logLines(t) {
		messages = append(messages, line["msg"])
	}
	if len(messages) != 2 || messages[0] != "unexpected error" || messages[1] != "request completed" {
		t.Errorf("log messages = %v, want [unexpected error, request completed]", messages)
	}
}
