// Package logging builds the structured JSON logger (log/slog), its lines shaped like the Node
// implementation's pino lines (node/src/shared/logger.ts), so one log pipeline reads both:
//
//	{"time":"2026-10-27T10:34:00.403Z","level":"info","msg":"deposit processed",
//	 "service":"deposit-worker","implementation":"go","deposit_id":"dep_01m5…"}
//
// Where slog's defaults differ from pino's, this package matches pino:
//
//   - level: slog writes "INFO"; pino (as Node configures it) writes "info". Lower-cased, and
//     the two levels slog lacks, trace and fatal, are added (LevelTrace, LevelFatal).
//   - time: slog writes local time to the nanosecond with an offset; pino writes UTC to the
//     millisecond ("…T10:34:00.403Z"). Reformatted.
//   - errors: slog writes an error as its message string; pino writes {type, message, stack}.
//     Written as {"type": the Go type, "message": …}; Go errors carry no stack.
//   - amounts: money.Amount logs itself as a digit string (its LogValue method), as Node's
//     logger turns BigInts into strings.
//
// Not matched: the order of the keys. slog always writes time, level and msg first; pino writes
// msg last. JSON objects are unordered, and no reader of these logs depends on the order.
//
// Context is attached the slog way, logger.With("deposit_id", id), the counterpart of pino's
// logger.child({ deposit_id }).
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// The levels pino has and slog doesn't. slog's levels are integers 4 apart (debug -4, info 0,
// warn 4, error 8), leaving room for these.
const (
	LevelTrace = slog.Level(-8)
	LevelFatal = slog.Level(12)
)

var levels = map[string]slog.Level{
	"trace": LevelTrace,
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
	"fatal": LevelFatal,
}

// New returns a logger writing JSON lines to w (os.Stdout in a process), each with service and
// implementation="go". level is a LOG_LEVEL value (config.LogLevels); "silent" logs nothing.
func New(w io.Writer, service, level string) (*slog.Logger, error) {
	if level == "silent" {
		return slog.New(slog.DiscardHandler), nil
	}
	minimum, ok := levels[level]
	if !ok {
		return nil, fmt.Errorf("unknown log level %q", level)
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: minimum, ReplaceAttr: pinoShape})
	return slog.New(handler).With("service", service, "implementation", "go"), nil
}

// Fatal logs at the fatal level, which slog has no method for. It doesn't exit: the caller
// decides how to stop, as with pino's logger.fatal.
func Fatal(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelFatal, msg, args...)
}

// pinoShape is the handler's ReplaceAttr hook: slog calls it for every attribute it is about to
// write (including time and level), and writes whatever it returns. groups is non-empty for an
// attribute inside a group; time and level are only ever at the top.
func pinoShape(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			return slog.String(slog.TimeKey, a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
		case slog.LevelKey:
			level, _ := a.Value.Any().(slog.Level)
			return slog.String(slog.LevelKey, levelName(level))
		}
	}
	if err, ok := a.Value.Any().(error); ok {
		return slog.Group(a.Key, slog.String("type", fmt.Sprintf("%T", err)), slog.String("message", err.Error()))
	}
	return a
}

func levelName(level slog.Level) string {
	for name, l := range levels {
		if l == level {
			return name
		}
	}
	// A level between the named ones (e.g. info+1, which slog allows): slog's own name for it,
	// lower-cased ("info+1").
	return strings.ToLower(level.String())
}
