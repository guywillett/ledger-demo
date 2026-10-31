package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// POST /provider/transfer-updates against withdrawals set up the real way: the cases of
// node/test/api/transfer-updates.test.ts, and the transfer-updates row of body-errors.test.ts.
// Every answer to a parseable update is 204 with no body, whatever it did, so each test asserts the
// recorded effects: the transfer_updates row, balances, movements, the log line. Every response is
// checked against the contract (postRaw).

const transferUpdates = "/provider/transfer-updates"

type testWithdrawal struct {
	id      ids.WithdrawalID
	account testdb.Account
}

// newWithdrawal is a withdrawal of 400 from an account funded with 1000: held, or (submitted)
// submitted as tr_1.
func (a testAPI) newWithdrawal(t *testing.T, submitted bool) testWithdrawal {
	t.Helper()
	account := fundedAccount(t, a.pool, 1000)
	r := a.withdraw(t, account, nil, newKey())
	id, err := ids.FromTypeID[ids.Withdrawal](r.json(t)["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if submitted {
		_, err := db.InTxValue(t.Context(), a.pool, func(tx db.Tx) (ledger.Result, error) {
			return ledger.MarkSubmitted(t.Context(), tx, id, "tr_1")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return testWithdrawal{id: id, account: account}
}

func update(w testWithdrawal, overrides map[string]any) map[string]any {
	u := map[string]any{
		"provider_event_id": "pev_" + newKey(),
		"transfer_id":       "tr_1",
		"reference":         w.id.TypeID(),
		"status":            "executed",
		"occurred_at":       "2026-10-24T09:00:00.000Z",
	}
	for k, v := range overrides {
		if v == nil {
			delete(u, k)
		} else {
			u[k] = v
		}
	}
	return u
}

func (a testAPI) postUpdate(t *testing.T, u map[string]any) int {
	t.Helper()
	body, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return a.postRaw(t, transferUpdates, "application/json", string(body), nil).status
}

type updateRow struct {
	outcome    string
	count      int
	withdrawal *string
}

func updateRowOf(t *testing.T, pool *pgxpool.Pool, providerEventID any) (updateRow, bool) {
	t.Helper()
	var r updateRow
	err := pool.QueryRow(context.Background(), `
SELECT outcome, notification_count, withdrawal_id::text FROM transfer_updates
WHERE provider_event_id = $1`, providerEventID).Scan(&r.outcome, &r.count, &r.withdrawal)
	if err != nil {
		return updateRow{}, false
	}
	return r, true
}

func stepsOf(t *testing.T, pool *pgxpool.Pool, id ids.WithdrawalID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT reference_step FROM balance_movements
WHERE reference_type = 'withdrawal' AND reference_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, s)
	}
	return steps
}

func storedWithdrawal(
	t *testing.T,
	pool *pgxpool.Pool,
	id ids.WithdrawalID,
) withdrawals.Withdrawal {
	t.Helper()
	w, found, err := withdrawals.Get(context.Background(), pool, id)
	if err != nil || !found {
		t.Fatalf("withdrawal %v: %v %v", id, found, err)
	}
	return w
}

// updateLog is the last log line about this provider event (not the access log's).
func (a testAPI) updateLog(t *testing.T, providerEventID any) map[string]any {
	t.Helper()
	var found map[string]any
	for _, l := range a.logLines(t) {
		if l["provider_event_id"] == providerEventID && l["msg"] != "request completed" {
			found = l
		}
	}
	return found
}

func TestExecutesASubmittedWithdrawal(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	u := update(w, nil)
	if status := a.postUpdate(t, u); status != 204 {
		t.Fatalf("status %d", status)
	}
	row, _ := updateRowOf(t, a.pool, u["provider_event_id"])
	if row.outcome != "applied" || row.count != 1 || row.withdrawal == nil ||
		*row.withdrawal != w.id.UUID().String() {
		t.Errorf("row %+v", row)
	}
	got := storedWithdrawal(t, a.pool, w.id)
	if got.Status != withdrawals.StatusExecuted || got.ProviderTransferID == nil ||
		*got.ProviderTransferID != "tr_1" {
		t.Errorf("withdrawal %+v", got)
	}
	if b := balancesOf(t, a.pool, w.account); b != (balanceRow{0, 600, 0}) {
		t.Errorf("balances %+v", b)
	}
	if s := stepsOf(t, a.pool, w.id); !slices.Equal(s, []string{"hold", "execute"}) {
		t.Errorf("steps %v", s)
	}
	line := a.updateLog(t, u["provider_event_id"])
	if line["level"] != "info" || line["msg"] != "transfer update applied" ||
		line["withdrawal_id"] != w.id.TypeID() || line["transfer_id"] != "tr_1" ||
		line["status"] != "executed" {
		t.Errorf("line %v", line)
	}
}

func TestReleasesAFailedOneFundsBackInAvailableTheReasonStored(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	u := update(w, map[string]any{"status": "failed", "reason": "beneficiary account closed"})
	a.postUpdate(t, u)
	row, _ := updateRowOf(t, a.pool, u["provider_event_id"])
	if row.outcome != "applied" {
		t.Errorf("row %+v", row)
	}
	got := storedWithdrawal(t, a.pool, w.id)
	if got.Status != withdrawals.StatusFailed || got.FailureReason == nil ||
		*got.FailureReason != "beneficiary account closed" {
		t.Errorf("withdrawal %+v", got)
	}
	if b := balancesOf(t, a.pool, w.account); b != (balanceRow{0, 1000, 0}) {
		t.Errorf("balances %+v", b)
	}
	if s := stepsOf(t, a.pool, w.id); !slices.Equal(s, []string{"hold", "release"}) {
		t.Errorf("steps %v", s)
	}
}

func TestAppliesAnUpdateThatOvertakesTheSubmitterTheLateMarkSubmittedSkips(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, false) // still held
	u := update(w, nil)
	a.postUpdate(t, u)
	row, _ := updateRowOf(t, a.pool, u["provider_event_id"])
	if row.outcome != "applied" {
		t.Errorf("row %+v", row)
	}
	late, err := db.InTxValue(t.Context(), a.pool, func(tx db.Tx) (ledger.Result, error) {
		return ledger.MarkSubmitted(t.Context(), tx, w.id, "tr_1")
	})
	if err != nil || late != ledger.Skipped {
		t.Errorf("late MarkSubmitted: %v %v", late, err)
	}
	got := storedWithdrawal(t, a.pool, w.id)
	if got.Status != withdrawals.StatusExecuted || *got.ProviderTransferID != "tr_1" {
		t.Errorf("withdrawal %+v", got)
	}
}

func TestCountsTheSameEventResentSettlingNothingTwice(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	u := update(w, nil)
	a.postUpdate(t, u)
	if status := a.postUpdate(t, u); status != 204 {
		t.Fatalf("resend: %d", status)
	}
	row, _ := updateRowOf(t, a.pool, u["provider_event_id"])
	if row.outcome != "applied" || row.count != 2 {
		t.Errorf("row %+v", row)
	}
	if s := stepsOf(t, a.pool, w.id); !slices.Equal(s, []string{"hold", "execute"}) {
		t.Errorf("steps %v", s)
	}
	if line := a.updateLog(t, u["provider_event_id"]); line["msg"] != "duplicate transfer update" ||
		line["notification_count"] != 2.0 {
		t.Errorf("line %v", line)
	}
}

func TestRecordsASecondEventWithTheSameResultAsADuplicate(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	a.postUpdate(t, update(w, nil))
	second := update(w, nil)
	a.postUpdate(t, second)
	row, _ := updateRowOf(t, a.pool, second["provider_event_id"])
	if row.outcome != "duplicate" || row.count != 1 {
		t.Errorf("row %+v", row)
	}
	if s := stepsOf(t, a.pool, w.id); !slices.Equal(s, []string{"hold", "execute"}) {
		t.Errorf("steps %v", s)
	}
}

func TestChangesNothingWhenTheProviderReusesAnEventIDWithOtherDetailsAndAlerts(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	u := update(w, nil)
	a.postUpdate(t, u)
	reused := update(w, map[string]any{
		"provider_event_id": u["provider_event_id"], "transfer_id": "tr_other",
	})
	if status := a.postUpdate(t, reused); status != 204 {
		t.Fatalf("status %d", status)
	}
	row, _ := updateRowOf(t, a.pool, u["provider_event_id"])
	if row.outcome != "applied" || row.count != 1 {
		t.Errorf("row %+v", row)
	}
	line := a.updateLog(t, u["provider_event_id"])
	if line["transfer_id"] != "tr_other" || line["level"] != "error" || line["alert"] != true {
		t.Errorf("line %v", line)
	}
}

func TestRecordsFailedAfterExecutedAsAConflictMovingNothingAndAlerts(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	a.postUpdate(t, update(w, nil))
	contradiction := update(w, map[string]any{"status": "failed", "reason": "returned"})
	a.postUpdate(t, contradiction)
	row, _ := updateRowOf(t, a.pool, contradiction["provider_event_id"])
	if row.outcome != "conflict" {
		t.Errorf("row %+v", row)
	}
	if s := storedWithdrawal(t, a.pool, w.id).Status; s != withdrawals.StatusExecuted {
		t.Errorf("status %s", s)
	}
	if s := stepsOf(t, a.pool, w.id); !slices.Equal(s, []string{"hold", "execute"}) {
		t.Errorf("steps %v", s)
	}
	line := a.updateLog(t, contradiction["provider_event_id"])
	if line["level"] != "error" || line["alert"] != true || line["stored_status"] != "executed" ||
		line["stored_transfer_id"] != "tr_1" {
		t.Errorf("line %v", line)
	}
	// Node also checks reconciliation lists it (Node only, decision 50): the conflict row above is
	// what it reads.
}

func TestRecordsATransferIDMismatchAsAConflictMovingNothing(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	u := update(w, map[string]any{"transfer_id": "tr_other"})
	if status := a.postUpdate(t, u); status != 204 {
		t.Fatalf("status %d", status)
	}
	row, _ := updateRowOf(t, a.pool, u["provider_event_id"])
	if row.outcome != "conflict" {
		t.Errorf("row %+v", row)
	}
	got := storedWithdrawal(t, a.pool, w.id)
	if got.Status != withdrawals.StatusSubmitted || *got.ProviderTransferID != "tr_1" {
		t.Errorf("withdrawal %+v", got)
	}
	if s := stepsOf(t, a.pool, w.id); !slices.Equal(s, []string{"hold"}) {
		t.Errorf("steps %v", s)
	}
	line := a.updateLog(t, u["provider_event_id"])
	if line["level"] != "error" || line["alert"] != true || line["stored_transfer_id"] != "tr_1" ||
		line["stored_status"] != "submitted" {
		t.Errorf("line %v", line)
	}
}

func TestRecordsAnUpdateForAnUnknownWithdrawalOrAForeignReferenceAsUnmatchedAndAlerts(t *testing.T) {
	a := newTestAPI(t)
	for name, reference := range map[string]string{
		"an unknown withdrawal":             ids.New[ids.Withdrawal]().TypeID(),
		"a reference that is no wd_ TypeID": "foreign-" + newKey(),
		"a deposit's TypeID":                ids.New[ids.Deposit]().TypeID(),
	} {
		u := map[string]any{
			"provider_event_id": "pev_" + newKey(), "transfer_id": "tr_9", "reference": reference,
			"status": "executed", "occurred_at": "2026-10-24T09:00:00.000Z",
		}
		if status := a.postUpdate(t, u); status != 204 {
			t.Fatalf("%s: status %d", name, status)
		}
		row, found := updateRowOf(t, a.pool, u["provider_event_id"])
		if !found || row.outcome != "unmatched" || row.count != 1 || row.withdrawal != nil {
			t.Errorf("%s: row %+v", name, row)
		}
		line := a.updateLog(t, u["provider_event_id"])
		if line["level"] != "error" || line["alert"] != true || line["reference"] != reference {
			t.Errorf("%s: line %v", name, line)
		}
	}
}

func TestAnswers400RecordingNothing(t *testing.T) {
	a := newTestAPI(t)
	for name, overrides := range map[string]map[string]any{
		"failed without a reason": {"status": "failed"},
		"executed with a reason":  {"reason": "why"},
		"an unknown status":       {"status": "returned"},
		"no transfer_id":          {"transfer_id": nil},
		"an invalid occurred_at":  {"occurred_at": "yesterday"},
		"an offset occurred_at":   {"occurred_at": "2026-10-24T10:00:00+01:00"},
	} {
		w := a.newWithdrawal(t, true)
		u := update(w, overrides)
		if status := a.postUpdate(t, u); status != 400 {
			t.Errorf("%s: status %d", name, status)
		}
		if _, found := updateRowOf(t, a.pool, u["provider_event_id"]); found {
			t.Errorf("%s: recorded", name)
		}
		if s := storedWithdrawal(t, a.pool, w.id).Status; s != withdrawals.StatusSubmitted {
			t.Errorf("%s: status %s", name, s)
		}
	}
}

// The transfer-updates row of node/test/api/body-errors.test.ts: a webhook has one 4xx.
func TestAnswersBodyFramingErrorsWith400(t *testing.T) {
	a := newTestAPI(t)
	oversized, _ := json.Marshal(map[string]string{"padding": strings.Repeat("x", 1_100_000)})
	for _, tt := range []struct{ name, contentType, body, detail string }{
		{"malformed JSON", "application/json", `{"status": `,
			"Body is not valid JSON but content-type is set to 'application/json'"},
		{"a non-JSON content type", "text/plain", "executed", "Unsupported Media Type"},
		{"an oversized body", "application/json", string(oversized), "Request body is too large"},
	} {
		r := a.postRaw(t, transferUpdates, tt.contentType, tt.body, http.Header{})
		got := r.json(t)
		if r.status != 400 || got["type"] != "about:blank" || got["detail"] != tt.detail {
			t.Errorf("%s: %d %s", tt.name, r.status, r.body)
		}
	}
}

// An execute movement already exists for a withdrawal still submitted: the guards disagree.
func TestAnswers500ForAnInvariantViolationRollingEverythingBackAndAlerts(t *testing.T) {
	a := newTestAPI(t)
	w := a.newWithdrawal(t, true)
	_, err := db.InTxValue(t.Context(), a.pool, func(tx db.Tx) (ledger.MoveResult, error) {
		return ledger.Move(t.Context(), tx, ledger.Movement{
			Account: w.account.ID, From: ledger.BucketHeld, To: ledger.BucketExternal, Amount: 400,
			Ref: ledger.WithdrawalReference(w.id, ledger.StepExecute),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	before := balancesOf(t, a.pool, w.account)
	u := update(w, nil)
	if status := a.postUpdate(t, u); status != 500 {
		t.Fatalf("status %d", status)
	}
	if _, found := updateRowOf(t, a.pool, u["provider_event_id"]); found {
		t.Error("recorded despite the rollback")
	}
	if s := storedWithdrawal(t, a.pool, w.id).Status; s != withdrawals.StatusSubmitted {
		t.Errorf("status %s", s)
	}
	if b := balancesOf(t, a.pool, w.account); b != before {
		t.Errorf("balances %+v, before %+v", b, before)
	}
	line := a.updateLog(t, u["provider_event_id"])
	if line["level"] != "fatal" || line["alert"] != true ||
		line["msg"] != "invariant violated applying a transfer update" {
		t.Errorf("line %v", line)
	}
}
