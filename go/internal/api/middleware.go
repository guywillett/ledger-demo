package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Middleware: each is a function from one http.Handler to another, which does its part and calls
// the next. Wrapped around the router, they run outside-in on the way in and inside-out on the way
// out: the same idea as the slog handler wrapper in package logging, and the Go counterpart of
// Fastify's hooks and logController.

// loggerKey is the context key for the request's logger. A type of its own, unexported, so no
// other package can read or overwrite the value by accident: context keys are compared by type and
// value, and nothing outside this package can make a loggerKey.
type loggerKey struct{}

// loggerFrom returns the request's logger (with its request_id), put there by withRequestID.
func loggerFrom(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default() // only outside withRequestID, which every request goes through
}

// An incoming x-request-id is reused only if it is short and plain: it ends up in every log line
// for the request. Anything else is ignored (not rejected) and a new ID generated. Node's pattern.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// requestIDFrom picks the request's ID: the incoming header if it's one value matching the
// pattern, or a random UUID, as Node's crypto.randomUUID(). A header sent twice is two values in
// Go; Node joins them with ", ", which the pattern refuses, so both generate a new one.
func requestIDFrom(values []string) string {
	if len(values) == 1 && requestIDPattern.MatchString(values[0]) {
		return values[0]
	}
	return uuid.NewString()
}

// withRequestID gives every request an ID: echoed in the x-request-id response header, and on
// every log line for the request through a logger carrying request_id, put in the request's
// context (context.WithValue returns a new context with the value added; r.WithContext a copy of
// the request carrying it).
func withRequestID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDFrom(r.Header.Values("X-Request-Id"))
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), loggerKey{}, logger.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recorder wraps the ResponseWriter to remember the status written, for the access log: a plain
// ResponseWriter can be written to but not asked what was written. Embedding http.ResponseWriter
// promotes its Header method; WriteHeader and Write are replaced to record.
type recorder struct {
	http.ResponseWriter
	status  int
	started bool
}

func (rec *recorder) WriteHeader(status int) {
	if !rec.started {
		rec.status, rec.started = status, true
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if !rec.started {
		rec.WriteHeader(http.StatusOK) // as net/http does for a Write without WriteHeader
	}
	return rec.ResponseWriter.Write(b)
}

// Unwrap gives http.ResponseController the ResponseWriter underneath (to flush, set deadlines).
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// withAccessLog writes one line per request, as Node's AccessLog: method, the path without its
// query string (caller input could carry anything, an IBAN or a name), status_code and
// response_time_ms, as "request completed".
//
// It also recovers a panic in a handler, as Fastify catches a thrown error: an "unexpected error"
// line and a 500 problem, then the access line with status 500 (Node logs exactly that pair). Only
// if the response had already started, so no 500 can be sent, is the access line "request errored"
// at error level, Fastify's line for an error it couldn't answer; it then panics again with
// http.ErrAbortHandler, so net/http cuts the connection instead of ending a half-written response
// as if it were complete (a client would take a truncated chunked body for the whole one).
func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		logger := loggerFrom(r.Context())
		// Deferred, so it runs however the handler returns, panics included; recover() stops a
		// panic only inside a deferred function.
		defer func() {
			var errored error
			if p := recover(); p != nil {
				// net/http's own signal to abort the response quietly: let it through.
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				err := fmt.Errorf("panic: %v", p)
				if rec.started {
					errored = err
				} else {
					logger.Error("unexpected error", "err", err, "stack", string(debug.Stack()))
					writeProblem(rec, httpProblem(http.StatusInternalServerError, ""))
				}
			}
			fields := []any{
				"method", r.Method,
				"path", strings.SplitN(r.RequestURI, "?", 2)[0],
				"status_code", rec.status,
				"response_time_ms", math.Round(float64(time.Since(start).Microseconds()) / 1000),
			}
			if errored != nil {
				logger.Error("request errored", append(fields, "err", errored)...)
				panic(http.ErrAbortHandler)
			}
			logger.Info("request completed", fields...)
		}()
		next.ServeHTTP(rec, r)
	})
}

// onlyCleanPaths answers 404 for a path that isn't in its clean form (a //, a . or .. segment, a
// trailing slash), as Node's router does. Go's ServeMux would instead redirect it (301) to the
// clean path; this runs first, so it never sees one.
//
// The path as sent (EscapedPath), not decoded (Path): an escaped / or .. belongs to a path value,
// as in Node. /accounts/acct_x%2F/balance is the balance route with account_id "acct_x/" (a 400),
// not the unclean /accounts/acct_x//balance (a 404). ServeMux matches the escaped path too.
func onlyCleanPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.EscapedPath(); p != path.Clean(p) {
			notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestTimeout bounds each request: its context is cancelled after this long, and pgx then
// cancels the query it is running, so a request stuck on the database (a lock, a slow query)
// answers 500 instead of holding its connection for ever.
//
// Stricter than Node, which has no per-request deadline at all (slice 3 decision 58, an exception
// to decision 42, and a known gap for Node): the two differ only when the database hangs this long.
const requestTimeout = 30 * time.Second

// withDeadline gives each request's context a deadline (context.WithTimeout returns a context that
// is cancelled when the time is up, or when cancel is called; defer cancel releases its timer).
func withDeadline(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// notFound is the answer for an unknown route, and for a known path with another method (Node
// answers 404 for both; Go's ServeMux would answer 405). A fixed detail: echoing the requested URL
// would return caller input.
func notFound(w http.ResponseWriter, _ *http.Request) {
	writeProblem(w, httpProblem(http.StatusNotFound, "No such route."))
}
