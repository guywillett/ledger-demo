package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// transitions is ledger_transitions_total (contracts/telemetry.md, Metrics; slice 5 decision 88):
// one count per applied status transition of a deposit or a withdrawal, its creation included
// (from "none"). One instrument for the process, made at package init from the global meter,
// before Setup registers a provider: the global meter delegates, so it records once one is (but
// to the first provider registered only; see internal/ledger's metric tests). The error is
// ignored: Int64Counter fails only for an invalid name or unit, and these are constants.
var transitions, _ = otel.Meter(
	"github.com/guywillett/ledger-demo/go/internal/telemetry",
).Int64Counter("ledger_transitions_total",
	metric.WithUnit("{transition}"),
	metric.WithDescription("Applied status transitions of deposits and withdrawals, their "+
		"creation included, counted once each after their transaction commits."),
)

// CountTransition counts one transition of entity ("deposit" or "withdrawal") from one status to
// another ("none" for a creation). Call it from a tx.AfterCommit hook, so it counts only what
// committed. implementation is an attribute of its own, as Node's: it saves every query by
// implementation a join on target_info. Node: countTransition in node/src/shared/metrics.ts.
func CountTransition(entity, from, to string) {
	transitions.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("entity", entity),
		attribute.String("from", from),
		attribute.String("to", to),
		attribute.String("implementation", "go"),
	))
}
