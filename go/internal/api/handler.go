// Package api is the HTTP API: the client API (accounts and withdrawals) and the provider
// webhooks (webhook_*.go). Node: node/src/api/. Routing is net/http's ServeMux (method and path
// patterns, Go 1.22+), and everything around the handlers (request IDs, the access log, panics,
// clean paths, a deadline per request) is middleware (middleware.go).
package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// NewHandler returns the API as one http.Handler, for the server (cmd/api) and for tests, which
// call its ServeHTTP directly through net/http/httptest (no network, as Fastify's inject).
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) http.Handler {
	return newHandler(pool, logger, newWebhookRoutes(pool))
}

// newHandler is NewHandler with the webhook routes given, so a test can replace how they record.
func newHandler(pool *pgxpool.Pool, logger *slog.Logger, webhooks webhookRoutes) http.Handler {
	mux := http.NewServeMux()
	accountsAPI := accountRoutes{db: pool}
	depositsAPI := depositRoutes{db: pool}
	withdrawalsAPI := withdrawalRoutes{pool: pool}
	transactionsAPI := transactionRoutes{db: pool}
	health := healthRoutes{db: pool}
	// "GET" in a pattern matches HEAD too, as Fastify's GET routes do: net/http then sends the
	// headers (Content-Length included) without the body.
	mux.Handle("GET /accounts/{account_id}", handle(accountsAPI.account))
	mux.Handle("GET /accounts/{account_id}/balance", handle(accountsAPI.balance))
	mux.Handle("GET /accounts/{account_id}/deposits", handle(depositsAPI.list))
	mux.Handle("GET /accounts/{account_id}/deposits/{deposit_id}", handle(depositsAPI.get))
	mux.Handle("POST /accounts/{account_id}/withdrawals", handle(withdrawalsAPI.create))
	mux.Handle("GET /accounts/{account_id}/withdrawals", handle(withdrawalsAPI.list))
	mux.Handle("GET /accounts/{account_id}/withdrawals/{withdrawal_id}", handle(withdrawalsAPI.get))
	mux.Handle("GET /accounts/{account_id}/transactions", handle(transactionsAPI.list))
	mux.Handle("POST /provider/incoming-payments", handle(webhooks.incomingPayment))
	mux.Handle("POST /provider/transfer-updates", handle(webhooks.transferUpdate))
	mux.Handle("GET /health", quietOnSuccess(handle(health.health)))
	// Every other request, whatever its method or path: the most specific matching pattern wins, so
	// this matches only what nothing else does. Registering it also keeps ServeMux from answering
	// 405 for a known path with another method: something always matches.
	mux.HandleFunc("/", notFound)

	return traced(withRequestID(logger, withAccessLog(withDeadline(requestTimeout, onlyCleanPaths(mux)))))
}

// traced gives every request a server span (otelhttp), outermost, so the span covers the
// middlewares too and their log lines carry its IDs (withRequestID). Named by the method until
// handle names it by its route; a request no route matches keeps that name, as in Node. A no-op
// until telemetry.Setup registers a provider (otelhttp uses the global one).
//
// With no propagators, so an incoming traceparent is never read: the API's callers are outside the
// system, and every request starts a new trace (slice 5 decision 84). An empty composite extracts
// nothing.
func traced(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "",
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method }),
	)
}

// handlerFunc is a handler that can fail: an error it returns is unexpected (a client error is
// answered by the handler itself, as a problem), so handle answers 500.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle adapts a handlerFunc to http.Handler: an error is logged as "unexpected error" and
// answered with a 500 problem, as Node's error handler does for anything thrown.
func handle(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nameByRoute(r)
		if err := h(w, r); err != nil {
			loggerFrom(r.Context()).Error("unexpected error", "err", err)
			writeProblem(w, httpProblem(http.StatusInternalServerError, ""))
		}
	})
}

// nameByRoute names the request's server span after its route's template, never the raw path (GET
// /accounts/{account_id}/deposits), and gives the template as http.route to the span and, through
// otelhttp's labeler, to the duration metric. Every route goes through handle, so here: ServeMux
// sets Pattern on the request it hands the route, which otelhttp, outside the middlewares' copies of
// the request, never sees. The method is the request's (HEAD on a GET route is HEAD, as in Node).
func nameByRoute(r *http.Request) {
	_, route, found := strings.Cut(r.Pattern, " ")
	if !found {
		return
	}
	span := trace.SpanFromContext(r.Context())
	span.SetName(r.Method + " " + route)
	span.SetAttributes(semconv.HTTPRoute(route))
	if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
		labeler.Add(semconv.HTTPRoute(route))
	}
}

// listResponse is every list's body, one shape for all (slice 4 decision 66): Data, the page's
// items ([] when empty: make it non-nil), and NextBefore, the next page's cursor (null on the last
// page). Each list names its instantiation once as a type alias (depositListResponse =
// listResponse[depositResponse, ids.DepositID]), so the pairing of item and cursor types is
// written in one place, and the instantiated type is as specific as a hand-written struct: Data is
// a []depositResponse, not a []any.
type listResponse[T any, C any] struct {
	Data       []T `json:"data"`
	NextBefore *C  `json:"next_before"`
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
