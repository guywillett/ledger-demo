package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// ContractLogs is the log processor that gives each record the contract's shape where otelslog
// (package logging's bridge) doesn't (contracts/telemetry.md, Logs), as ContractMetrics does for
// metrics. It goes before the exporting processor: the SDK runs processors in the order they're
// registered, and a change one makes in OnEmit is what the next one sees. Node's copy builds its
// records this way to begin with (node/src/shared/logger.ts).
//
//   - The trace link: otelslog takes a record's trace and span IDs from the context passed to Handle,
//     and most of our code logs without one (logger.Info, not InfoContext), so its records would have
//     none. The IDs are on the logger instead, bound as trace_id and span_id (decision 86,
//     logging.WithTrace): they become the record's own, as Node's are, rather than attributes. The
//     context's, when a line has one, are kept: a span started after the logger was bound is the
//     nearer one.
//     Some backends (Loki) link a record from trace_id and span_id
//     attributes too, but not all, so this keeps the records backend-agnostic.
//   - The severity text: otelslog writes slog's name for the level (INFO; DEBUG-4 for our trace and
//     ERROR+4 for fatal); the contract's is stdout's (info, trace, fatal).
type ContractLogs struct{}

var _ sdklog.Processor = ContractLogs{}

// otelslog's severity is the slog level plus this (slog's debug -4 is OpenTelemetry's DEBUG 5).
const severityOffset = slog.Level(log.SeverityDebug) - slog.LevelDebug

// OnEmit reshapes record in place, for the processors after it.
func (ContractLogs) OnEmit(_ context.Context, record *sdklog.Record) error {
	record.SetSeverityText(logging.LevelName(slog.Level(record.Severity()) - severityOffset))

	var traceID, spanID string
	var others []attribute.KeyValue
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		switch kv.Key {
		case "trace_id":
			traceID = kv.Value.AsString()
		case "span_id":
			spanID = kv.Value.AsString()
		default:
			others = append(others, kv)
		}
		return true
	})
	if traceID == "" && spanID == "" {
		return nil
	}
	record.SetAttributes(others...)
	if record.TraceID().IsValid() {
		return nil
	}
	// Valid unless something other than logging.WithTrace bound them; dropped then.
	if id, err := trace.TraceIDFromHex(traceID); err == nil {
		record.SetTraceID(id)
	}
	if id, err := trace.SpanIDFromHex(spanID); err == nil {
		record.SetSpanID(id)
	}
	return nil
}

// Enabled is true: it reshapes every record, and leaves deciding to the processors after it.
func (ContractLogs) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

// Shutdown and ForceFlush have nothing to do: it holds no records.
func (ContractLogs) Shutdown(context.Context) error { return nil }

func (ContractLogs) ForceFlush(context.Context) error { return nil }
