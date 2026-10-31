// Package compliance is the deposit review: the decision (for now, at random) and the transitions
// compliance owns. They record the review and its decision and never move money; posting the
// decision (crediting or returning the funds) is the ledger's job (ledger.PostDecision). Node:
// node/src/compliance/ (decision.ts, transitions.ts).
//
// The orchestration layer (decision 57): it uses deposits' transitions.
package compliance

import (
	"context"
	"errors"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Decision is a compliance decision: approved, with an optional reason, or rejected, with a
// required one. Its fields are unexported, so a Decision comes only from Approved or Rejected, as
// withdrawals.Transition comes only from its constructors (Node: a union type). What a function
// parameter can't enforce is checked by Decide at run time: a rejection's reason being non-empty,
// and a transition other than Approve or Reject (the zero Decision included).
type Decision struct {
	transition deposits.Transition // Approve or Reject
	reason     string              // "" for none
}

// Approved is an approval; reason may be "" (none).
func Approved(reason string) Decision {
	return Decision{transition: deposits.Approve, reason: reason}
}

// Rejected is a rejection, with its reason (required).
func Rejected(reason string) Decision {
	return Decision{transition: deposits.Reject, reason: reason}
}

// Outcome is "approved" or "rejected" (the status the decision moves the deposit to), "" for the
// zero Decision.
func (d Decision) Outcome() string { return string(d.transition.To()) }

// Reason is the decision's reason, "" for none.
func (d Decision) Reason() string { return d.reason }

// RejectionReason is the random decider's reason for a rejection (Node's text).
const RejectionReason = "rejected by the demo compliance check (random)"

// DecideRandomly is the pseudo compliance check: approve with probability approveRate, otherwise
// reject (random() < approveRate approves, as Node). Not a rules engine (see Known gaps). random
// is a parameter so tests force either outcome; the process passes math/rand/v2's Float64, a
// number in [0, 1) as JavaScript's Math.random.
func DecideRandomly(approveRate float64, random func() float64) Decision {
	if random() < approveRate {
		return Approved("")
	}
	return Rejected(RejectionReason)
}

// Result is what a compliance transition did: the "applied" | "skipped" vocabulary of
// conditional transitions (CLAUDE.md).
type Result string

const (
	Applied Result = "applied"
	Skipped Result = "skipped"
)

func resultOf(applied bool) Result {
	if applied {
		return Applied
	}
	return Skipped
}

// StartReview is received -> under_review: compliance has picked the deposit up. Skipped if the
// deposit isn't received (already under review or beyond, or no such deposit).
func StartReview(ctx context.Context, tx db.Tx, id ids.DepositID) (Result, error) {
	_, applied, err := deposits.TransitionStatus(ctx, tx, id, deposits.StartReview, "")
	return resultOf(applied), err
}

// Decide is under_review -> approved | rejected, recording the decision's reason. Skipped if the
// deposit isn't under review, in particular if it has already been decided: a recorded decision is
// never re-made.
func Decide(ctx context.Context, tx db.Tx, id ids.DepositID, d Decision) (Result, error) {
	switch {
	case d.transition != deposits.Approve && d.transition != deposits.Reject:
		return "", errors.New("invalid decision (neither Approved nor Rejected)")
	case d.transition == deposits.Reject && d.reason == "":
		return "", errors.New("a rejection needs a reason")
	}
	_, applied, err := deposits.TransitionStatus(ctx, tx, id, d.transition, d.reason)
	return resultOf(applied), err
}
