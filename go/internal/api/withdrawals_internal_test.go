package api

import (
	"encoding/json"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/requests"
	"github.com/guywillett/ledger-demo/go/internal/vectortest"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// The shared vectors (contracts/vectors/withdrawal-same-details.json): both bodies decoded with the
// endpoint's real decoding (so an IBAN spelled two ways, or text with separators at its ends,
// compares equal), then compared with requests.DifferingFields, the rule a repeat with the same
// key is judged by.
func TestWithdrawalSameDetailsVectors(t *testing.T) {
	account := ids.New[ids.Account]()
	vectortest.Run(t, "withdrawal-same-details.json", func(input json.RawMessage) (any, error) {
		var pair struct{ A, B json.RawMessage }
		if err := json.Unmarshal(input, &pair); err != nil {
			return nil, err
		}
		details := func(body json.RawMessage) requests.Details {
			b, errs := decodeWithdrawalBody(body)
			if errs != nil {
				t.Fatalf("a vector body doesn't decode: %v", errs)
			}
			return requests.RequestDetails(withdrawals.NewWithdrawal{
				Account: account, IdempotencyKey: "key", Amount: b.amount,
				CreditorIBAN: b.creditorIBAN, CreditorName: b.creditorName,
				RemittanceInfo: b.remittanceInfo,
			})
		}
		differing := requests.DifferingFields(details(pair.A), details(pair.B))
		if differing == nil {
			differing = []requests.ComparedField{} // the vectors write none as []
		}
		return differing, nil
	})
}
