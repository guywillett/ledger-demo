package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/api"
	"github.com/guywillett/ledger-demo/go/internal/contracttest"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// testAPI is the API's handler on the test database, with its log lines captured.
type testAPI struct {
	handler http.Handler
	pool    *pgxpool.Pool
	logs    *bytes.Buffer
}

func newTestAPI(t *testing.T) testAPI {
	t.Helper()
	var logs bytes.Buffer
	logger, err := logging.New(&logs, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.Pool(t)
	return testAPI{handler: api.NewHandler(pool, logger), pool: pool, logs: &logs}
}

// response is what a request got back.
type response struct {
	status int
	header http.Header
	body   string
}

// json decodes the body.
func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("body is not a JSON object: %s", r.body)
	}
	return v
}

// do sends a request through the handler with net/http/httptest: NewRequest builds an incoming
// request, as the server would have parsed it, and NewRecorder is a ResponseWriter that keeps
// what's written. No network: the counterpart of Fastify's inject.
func (a testAPI) do(t *testing.T, method, target string, header http.Header) (*http.Request, response) {
	t.Helper()
	// t.Context() is cancelled when the test ends, as the server cancels a request's context when
	// the client goes away.
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for name, values := range header {
		req.Header[name] = values
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return req, response{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

// get sends a GET and checks the response against the contract, as Node's getJson does.
func (a testAPI) get(t *testing.T, target string) response {
	t.Helper()
	req, r := a.do(t, http.MethodGet, target, nil)
	resp := &http.Response{StatusCode: r.status, Header: r.header, Body: io.NopCloser(strings.NewReader(r.body))}
	contracttest.Check(t, req, resp)
	return r
}

// logLines parses the captured log lines.
func (a testAPI) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(a.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON log line: %q", line)
		}
		lines = append(lines, m)
	}
	return lines
}

// process takes a deposit through the lifecycle to its decision, posted: what the deposit-worker
// does (none needed here), with the ledger and the compliance transitions directly.
func process(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID, decision deposits.Transition, reason string) {
	t.Helper()
	ctx := context.Background()
	err := db.InTx(ctx, pool, func(tx db.Tx) error {
		if _, err := ledger.Book(ctx, tx, deposit); err != nil {
			return err
		}
		if _, _, err := deposits.TransitionStatus(ctx, tx, deposit, deposits.StartReview, ""); err != nil {
			return err
		}
		if _, _, err := deposits.TransitionStatus(ctx, tx, deposit, decision, reason); err != nil {
			return err
		}
		_, err := ledger.PostDecision(ctx, tx, deposit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func book(t *testing.T, pool *pgxpool.Pool, deposit ids.DepositID) {
	t.Helper()
	err := db.InTx(context.Background(), pool, func(tx db.Tx) error {
		_, err := ledger.Book(context.Background(), tx, deposit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
