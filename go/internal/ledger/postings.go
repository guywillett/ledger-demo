package ledger

import (
	"context"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Posting: the transitions that move money. Each changes the entity's status and posts its movement
// in the caller's transaction, so both happen or neither does. Node: node/src/ledger/posting.ts.
// The compliance transitions in between (review, decision) move no money and aren't here.
//
// A transition whose movement already exists returns a *LedgerInconsistencyError (MoveOnce).

// Result is what a posting did: the "applied" | "skipped" vocabulary of conditional transitions
// (CLAUDE.md).
type Result string

const (
	// Applied: this call made the transition and posted its movement.
	Applied Result = "applied"
	// Skipped: the deposit wasn't in the transition's from status (already done, or another state).
	Skipped Result = "skipped"
)

// postingStep is one of the ledger's own deposit transitions and the movement it posts.
type postingStep struct {
	transition deposits.Transition
	from, to   Bucket
	step       DepositStep
}

var (
	bookStep   = postingStep{deposits.Book, BucketExternal, BucketPendingIn, StepBook}
	creditStep = postingStep{deposits.Credit, BucketPendingIn, BucketAvailable, StepCredit}
	returnStep = postingStep{deposits.Return, BucketPendingIn, BucketExternal, StepReturn}
)

// Book: created -> received, the funds booked into pending_in.
func Book(ctx context.Context, tx db.Tx, id ids.DepositID) (Result, error) {
	return post(ctx, tx, id, bookStep)
}

// Credit: approved -> credited, pending_in -> available.
func Credit(ctx context.Context, tx db.Tx, id ids.DepositID) (Result, error) {
	return post(ctx, tx, id, creditStep)
}

// ReturnToSender: rejected -> returned, pending_in -> external.
func ReturnToSender(ctx context.Context, tx db.Tx, id ids.DepositID) (Result, error) {
	return post(ctx, tx, id, returnStep)
}

// Posted is what PostDecision did.
type Posted string

const (
	PostedCredit  Posted = "credited"
	PostedReturn  Posted = "returned"
	PostedSkipped Posted = "skipped" // not decided yet, or already posted
)

// PostDecision posts whatever compliance decided, reading the decision from the deposit's recorded
// state: it tries both conditional transitions, and at most one can match.
func PostDecision(ctx context.Context, tx db.Tx, id ids.DepositID) (Posted, error) {
	credited, err := Credit(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if credited == Applied {
		return PostedCredit, nil
	}
	returned, err := ReturnToSender(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if returned == Applied {
		return PostedReturn, nil
	}
	return PostedSkipped, nil
}

func post(ctx context.Context, tx db.Tx, id ids.DepositID, s postingStep) (Result, error) {
	deposit, applied, err := deposits.TransitionStatus(ctx, tx, id, s.transition, "")
	if err != nil {
		return "", err
	}
	if !applied {
		return Skipped, nil
	}
	err = MoveOnce(ctx, tx, Movement{
		Account: deposit.Account,
		From:    s.from,
		To:      s.to,
		Amount:  deposit.Amount,
		Ref:     DepositReference(id, s.step),
	})
	if err != nil {
		return "", err
	}
	return Applied, nil
}
