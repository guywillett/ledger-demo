// Package deposits is the deposit entity: its statuses, its legal transitions and their SQL.
// Node: node/src/deposits/status.ts.
//
// The entity layer (decision 57): it imports no ledger. The ledger's postings (book, credit,
// return) and, later, compliance's transitions (review, decision) use the transitions defined
// here; only the ledger's change balances.
//
// The lifecycle:
//
//	created -> received -> under_review -> approved -> credited
//	                                    \-> rejected -> returned
//
// plus unmatched (suspense), which ingestion creates directly and no transition enters or leaves.
package deposits

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
)

// Status is a deposit's status. A defined string type: a plain string can't be passed where a
// Status is expected without a conversion, and the constants below are the values the database's
// CHECK allows. Status names are part of the event contract (deposit.<status>).
type Status string

const (
	StatusCreated     Status = "created"
	StatusReceived    Status = "received"
	StatusUnderReview Status = "under_review"
	StatusApproved    Status = "approved"
	StatusRejected    Status = "rejected"
	StatusCredited    Status = "credited"
	StatusReturned    Status = "returned"
	// Suspense: the deposit matches none of our accounts. Outside the lifecycle: a terminal state
	// until a resolution flow (match to an account, or return) exists.
	StatusUnmatched Status = "unmatched"
)

// ParseStatus checks a status read from the database.
func ParseStatus(s string) (Status, error) {
	switch status := Status(s); status {
	case StatusCreated, StatusReceived, StatusUnderReview, StatusApproved, StatusRejected,
		StatusCredited, StatusReturned, StatusUnmatched:
		return status, nil
	}
	return "", fmt.Errorf("unknown deposit status in database: %q", s)
}

// Transition is one of the legal status changes, and nothing else: only the constants below exist,
// each with its from and to status fixed in the table under them.
//
// This is Go's answer to Node's transition types (status.ts), where an illegal pair such as
// credited -> created doesn't compile (node/test/ledger/lifecycle.test.ts, "rejects illegal
// transitions at compile time"). Go can't check a pair of values at compile time, so the caller
// doesn't pass a pair at all: it names a transition. The constants can't be reassigned (as
// exported variables could), and the table can't be changed from outside the package. What Go
// can't rule out is a value built by conversion, deposits.Transition(42): TransitionStatus
// rejects it with an error.
type Transition int

// The transitions, grouped by the module that owns them, as in Node.
const (
	// Posting (the ledger): each moves money.
	Book   Transition = iota + 1 // created -> received
	Credit                       // approved -> credited
	Return                       // rejected -> returned
	// Compliance: no money moves.
	StartReview // received -> under_review
	Approve     // under_review -> approved
	Reject      // under_review -> rejected
)

var transitions = map[Transition]struct{ from, to Status }{
	Book:        {StatusCreated, StatusReceived},
	Credit:      {StatusApproved, StatusCredited},
	Return:      {StatusRejected, StatusReturned},
	StartReview: {StatusReceived, StatusUnderReview},
	Approve:     {StatusUnderReview, StatusApproved},
	Reject:      {StatusUnderReview, StatusRejected},
}

// From is the status the transition starts from ("" for an invalid Transition).
func (t Transition) From() Status { return transitions[t].from }

// To is the status the transition ends in ("" for an invalid Transition).
func (t Transition) To() Status { return transitions[t].to }

// String names the transition by its statuses, e.g. "created -> received" (fmt uses it).
func (t Transition) String() string {
	pair, ok := transitions[t]
	if !ok {
		return fmt.Sprintf("Transition(%d)", int(t))
	}
	return string(pair.from) + " -> " + string(pair.to)
}

// Transitioned is what a transition reports about the deposit it moved: what a posting needs to
// post its movement.
type Transitioned struct {
	Account ids.AccountID
	Amount  money.Amount
}

const transitionSQL = `
UPDATE deposits
SET status = $3, updated_at = now()
WHERE id = $1 AND status = $2
RETURNING account_id, amount_minor
`

const historySQL = `
INSERT INTO deposit_status_history (deposit_id, from_status, to_status, reason)
VALUES ($1, $2, $3, $4)
`

// TransitionStatus moves a deposit from t's from status to its to status if, and only if, it is
// currently in the from status, and records the history row in the same transaction (a Tx: both
// statements must commit together). reason is recorded with the change; "" records none (NULL).
//
// It returns the deposit's account and amount and true when this call made the transition, and
// false when no row changed: the transition already happened, or the deposit is in another state
// (or doesn't exist). That ambiguity is deliberate: it is what makes repeating a transition safe
// (no check-then-act). Postgres re-evaluates the WHERE after waiting for a concurrent
// transaction's row lock, so of two concurrent callers exactly one sees its row change.
//
// "Not applied" is a bool, not an error: it is a normal outcome every caller handles, and an error
// would make each caller tell it apart from real failures with errors.Is. The comma-ok form
// (value, ok) is Go's usual shape for "maybe there's a value", as in a map lookup.
func TransitionStatus(
	ctx context.Context,
	tx db.Tx,
	id ids.DepositID,
	t Transition,
	reason string,
) (Transitioned, bool, error) {
	pair, ok := transitions[t]
	if !ok {
		return Transitioned{}, false, fmt.Errorf("invalid deposit transition %v", t)
	}
	var moved Transitioned
	err := tx.QueryRow(ctx, transitionSQL, id, pair.from, pair.to).Scan(&moved.Account, &moved.Amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transitioned{}, false, nil
	}
	if err != nil {
		return Transitioned{}, false, fmt.Errorf("deposit %v %v: %w", id, t, err)
	}
	var reasonArg *string // nil is written as NULL
	if reason != "" {
		reasonArg = &reason
	}
	if _, err := tx.Exec(ctx, historySQL, id, pair.from, pair.to, reasonArg); err != nil {
		return Transitioned{}, false, fmt.Errorf("deposit %v %v history: %w", id, t, err)
	}
	return moved, true, nil
}
