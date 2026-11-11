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
//
// Each line also goes to the telemetry backend, as a copy (slice 5 decision 93; New): stdout stays
// the source of truth.
package logging

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
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
//
// Each line also goes, as a copy, to OpenTelemetry's otelslog bridge (slice 5 decision 93): the
// global logger provider, a no-op until telemetry.Setup registers one, which exports it. The two
// sides are slog's multi-handler, which clones each record for each side and passes With's and
// WithGroup's to both, so a bound logger's fields reach both:
//   - stdout: the JSON handler, with service and implementation bound on it alone (on the backend
//     they're the resource's, not attributes);
//   - the copy: otelslog behind a gate at the same level, so it is a copy of stdout, no more, and
//     with plainValues, so a named type's value reaches it as stdout writes it.
//
// renameReserved wraps the multi-handler, so both sides get the same renamed keys ("attr.level"),
// renamed once per line.
func New(w io.Writer, service, level string) (*slog.Logger, error) {
	if level == "silent" {
		return slog.New(slog.DiscardHandler), nil
	}
	minimum, ok := levels[level]
	if !ok {
		return nil, fmt.Errorf("unknown log level %q", level)
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: minimum, ReplaceAttr: pinoShape}).
		WithAttrs([]slog.Attr{slog.String("service", service), slog.String("implementation", "go")})
	otelHandler := otelslog.NewHandler("github.com/guywillett/ledger-demo/go/internal/logging")
	multiHandler := slog.NewMultiHandler(handler, gate{plainValues{otelHandler}, minimum})
	return slog.New(renameReserved{multiHandler}), nil
}

// WithTrace returns logger with the trace_id and span_id of the span in ctx, so every line it
// writes leads to its trace (slice 5 decision 86); logger itself if ctx has no span (or telemetry is
// off). slog sees a context only through its ...Context calls, which this code mostly doesn't use,
// so the IDs are bound where a request's and a delivery's loggers are made instead (the API's
// withRequestID, consumer's Deliver). Node: the logger's mixin, on every line.
func WithTrace(ctx context.Context, logger *slog.Logger) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return logger
	}
	return logger.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}

// Fatal logs at the fatal level, which slog has no method for. It doesn't exit: the caller
// decides how to stop, as with pino's logger.fatal.
func Fatal(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	logger.Log(ctx, LevelFatal, msg, args...)
}

// gate wraps a handler and disables it below minLevel: the backend side's LOG_LEVEL. otelslog has no
// level option, and the SDK's logger is enabled at every level, so without it a Debug line with
// LOG_LEVEL=info would skip stdout but still be exported.
//
// All three of WithAttrs, WithGroup and Enabled are its own: promoted from the embedded handler,
// WithAttrs and WithGroup would return the bare inner handler, and a logger from With or WithGroup
// would lose the gate (the trap renameReserved's comment describes). Handle is promoted: the gate
// only decides, through Enabled.
type gate struct {
	slog.Handler
	minLevel slog.Level
}

// A compile-time check that gate is a slog.Handler.
var _ slog.Handler = gate{}

// Enabled asks the wrapped handler too, so the gate only ever narrows: with telemetry off (a no-op
// provider) otelslog says no at every level, and the line skips otelslog's conversion altogether.
func (h gate) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.minLevel && h.Handler.Enabled(ctx, l)
}

func (h gate) WithAttrs(as []slog.Attr) slog.Handler {
	return gate{h.Handler.WithAttrs(as), h.minLevel}
}

func (h gate) WithGroup(name string) slog.Handler {
	return gate{h.Handler.WithGroup(name), h.minLevel}
}

// plainValues wraps a handler (the backend side's otelslog) and gives it each attribute whose value
// is of a named type with a basic underlying type (withdrawals.Status, a string; ingestion.Kind, an
// int) as that plain value: withdrawals.Status("executed") as the string "executed", as stdout's
// JSON handler writes it. otelslog converts only the built-in types themselves, and writes any
// other such value as "unhandled: (withdrawals.Status) executed". The contract says attributes are
// the line's fields as written.
//
// Left as they are: errors (otelslog makes them the record's exception), and values with a JSON or
// text marshalling of their own (encoding/json writes those its way; none is logged today). A
// LogValuer (money.Amount, the IDs) is resolved first, so its result is what's converted.
//
// The same middleware shape as renameReserved: an attribute reaches a handler through Handle (a log
// call's) or WithAttrs (a With's), so both convert, and WithGroup re-wraps so a grouped logger keeps
// converting. Enabled is promoted.
type plainValues struct {
	slog.Handler
}

// A compile-time check that plainValues is a slog.Handler.
var _ slog.Handler = plainValues{}

// Handle passes on a copy of r with its values made plain (a Record's attributes can't be changed
// in place).
func (h plainValues) Handle(ctx context.Context, r slog.Record) error {
	newRecord := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		newRecord.AddAttrs(slog.Attr{Key: a.Key, Value: plain(a.Value)})
		return true
	})
	return h.Handler.Handle(ctx, newRecord)
}

func (h plainValues) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		newAttrs[i] = slog.Attr{Key: a.Key, Value: plain(a.Value)}
	}
	return plainValues{h.Handler.WithAttrs(newAttrs)}
}

func (h plainValues) WithGroup(name string) slog.Handler {
	return plainValues{h.Handler.WithGroup(name)}
}

// plain is v with a named basic type's value as its plain value (see plainValues), inside groups
// too; any other value as it is.
//
// Its two switches are over library kinds (slog's, reflect's), not our enums, and handle only the
// kinds that need converting: the exhaustive linter, there for our enums, is told to ignore them.
func plain(v slog.Value) slog.Value {
	// Only the kinds that need converting (see above).
	//exhaustive:ignore
	switch v.Kind() {
	case slog.KindLogValuer:
		return plain(v.Resolve())
	case slog.KindGroup:
		attrs := v.Group()
		plainAttrs := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			plainAttrs[i] = slog.Attr{Key: a.Key, Value: plain(a.Value)}
		}
		return slog.GroupValue(plainAttrs...)
	case slog.KindAny:
	default:
		return v // slog's own kinds (string, int64, time, …): otelslog converts them
	}
	switch v.Any().(type) {
	case error, json.Marshaler, encoding.TextMarshaler:
		return v
	}
	// reflect.ValueOf(x).Kind() is the underlying type's kind: String for withdrawals.Status.
	rv := reflect.ValueOf(v.Any())
	// Only the basic kinds; any other is left to otelslog.
	//exhaustive:ignore
	switch rv.Kind() {
	case reflect.String:
		return slog.StringValue(rv.String())
	case reflect.Bool:
		return slog.BoolValue(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64Value(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return slog.Uint64Value(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return slog.Float64Value(rv.Float())
	}
	return v
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
				return slog.String(slog.LevelKey, LevelName(level))
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

// LevelName is level's name on stdout: pino's (trace, debug, info, warn, error, fatal). Package
// telemetry names a log record's severity with it too (contracts/telemetry.md, Logs).
func LevelName(level slog.Level) string {
	for name, l := range levels {
		if l == level {
			return name
		}
	}
	// A level between the named ones (e.g. info+1, which slog allows): slog's own name for it,
	// lower-cased ("info+1").
	return strings.ToLower(level.String())
}
