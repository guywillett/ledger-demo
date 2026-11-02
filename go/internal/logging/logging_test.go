package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

// lines parses each JSON line written to buf.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper() // failures are reported at the caller's line, not here
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func newLogger(t *testing.T, level string) (*bytes.Buffer, func() []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "deposit-worker", level)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	logger.Info("deposit processed", "deposit_id", "dep_01m5yes0f2ew7bgnwp2dx5wa61", "attempt", 1)
	return &buf, func() []map[string]any { return lines(t, &buf) }
}

// Shaped like Node's pino line: the same field names, "info" not "INFO", UTC milliseconds.
func TestPinoShape(t *testing.T) {
	_, read := newLogger(t, "info")
	got := read()
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	line := got[0]
	want := map[string]any{
		"level":          "info",
		"msg":            "deposit processed",
		"service":        "deposit-worker",
		"implementation": "go",
		"deposit_id":     "dep_01m5yes0f2ew7bgnwp2dx5wa61",
		"attempt":        float64(1), // encoding/json decodes every JSON number as float64
	}
	for key, value := range want {
		if line[key] != value {
			t.Errorf("%s = %#v, want %#v", key, line[key], value)
		}
	}
	written, _ := line["time"].(string)
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(written) {
		t.Errorf("time = %q, want UTC to the millisecond like 2026-10-27T10:34:00.403Z", written)
	}
	if len(line) != len(want)+1 {
		t.Errorf("line has extra fields: %v", line)
	}
}

func TestLevels(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "api", "trace")
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	ctx := context.Background()
	logger.Log(ctx, logging.LevelTrace, "a")
	logger.Debug("b")
	logger.Warn("c")
	logger.Error("d")
	logging.Fatal(ctx, logger, "e")
	var names []string
	for _, line := range lines(t, &buf) {
		names = append(names, line["level"].(string))
	}
	if got := strings.Join(names, " "); got != "trace debug warn error fatal" {
		t.Errorf("levels = %q, want pino's names", got)
	}
}

func TestMinimumLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := logging.New(&buf, "api", "warn")
	logger.Info("dropped")
	logger.Warn("kept")
	if got := lines(t, &buf); len(got) != 1 || got[0]["msg"] != "kept" {
		t.Errorf("lines = %v, want only the warn line", got)
	}
}

func TestSilent(t *testing.T) {
	buf, _ := newLogger(t, "silent")
	if buf.Len() != 0 {
		t.Errorf("silent wrote %q", buf.String())
	}
}

func TestUnknownLevel(t *testing.T) {
	if _, err := logging.New(&bytes.Buffer{}, "api", "verbose"); err == nil {
		t.Error("New(verbose) succeeded, want an error")
	}
}

// Errors become {type, message}, as pino's err serializer writes {type, message, stack}.
func TestErrors(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := logging.New(&buf, "api", "info")
	logger.Error("query failed", "err", errors.New("connection refused"))
	got, _ := lines(t, &buf)[0]["err"].(map[string]any)
	if got["message"] != "connection refused" || got["type"] != "*errors.errorString" {
		t.Errorf("err = %v, want type and message", got)
	}
}

// Amounts are digit strings, exact above 2^53, as in Node's logs.
func TestAmounts(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := logging.New(&buf, "api", "info")
	logger.Info("held", "amount_minor", money.Amount(9007199254740993))
	if !strings.Contains(buf.String(), `"amount_minor":"9007199254740993"`) {
		t.Errorf("line = %s, want the amount as a digit string", buf.String())
	}
}

// A caller's own "time", "level" or "msg" field is renamed to attr.*, whether it's passed in the
// log call (Handle) or with logger.With (WithAttrs), and whatever its type: a valid slog.Level or
// time.Time under those keys would otherwise replace the real one when the line is read, since
// slog writes duplicate keys and JSON readers keep the last.
func TestReservedKeysAreRenamed(t *testing.T) {
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		log  func(logger *slog.Logger)
	}{
		{"in the log call", func(logger *slog.Logger) {
			logger.Warn("x", "level", slog.LevelError, "msg", "hi", "time", when)
		}},
		{"with logger.With", func(logger *slog.Logger) {
			logger.With("level", slog.LevelError, "msg", "hi", "time", when).Warn("x")
		}},
		{"values of other types", func(logger *slog.Logger) {
			logger.Warn("x", "level", "high", "msg", 42, "time", "yesterday")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := logging.New(&buf, "api", "info")
			if err != nil {
				t.Fatal(err)
			}
			tt.log(logger)
			raw := buf.String()
			// Each key once: json.Unmarshal would hide a duplicate by keeping the last.
			for _, key := range []string{`"level":`, `"msg":`, `"time":`} {
				if n := strings.Count(raw, key); n != 1 {
					t.Errorf("%s appears %d times in %s", key, n, raw)
				}
			}
			line := lines(t, &buf)[0]
			if line["level"] != "warn" || line["msg"] != "x" {
				t.Errorf("level, msg = %v, %v; want warn, x", line["level"], line["msg"])
			}
			for _, key := range []string{"attr.level", "attr.msg", "attr.time"} {
				if _, ok := line[key]; !ok {
					t.Errorf("%s missing from %s", key, raw)
				}
			}
		})
	}
}
