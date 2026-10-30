package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// The panic recovery, with handlers that panic: an internal test (package api, not api_test), so
// it can wrap its own handler in the middleware.

// serve runs one request through the middleware; aborted is the panic value that left ServeHTTP,
// if any (net/http itself recovers it in a real server).
func serve(t *testing.T, handler http.Handler) (rec *httptest.ResponseRecorder, lines []map[string]any, aborted any) {
	t.Helper()
	var logs bytes.Buffer
	logger, err := logging.New(&logs, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil)
	func() {
		defer func() { aborted = recover() }()
		withRequestID(logger, withAccessLog(handler)).ServeHTTP(rec, req)
	}()
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON log line: %q", line)
		}
		lines = append(lines, m)
	}
	return rec, lines, aborted
}

// A panic before the response starts: a 500 problem, as Fastify answers a thrown error.
func TestRecoversAPanicWith500(t *testing.T) {
	rec, lines, aborted := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	if aborted != nil {
		t.Errorf("panicked through the middleware: %v", aborted)
	}
	if want := `{"type":"about:blank","title":"Internal Server Error","status":500}`; rec.Code != 500 || rec.Body.String() != want {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if len(lines) != 2 || lines[0]["msg"] != "unexpected error" || lines[0]["level"] != "error" ||
		lines[1]["msg"] != "request completed" || lines[1]["status_code"] != float64(500) {
		t.Errorf("log lines = %v", lines)
	}
	if err, _ := lines[0]["err"].(map[string]any); err["message"] != "panic: boom" {
		t.Errorf("err = %v", lines[0]["err"])
	}
}

// A panic after the response has started: nothing more can be sent, so the access line itself is
// "request errored", at error level, with the status that went out; then http.ErrAbortHandler, so
// net/http cuts the connection rather than end the response as if it were complete.
func TestReportsAPanicAfterTheResponseStartedAndAborts(t *testing.T) {
	rec, lines, aborted := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("half-way")
	}))
	if aborted != http.ErrAbortHandler { //nolint:errorlint // the exact panic value net/http looks for
		t.Errorf("panic value = %v, want http.ErrAbortHandler", aborted)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want the 200 already sent", rec.Code)
	}
	if len(lines) != 1 || lines[0]["msg"] != "request errored" || lines[0]["level"] != "error" ||
		lines[0]["status_code"] != float64(200) {
		t.Errorf("log lines = %v", lines)
	}
}

// The request's context has a deadline: a handler waiting on it (as pgx does on a query) is
// released when it passes, instead of running on.
func TestEveryRequestHasADeadline(t *testing.T) {
	var err error
	handler := withDeadline(20*time.Millisecond, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		err = r.Context().Err()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("context error = %v, want DeadlineExceeded", err)
	}
}

func TestJSNumber(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"2", 2}, {" 2 ", 2}, {"+2", 2}, {"-2", -2}, {"", 0}, {"  ", 0},
		{"0x10", 16}, {"0X10", 16}, {"0o20", 16}, {"0b10000", 16},
		{"1e2", 100}, {".5e1", 5}, {"2.", 2}, {"2.50", 2.5},
		// Beyond 64 bits: still a (finite) number, as in JavaScript, so it reads as too big.
		{"0xFFFFFFFFFFFFFFFFFFFF", math.MaxFloat64},
		{"\u00a02\ufeff", 2}, // JavaScript's whitespace, not decision 56's separators
	}
	for _, tt := range tests {
		if got := jsNumber(tt.in); got != tt.want {
			t.Errorf("jsNumber(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	for _, nan := range []string{"lots", "2;x", "0x", "-0x10", "0x1_0", "1_000", "1e", "e2", ".", "+", "inf", "NaN", "0x1p4", "2\u0085"} {
		if got := jsNumber(nan); got == got { // NaN is the only value not equal to itself
			t.Errorf("jsNumber(%q) = %v, want NaN", nan, got)
		}
	}
}
