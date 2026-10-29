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
//     logger turns BigInts into strings, and the same as its JSON format (MarshalJSON).
//   - reserved keys: slog doesn't remove duplicate keys, and JSON readers keep the last one, so a
//     caller's own "time", "level" or "msg" field would replace the real one when the line is
//     read. Such fields are written as "attr.time", "attr.level" and "attr.msg" (renameReserved).
//     Better still, don't use those names.
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
	return slog.New(renameReserved{handler}).With("service", service, "implementation", "go"), nil
}

// Fatal logs at the fatal level, which slog has no method for. It doesn't exit: the caller
// decides how to stop, as with pino's logger.fatal.
func Fatal(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelFatal, msg, args...)
}

// renameReserved wraps a handler and renames caller attributes whose keys would clash with the
// built-in time, level and msg fields (see renamed). It's a handler middleware: it changes what
// it's given, then passes it on to the handler it wraps.
//
// A caller's attributes reach a handler by two routes, so both are covered:
//   - Handle: the attributes of one log call (logger.Warn("x", "level", …)), in the Record.
//   - WithAttrs: the attributes of logger.With(…). They never appear in a later Record: With
//     calls WithAttrs once and gets back a new handler with them built in (the JSON handler
//     formats them there, once), which the new logger holds.
//
// slog.Handler is embedded (a field with no name), so the wrapped handler's methods are promoted:
// renameReserved has its Enabled and WithGroup as they are, and Handle and WithAttrs below
// replace the promoted ones. The promoted WithGroup returns an unwrapped handler, which is
// harmless: after WithGroup, every attribute is inside the group, where its key can't clash
// with the top-level time, level and msg.
type renameReserved struct {
	slog.Handler
}

// A compile-time check that renameReserved is a slog.Handler (the interface is satisfied
// implicitly; this fails the build if it ever stops being).
var _ slog.Handler = renameReserved{}

// renamed returns a with an "attr." prefix if its key would clash with a built-in field.
func renamed(a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey, slog.LevelKey, slog.MessageKey:
		a.Key = "attr." + a.Key
	}
	return a
}

// Handle passes on a copy of r with its attributes renamed where they clash. A Record's
// attributes can only be read through its Attrs method (a callback per attribute, returning true
// to carry on), not changed in place, hence the new Record.
func (h renameReserved) Handle(ctx context.Context, r slog.Record) error {
	newRecord := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		newRecord.AddAttrs(renamed(a))
		return true
	})
	return h.Handler.Handle(ctx, newRecord)
}

// WithAttrs renames clashing keys in attrs, and wraps the handler the wrapped handler returns:
// returned bare, the logger from With would hold an unwrapped JSON handler and rename nothing
// from then on. New calls With at once, so every logger would lose the wrapper.
func (h renameReserved) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		newAttrs[i] = renamed(a)
	}
	return renameReserved{h.Handler.WithAttrs(newAttrs)}
}

// pinoShape is the handler's ReplaceAttr hook: slog calls it for every attribute it is about to
// write (including time and level), and writes whatever it returns. groups is non-empty for an
// attribute inside a group; time and level are only ever at the top.
func pinoShape(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			if a.Value.Kind() == slog.KindTime {
				return slog.String(slog.TimeKey, a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
			}
			// A second line of defence: renameReserved has already renamed a caller's "time". If
			// one gets here anyway, Time() would panic on a value that isn't a time, so it's
			// renamed and written as given.
			return slog.Attr{Key: "attr." + slog.TimeKey, Value: a.Value}
		case slog.LevelKey:
			if level, ok := a.Value.Any().(slog.Level); ok {
				return slog.String(slog.LevelKey, levelName(level))
			}
			// As for time: a value that isn't a slog.Level is a caller's, renamed and written as
			// given (rather than turned into "info", the zero Level).
			return slog.Attr{Key: "attr." + slog.LevelKey, Value: a.Value}
		}
	}
	if err, ok := a.Value.Any().(error); ok {
		return slog.Group(a.Key,
			slog.String("type", fmt.Sprintf("%T", err)),
			slog.String("message", err.Error()))
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
