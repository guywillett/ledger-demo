// Package api is the HTTP API: the client API (accounts, later withdrawals) and, from step 4b, the
// provider webhooks. Node: node/src/api/. Routing is net/http's ServeMux (method and path
// patterns, Go 1.22+), and everything around the handlers (request IDs, the access log, panics,
// clean paths, a deadline per request) is middleware (middleware.go).
package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewHandler returns the API as one http.Handler, for the server (cmd/api) and for tests, which
// call its ServeHTTP directly through net/http/httptest (no network, as Fastify's inject).
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	accounts := accountRoutes{db: pool}
	// "GET" in a pattern matches HEAD too, as Fastify's GET routes do: net/http then sends the
	// headers (Content-Length included) without the body.
	mux.Handle("GET /accounts/{account_id}/balance", handle(accounts.balance))
	mux.Handle("GET /accounts/{account_id}/transactions", handle(accounts.transactions))
	// Every other request, whatever its method or path: the most specific matching pattern wins, so
	// this matches only what nothing else does. Registering it also keeps ServeMux from answering
	// 405 for a known path with another method: something always matches.
	mux.HandleFunc("/", notFound)

	return withRequestID(logger, withAccessLog(withDeadline(requestTimeout, onlyCleanPaths(mux))))
}

// handlerFunc is a handler that can fail: an error it returns is unexpected (a client error is
// answered by the handler itself, as a problem), so handle answers 500.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle adapts a handlerFunc to http.Handler: an error is logged as "unexpected error" and
// answered with a 500 problem, as Node's error handler does for anything thrown.
func handle(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			loggerFrom(r.Context()).Error("unexpected error", "err", err)
			writeProblem(w, httpProblem(http.StatusInternalServerError, ""))
		}
	})
}

// writeJSON answers with v as JSON, byte for byte as Node's JSON.stringify writes it: no HTML
// escaping (encoding/json writes <, > and & as <… by default, safe inside HTML but not what
// Node writes) and no trailing newline (json.Encoder adds one).
func writeJSON(w http.ResponseWriter, status int, contentType string, v any) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		// Only a value that can't be JSON (a channel, a func): a bug in this package.
		panic(err)
	}
	out := bytes.TrimSuffix(body.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", contentType)
	// Set, as Node does: net/http works out a Content-Length only for a body that fits its 2 KB
	// buffer, and sends a larger one chunked (Transfer-Encoding: chunked) instead.
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(status)
	_, _ = w.Write(out) // a write error means the client left
}
