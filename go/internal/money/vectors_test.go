package money_test

import (
	"encoding/json"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/vectortest"
)

// The shared vectors (contracts/vectors/money.json): each input is decoded the way a request's
// amount_minor field is, by encoding/json into an Amount (UnmarshalJSON).
func TestVectors(t *testing.T) {
	vectortest.Run(t, "money.json", func(input json.RawMessage) (any, error) {
		var a money.Amount
		err := json.Unmarshal(input, &a)
		return a, err
	})
}
