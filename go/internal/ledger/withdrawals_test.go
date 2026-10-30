package ledger_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// The withdrawal lifecycle against the test database: the cases of
// node/test/ledger/withdrawals.test.ts. Each test uses its own account, funded through a real
// deposit (fundedAccount).
//
// Not ported: Node's "withdrawals table" cases (the SEPA character set CHECKs, '' refused for
// remittance_info). They test the shared schema, not Node's code, and run in Node's suite against
// the same migrations; repeating them here would test Postgres twice. Node's "types" case
// (illegal transitions and mismatched references don't compile) has no runnable Go counterpart:
// see Transition in package withdrawals and Reference in this package.

func withdrawalSteps(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT reference_step FROM balance_movements
WHERE reference_type = 'withdrawal' AND reference_id = $1 ORDER BY created_at`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return steps
}

func getWithdrawal(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID) withdrawals.Withdrawal {
	t.Helper()
	w, ok, err := withdrawals.Get(context.Background(), pool, id)
	if err != nil || !ok {
		t.Fatalf("Get(%v) = %v, %v", id, ok, err)
	}
	return w
}

// historyPairs is a withdrawal's history as (from, to) pairs, "" for the first row's NULL.
func historyPairs(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID) [][2]string {
	t.Helper()
	history, err := withdrawals.StatusHistory(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	pairs := make([][2]string, len(history))
	for i, change := range history {
		if change.From != nil {
			pairs[i][0] = string(*change.From)
		}
		pairs[i][1] = string(change.To)
	}
	return pairs
}

func withdrawalCount(t *testing.T, pool *pgxpool.Pool, account ids.AccountID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*)::int FROM withdrawals WHERE account_id = $1`, account).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// newWithdrawal creates a withdrawal that must succeed.
func newWithdrawal(t *testing.T, pool *pgxpool.Pool, account testdb.Account, amount money.Amount) ids.WithdrawalID {
	t.Helper()
	w, ok, err := createWithdrawal(pool, account, amount, "")
	if err != nil || !ok {
		t.Fatalf("createWithdrawal = %v, %v", ok, err)
	}
	return w.ID
}

func withdrawalPosting(
	pool *pgxpool.Pool,
	fn func(ctx context.Context, tx db.Tx) (ledger.Result, error),
) (ledger.Result, error) {
	return inTx(pool, fn)
}

func submit(pool *pgxpool.Pool, id ids.WithdrawalID, transferID withdrawals.TransferID) (ledger.Result, error) {
	return withdrawalPosting(pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
		return ledger.MarkSubmitted(ctx, tx, id, transferID)
	})
}

func execute(pool *pgxpool.Pool, id ids.WithdrawalID, transferID withdrawals.TransferID) (ledger.Result, error) {
	return withdrawalPosting(pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
		return ledger.Execute(ctx, tx, id, transferID)
	})
}

func release(
	pool *pgxpool.Pool,
	id ids.WithdrawalID,
	reason string,
	transferID withdrawals.TransferID,
) (ledger.Result, error) {
	return withdrawalPosting(pool, func(ctx context.Context, tx db.Tx) (ledger.Result, error) {
		return ledger.Release(ctx, tx, id, reason, transferID)
	})
}

func TestHoldMovesAvailableToHeld(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	id := newWithdrawal(t, pool, account, 400)

	if got := balancesOf(t, pool, account.ID); got != (balances{0, 600, 400}) {
		t.Errorf("balances = %+v", got)
	}
	if steps := withdrawalSteps(t, pool, id); !slices.Equal(steps, []string{"hold"}) {
		t.Errorf("movements = %v, want [hold]", steps)
	}
	w := getWithdrawal(t, pool, id)
	if w.Account != account.ID || w.Amount != 400 || w.Currency != "EUR" || w.CreditorName != "Jane Doe" ||
		w.Status != withdrawals.StatusHeld || w.FailureReason != nil || w.ProviderTransferID != nil {
		t.Errorf("withdrawal = %+v", w)
	}
	if h := historyPairs(t, pool, id); !slices.Equal(h, [][2]string{{"", "held"}}) {
		t.Errorf("history = %v", h)
	}
}

func TestInsufficientFundsStoresNothing(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	_, _, err := createWithdrawal(pool, account, 1001, "")

	insufficient, ok := errors.AsType[*ledger.InsufficientFundsError](err)
	if !ok {
		t.Fatalf("error = %v, want an InsufficientFundsError", err)
	}
	if insufficient.Account != account.ID || insufficient.Amount != 1001 {
		t.Errorf("InsufficientFundsError = %+v", insufficient)
	}
	// A business outcome, not the invariant violation an OverdraftError means, although it wraps
	// one (the database's refusal stays in the chain).
	if overdraft, ok := errors.AsType[*ledger.OverdraftError](err); !ok || overdraft.Bucket != ledger.BucketAvailable {
		t.Errorf("wrapped error = %v, want an OverdraftError on available", err)
	}
	if ledger.IsInvariantViolation(err) {
		t.Error("IsInvariantViolation(insufficient funds) = true, want false")
	}
	if n := withdrawalCount(t, pool, account.ID); n != 0 {
		t.Errorf("%d withdrawals stored, want 0 after the rollback", n)
	}
	if got := balancesOf(t, pool, account.ID); got != (balances{0, 1000, 0}) {
		t.Errorf("balances = %+v", got)
	}
}

func TestUnknownAccountIsAccountNotFound(t *testing.T) {
	pool := testdb.Pool(t)
	ghost := testdb.Account{ID: ids.New[ids.Account]()}
	_, _, err := createWithdrawal(pool, ghost, 100, "")
	notFound, ok := errors.AsType[*withdrawals.AccountNotFoundError](err)
	if !ok || notFound.Account != ghost.ID {
		t.Errorf("error = %v, want an AccountNotFoundError for %v", err, ghost.ID)
	}
}

// Two transactions, released together. Both inserts succeed (different keys); the holds then
// update the same balance row, so the second waits for the first's row lock. Once the first
// commits, the second's update sees available = 300 and the CHECK refuses it.
func TestOnlyOneOfTwoConcurrentWithdrawalsThatExceedAvailableCommits(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)

	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			<-start
			_, _, errs[i] = createWithdrawal(pool, account, 700, "")
		})
	}
	close(start)
	wg.Wait()

	committed, refused := 0, 0
	for _, err := range errs {
		switch _, insufficient := errors.AsType[*ledger.InsufficientFundsError](err); {
		case err == nil:
			committed++
		case insufficient:
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if committed != 1 || refused != 1 {
		t.Errorf("committed %d, refused %d; want 1 and 1", committed, refused)
	}
	if n := withdrawalCount(t, pool, account.ID); n != 1 {
		t.Errorf("%d withdrawals, want 1", n)
	}
	if got := balancesOf(t, pool, account.ID); got != (balances{0, 300, 700}) {
		t.Errorf("balances = %+v", got)
	}
}

func TestAReusedKeyHoldsNothingTwice(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	key := ids.New[ids.Event]().TypeID()
	first, _, err := createWithdrawal(pool, account, 100, key)
	if err != nil {
		t.Fatal(err)
	}

	_, ok, err := createWithdrawal(pool, account, 100, key)
	if err != nil || ok {
		t.Fatalf("repeat = %v, %v; want not inserted", ok, err)
	}
	if n := withdrawalCount(t, pool, account.ID); n != 1 {
		t.Errorf("%d withdrawals, want 1", n)
	}
	if steps := withdrawalSteps(t, pool, first.ID); !slices.Equal(steps, []string{"hold"}) {
		t.Errorf("movements = %v", steps)
	}
	if got := balancesOf(t, pool, account.ID); got.available != 900 || got.held != 100 {
		t.Errorf("balances = %+v", got)
	}

	// The same key on another account is another withdrawal.
	other := fundedAccount(t, pool, 1000)
	if _, ok, err := createWithdrawal(pool, other, 100, key); err != nil || !ok {
		t.Errorf("same key, other account = %v, %v; want created", ok, err)
	}
}

func TestMarkSubmittedStoresTheTransferIDWithoutMovingMoney(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	id := newWithdrawal(t, pool, account, 400)

	if r := must(submit(pool, id, "tr_provider_1")); r != ledger.Applied {
		t.Fatalf("MarkSubmitted = %v", r)
	}
	w := getWithdrawal(t, pool, id)
	if w.Status != withdrawals.StatusSubmitted || w.ProviderTransferID == nil || *w.ProviderTransferID != "tr_provider_1" {
		t.Errorf("withdrawal = %+v", w)
	}
	if steps := withdrawalSteps(t, pool, id); !slices.Equal(steps, []string{"hold"}) {
		t.Errorf("movements = %v", steps)
	}
	if got := balancesOf(t, pool, account.ID); got.available != 600 || got.held != 400 {
		t.Errorf("balances = %+v", got)
	}
	// A repeat (the submitter retrying, same transfer) skips.
	if r := must(submit(pool, id, "tr_provider_1")); r != ledger.Skipped {
		t.Errorf("repeat = %v, want skipped", r)
	}
}

func TestExecuteFromHeldAndFromSubmitted(t *testing.T) {
	for _, origin := range []withdrawals.Status{withdrawals.StatusHeld, withdrawals.StatusSubmitted} {
		t.Run(string(origin), func(t *testing.T) {
			pool := testdb.Pool(t)
			account := fundedAccount(t, pool, 1000)
			id := newWithdrawal(t, pool, account, 400)
			if origin == withdrawals.StatusSubmitted {
				must(submit(pool, id, "tr_1"))
			}

			if r := must(execute(pool, id, "")); r != ledger.Applied {
				t.Fatalf("Execute = %v", r)
			}
			if s := getWithdrawal(t, pool, id).Status; s != withdrawals.StatusExecuted {
				t.Errorf("status = %s", s)
			}
			if got := balancesOf(t, pool, account.ID); got != (balances{0, 600, 0}) {
				t.Errorf("balances = %+v", got)
			}
			if steps := withdrawalSteps(t, pool, id); !slices.Equal(steps, []string{"hold", "execute"}) {
				t.Errorf("movements = %v", steps)
			}
			// The history records the status it actually came from.
			if h := historyPairs(t, pool, id); h[len(h)-1] != [2]string{string(origin), "executed"} {
				t.Errorf("last history row = %v", h[len(h)-1])
			}
			if r := must(execute(pool, id, "")); r != ledger.Skipped {
				t.Errorf("repeat = %v, want skipped", r)
			}
			if steps := withdrawalSteps(t, pool, id); !slices.Equal(steps, []string{"hold", "execute"}) {
				t.Errorf("movements after the repeat = %v", steps)
			}
		})
	}
}

func TestReleaseFromHeldAndFromSubmitted(t *testing.T) {
	for _, origin := range []withdrawals.Status{withdrawals.StatusHeld, withdrawals.StatusSubmitted} {
		t.Run(string(origin), func(t *testing.T) {
			pool := testdb.Pool(t)
			account := fundedAccount(t, pool, 1000)
			id := newWithdrawal(t, pool, account, 400)
			if origin == withdrawals.StatusSubmitted {
				must(submit(pool, id, "tr_1"))
			}

			if r := must(release(pool, id, "destination closed", "")); r != ledger.Applied {
				t.Fatalf("Release = %v", r)
			}
			w := getWithdrawal(t, pool, id)
			if w.Status != withdrawals.StatusFailed || w.FailureReason == nil || *w.FailureReason != "destination closed" {
				t.Errorf("withdrawal = %+v", w)
			}
			if got := balancesOf(t, pool, account.ID); got != (balances{0, 1000, 0}) {
				t.Errorf("balances = %+v", got)
			}
			if steps := withdrawalSteps(t, pool, id); !slices.Equal(steps, []string{"hold", "release"}) {
				t.Errorf("movements = %v", steps)
			}
			history, err := withdrawals.StatusHistory(context.Background(), pool, id)
			if err != nil {
				t.Fatal(err)
			}
			last := history[len(history)-1]
			if last.From == nil || *last.From != origin || last.To != withdrawals.StatusFailed ||
				last.Reason == nil || *last.Reason != "destination closed" {
				t.Errorf("last history row = %+v", last)
			}
			if r := must(release(pool, id, "again", "")); r != ledger.Skipped {
				t.Errorf("repeat = %v, want skipped", r)
			}
		})
	}
}

func TestFinalStatusesNeverChange(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	released := newWithdrawal(t, pool, account, 100)
	executed := newWithdrawal(t, pool, account, 200)
	must(release(pool, released, "refused", ""))
	must(execute(pool, executed, ""))
	before := balancesOf(t, pool, account.ID)

	for name, r := range map[string]ledger.Result{
		"execute after release": must(execute(pool, released, "")),
		"release after execute": must(release(pool, executed, "late", "")),
		"submit after release":  must(submit(pool, released, "tr_late")),
	} {
		if r != ledger.Skipped {
			t.Errorf("%s = %v, want skipped", name, r)
		}
	}
	if s := getWithdrawal(t, pool, released).Status; s != withdrawals.StatusFailed {
		t.Errorf("released status = %s", s)
	}
	if s := getWithdrawal(t, pool, executed).Status; s != withdrawals.StatusExecuted {
		t.Errorf("executed status = %s", s)
	}
	if steps := withdrawalSteps(t, pool, released); !slices.Equal(steps, []string{"hold", "release"}) {
		t.Errorf("released movements = %v", steps)
	}
	if steps := withdrawalSteps(t, pool, executed); !slices.Equal(steps, []string{"hold", "execute"}) {
		t.Errorf("executed movements = %v", steps)
	}
	if got := balancesOf(t, pool, account.ID); got != before {
		t.Errorf("balances = %+v, want %+v", got, before)
	}
}

// Decision 27: the webhook settles the withdrawal before the submitter has processed the response
// to its own request.
func TestTheProvidersUpdateWinsTheRace(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	id := newWithdrawal(t, pool, account, 400)

	if r := must(execute(pool, id, "tr_from_update")); r != ledger.Applied {
		t.Fatalf("Execute = %v", r)
	}
	// The submitter's response for the same transfer arrives late.
	if r := must(submit(pool, id, "tr_from_update")); r != ledger.Skipped {
		t.Errorf("late MarkSubmitted = %v, want skipped", r)
	}
	// The update's transfer ID is stored: the skipped submission couldn't record it.
	w := getWithdrawal(t, pool, id)
	if w.Status != withdrawals.StatusExecuted || w.ProviderTransferID == nil || *w.ProviderTransferID != "tr_from_update" {
		t.Errorf("withdrawal = %+v", w)
	}
	if h := historyPairs(t, pool, id); !slices.Equal(h, [][2]string{{"", "held"}, {"held", "executed"}}) {
		t.Errorf("history = %v", h)
	}
}

// snapshot is everything about a withdrawal that a refused transition must leave as it was.
type snapshot struct {
	withdrawal withdrawals.Withdrawal
	history    [][2]string
	movements  []string
	balances   balances
}

func snapshotOf(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID, account ids.AccountID) snapshot {
	t.Helper()
	return snapshot{
		withdrawal: getWithdrawal(t, pool, id),
		history:    historyPairs(t, pool, id),
		movements:  withdrawalSteps(t, pool, id),
		balances:   balancesOf(t, pool, account),
	}
}

func submittedWith(t *testing.T, pool *pgxpool.Pool, transferID withdrawals.TransferID) (testdb.Account, ids.WithdrawalID) {
	t.Helper()
	account := fundedAccount(t, pool, 1000)
	id := newWithdrawal(t, pool, account, 400)
	must(submit(pool, id, transferID))
	return account, id
}

func TestASettlementReportingAnotherTransferIDChangesNothing(t *testing.T) {
	settlements := map[string]func(*pgxpool.Pool, ids.WithdrawalID) (ledger.Result, error){
		"execute": func(pool *pgxpool.Pool, id ids.WithdrawalID) (ledger.Result, error) {
			return execute(pool, id, "tr_B")
		},
		"release": func(pool *pgxpool.Pool, id ids.WithdrawalID) (ledger.Result, error) {
			return release(pool, id, "refused", "tr_B")
		},
	}
	for name, settle := range settlements {
		t.Run(name, func(t *testing.T) {
			pool := testdb.Pool(t)
			account, id := submittedWith(t, pool, "tr_A")
			before := snapshotOf(t, pool, id, account.ID)

			_, err := settle(pool, id)
			mismatch, ok := errors.AsType[*withdrawals.TransferIDMismatchError](err)
			if !ok {
				t.Fatalf("error = %v, want a TransferIDMismatchError", err)
			}
			if mismatch.Withdrawal != id || mismatch.Stored != "tr_A" || mismatch.Reported != "tr_B" {
				t.Errorf("TransferIDMismatchError = %+v", mismatch)
			}
			// reflect.DeepEqual compares the two snapshots field by field, following pointers.
			if after := snapshotOf(t, pool, id, account.ID); !reflect.DeepEqual(after, before) {
				t.Errorf("changed:\n%+v\nwas\n%+v", after, before)
			}
		})
	}
}

func TestASettlementWithTheSameTransferIDOrNoneApplies(t *testing.T) {
	pool := testdb.Pool(t)
	_, same := submittedWith(t, pool, "tr_A")
	if r := must(execute(pool, same, "tr_A")); r != ledger.Applied {
		t.Errorf("Execute with the same ID = %v", r)
	}
	_, none := submittedWith(t, pool, "tr_A")
	if r := must(release(pool, none, "refused", "")); r != ledger.Applied {
		t.Errorf("Release with no ID = %v", r)
	}
	if w := getWithdrawal(t, pool, none); w.ProviderTransferID == nil || *w.ProviderTransferID != "tr_A" {
		t.Errorf("transfer ID = %v, want tr_A kept", w.ProviderTransferID)
	}
}

func TestARepeatedUpdateForAFinalWithdrawal(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)
	id := newWithdrawal(t, pool, account, 400)
	must(execute(pool, id, "tr_A"))
	before := snapshotOf(t, pool, id, account.ID)

	if r := must(execute(pool, id, "tr_A")); r != ledger.Skipped {
		t.Errorf("same ID = %v, want skipped", r)
	}
	_, err := execute(pool, id, "tr_B")
	if _, ok := errors.AsType[*withdrawals.TransferIDMismatchError](err); !ok {
		t.Errorf("another ID: error = %v, want a TransferIDMismatchError", err)
	}
	if after := snapshotOf(t, pool, id, account.ID); !reflect.DeepEqual(after, before) {
		t.Errorf("changed:\n%+v\nwas\n%+v", after, before)
	}
}

func TestASubmissionAfterTheUpdate(t *testing.T) {
	pool := testdb.Pool(t)
	account := fundedAccount(t, pool, 1000)

	// The update stored tr_A: another ID is a contradiction, the same one an ordinary skip.
	reported := newWithdrawal(t, pool, account, 100)
	must(execute(pool, reported, "tr_A"))
	if _, err := submit(pool, reported, "tr_B"); err == nil {
		t.Error("MarkSubmitted with another ID succeeded, want a TransferIDMismatchError")
	} else if _, ok := errors.AsType[*withdrawals.TransferIDMismatchError](err); !ok {
		t.Errorf("error = %v, want a TransferIDMismatchError", err)
	}
	if r := must(submit(pool, reported, "tr_A")); r != ledger.Skipped {
		t.Errorf("MarkSubmitted with the same ID = %v, want skipped", r)
	}

	// The update stored none: the submission skips, and stores nothing (the withdrawal is final).
	unreported := newWithdrawal(t, pool, account, 100)
	must(release(pool, unreported, "refused", ""))
	if r := must(submit(pool, unreported, "tr_B")); r != ledger.Skipped {
		t.Errorf("MarkSubmitted = %v, want skipped", r)
	}
	if w := getWithdrawal(t, pool, unreported); w.ProviderTransferID != nil {
		t.Errorf("transfer ID = %v, want none", *w.ProviderTransferID)
	}
}
