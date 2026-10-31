package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ingestion"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/vectortest"
)

// The shared vectors (contracts/vectors/deposit-same-details.json): both bodies decoded with the
// webhook's real decoding (so an IBAN spelled two ways compares equal), then compared with
// ingestion's DifferingFields, the rule the upsert's ON CONFLICT … WHERE applies.
func TestDepositSameDetailsVectors(t *testing.T) {
	vectortest.Run(t, "deposit-same-details.json", func(input json.RawMessage) (any, error) {
		var pair struct{ A, B json.RawMessage }
		if err := json.Unmarshal(input, &pair); err != nil {
			return nil, err
		}
		a, errsA := decodeNotification(pair.A)
		b, errsB := decodeNotification(pair.B)
		if errsA != nil || errsB != nil {
			t.Fatalf("a vector body doesn't decode: %v %v", errsA, errsB)
		}
		details := func(n ingestion.Notification) ingestion.Details {
			return ingestion.Details{CreditorIBAN: n.CreditorIBAN, Amount: n.Amount, Currency: n.Currency}
		}
		return ingestion.DifferingFields(details(a), details(b)), nil
	})
}

// The strict mode client bodies will use (step 7): unknown keys are problems, in the body's order,
// with Zod's strictObject messages.
func TestRejectUnknown(t *testing.T) {
	tests := []struct{ body, want string }{
		{`{"a":1}`, ""},
		{`{"a":1,"zeta":2}`, `Unrecognized key: "zeta"`},
		{`{"zeta":2,"a":1,"beta":3}`, `Unrecognized keys: "zeta", "beta"`},
	}
	for _, tt := range tests {
		f := newFields([]byte(tt.body))
		f.rejectUnknown("a")
		got := ""
		if len(f.errs) == 1 {
			got = f.errs[0].Message
		}
		if got != tt.want || len(f.errs) > 1 {
			t.Errorf("%s: errors %v, want %q", tt.body, f.errs, tt.want)
		}
	}
}

// A refused notification whose stored deposit differs in no compared field is our problem, not the
// provider's: 500 and an error line. The real database can't produce it, so record is replaced, as
// Node's test stubs the pool.
func TestInconsistentIs500(t *testing.T) {
	var logs bytes.Buffer
	logger, err := logging.New(&logs, "api", "info")
	if err != nil {
		t.Fatal(err)
	}
	stored := ids.New[ids.Deposit]()
	webhooks := webhookRoutes{record: func(context.Context, ingestion.Notification) (ingestion.Result, error) {
		return ingestion.Result{Kind: ingestion.Inconsistent, Deposit: stored}, nil
	}}
	handler := newHandler(nil, logger, webhooks)

	body := `{"network_transaction_id":"NTX-1","creditor_iban":"FR7612345000010000000000193",` +
		`"amount_minor":"1250","currency":"EUR","network":"SEPA"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/provider/incoming-payments", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if want := `{"type":"about:blank","title":"Internal Server Error","status":500}`; rec.Code != 500 || rec.Body.String() != want {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	var errorLines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil && m["level"] == "error" {
			errorLines = append(errorLines, m)
		}
	}
	if len(errorLines) != 1 || !strings.Contains(errorLines[0]["msg"].(string), "no field differs") ||
		errorLines[0]["creditor_iban"] != "FR76…0193" || errorLines[0]["alert"] != true ||
		errorLines[0]["deposit_id"] != stored.TypeID() {
		t.Errorf("error lines = %v", errorLines)
	}
}
