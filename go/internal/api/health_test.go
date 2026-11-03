package api_test

// GET /health (slice 4 decision 72): the cases of node/test/api/health.test.ts, through the full
// handler (NewHandler), every response checked against the contract.
//
// How the database fails here: a real pool pointed at a database that isn't there, as the 500 test
// in http_test.go does, rather than a fake Querier (NewHandler takes a *pgxpool.Pool, and the
// tests go through it). Two ways: nothing listening (the connection is refused at once), and a
// listener that accepts the connection and never answers (only healthTimeout ends the check).

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/api"
	"github.com/guywillett/ledger-demo/go/internal/contracttest"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/logging"
)

const notReadyBody = `{"type":"urn:ledger-demo:problem:not-ready","title":"Not ready","status":503,` +
	`"detail":"The database did not answer."}`

// getChecked sends a GET through the handler with these headers and checks the response against
// the contract.
func (a testAPI) getChecked(t *testing.T, target string, header http.Header) response {
	t.Helper()
	req, r := a.do(t, http.MethodGet, target, header)
	contracttest.Check(t, req, &http.Response{
		StatusCode: r.status, Header: r.header, Body: io.NopCloser(strings.NewReader(r.body)),
	})
	return r
}

func TestHealthAnswers200StatusOKWhenTheDatabaseAnswersLoggingNothing(t *testing.T) {
	a := newTestAPI(t)
	r := a.getChecked(t, "/health", http.Header{"X-Request-Id": {"probe-1"}})
	if r.status != 200 || r.body != `{"status":"ok"}` {
		t.Errorf("got %d %s, want 200 {\"status\":\"ok\"}", r.status, r.body)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if id := r.header.Get("X-Request-Id"); id != "probe-1" {
		t.Errorf("X-Request-Id = %q, want the one sent", id)
	}
	// No access line for a successful probe (nor anything else).
	if lines := a.logLines(t); len(lines) != 0 {
		t.Errorf("log lines = %v, want none", lines)
	}
}

// HEAD: 200, the headers (Content-Length included), no body, and no log line. Through a real server
// (httptest.NewServer), as TestHeadAnswersTheHeadersOnly: the recorder keeps a body net/http
// wouldn't send.
func TestHealthAnswersHEADWithTheHeadersOnlyLoggingNothing(t *testing.T) {
	a := newTestAPI(t)
	server := httptest.NewServer(a.handler)
	defer server.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(body) != 0 || resp.ContentLength != int64(len(`{"status":"ok"}`)) ||
		resp.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
		resp.Header.Get("X-Request-Id") == "" {
		t.Errorf("HEAD = %d, Content-Length %d, Content-Type %q, X-Request-Id %q, body %q",
			resp.StatusCode, resp.ContentLength, resp.Header.Get("Content-Type"),
			resp.Header.Get("X-Request-Id"), body)
	}
	if lines := a.logLines(t); len(lines) != 0 {
		t.Errorf("log lines = %v, want none", lines)
	}
}

func TestHealthAnswers503WhenTheDatabaseRefusesConnections(t *testing.T) {
	// Nothing listens on port 1: the connection fails at once.
	expectNotReady(t, "postgres://ledger:ledger@127.0.0.1:1/none_test")
}

func TestHealthAnswers503WithinTheTimeoutWhenTheDatabaseHangs(t *testing.T) {
	expectNotReady(t, silentDatabase(t))
}

// A probe that gives up mid-check: the request's context is cancelled while SELECT 1 waits on a
// database that hangs. Not the database's fault, so no error line, and the handler returns at once
// rather than at healthTimeout. Go only: Node's handler doesn't notice the client leaving. What is
// written (nothing) and the access log (none: the recorder's default 200 counts as a success for
// quietOnSuccess) aren't asserted: nobody receives the answer.
func TestHealthLogsNothingWhenTheClientGivesUp(t *testing.T) {
	var logs bytes.Buffer
	logger, err := logging.New(&logs, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), silentDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a := testAPI{handler: api.NewHandler(pool, logger), logs: &logs}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel) // the client leaves after 100 ms
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/health", nil)
	started := time.Now()
	a.handler.ServeHTTP(httptest.NewRecorder(), req)

	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Errorf("returned after %v, want soon after the cancel (not at the 1 s timeout)", elapsed)
	}
	for _, line := range a.logLines(t) {
		if line["level"] == "error" {
			t.Errorf("error line %v, want none: the client left, the database didn't fail", line)
		}
	}
}

// expectNotReady checks the answer of an API whose database is at databaseURL and doesn't answer:
// the 503 problem, in time, one error line with the cause, and the access line (a failure is
// logged).
func expectNotReady(t *testing.T, databaseURL string) {
	t.Helper()
	var logs bytes.Buffer
	logger, err := logging.New(&logs, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	// db.Connect is lazy: nothing is dialled until the first query.
	pool, err := db.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a := testAPI{handler: api.NewHandler(pool, logger), logs: &logs}

	started := time.Now()
	r := a.getChecked(t, "/health", nil)
	elapsed := time.Since(started)

	if r.status != 503 || r.body != notReadyBody {
		t.Errorf("got %d %s\nwant 503 %s", r.status, r.body, notReadyBody)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/problem+json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if r.header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id")
	}
	// The timeout is 1 s (healthTimeout, Node's HEALTH_TIMEOUT_MS); a second's margin, as Node's test.
	if elapsed > 2*time.Second {
		t.Errorf("answered after %v, want within the 1 s timeout (and a margin)", elapsed)
	}

	var errorLines, accessLines []map[string]any
	for _, line := range a.logLines(t) {
		if line["level"] == "error" {
			errorLines = append(errorLines, line)
		}
		if line["msg"] == "request completed" {
			accessLines = append(accessLines, line)
		}
	}
	if len(errorLines) != 1 || errorLines[0]["msg"] != "health check failed" || errorLines[0]["err"] == nil {
		t.Errorf("error lines = %v, want one \"health check failed\" with its err", errorLines)
	}
	if len(accessLines) != 1 || accessLines[0]["path"] != "/health" ||
		accessLines[0]["status_code"] != float64(503) {
		t.Errorf("access lines = %v, want one for /health with status 503", accessLines)
	}
}

// silentDatabase listens on a free local port, accepts connections and never answers (no reply to
// the connection's startup message): a database that hangs. Returns its URL; everything is closed
// when the test ends.
func silentDatabase(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // the listener was closed
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return "postgres://ledger:ledger@" + listener.Addr().String() + "/none_test"
}
