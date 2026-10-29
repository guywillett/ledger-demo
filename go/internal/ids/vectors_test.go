package ids_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/vectortest"
)

// The shared vectors (contracts/vectors/typeid.json): each text is decoded the way a request's
// TypeID field is, by encoding/json into the ID type for the case's prefix (UnmarshalText).
func TestVectors(t *testing.T) {
	vectortest.Run(t, "typeid.json", func(input json.RawMessage) (any, error) {
		var in struct {
			Prefix string          `json:"prefix"`
			Text   json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		switch in.Prefix {
		case "acct":
			return decode[ids.Account](in.Text)
		case "dep":
			return decode[ids.Deposit](in.Text)
		case "wd":
			return decode[ids.Withdrawal](in.Text)
		case "evt":
			return decode[ids.Event](in.Text)
		case "mov":
			return decode[ids.Movement](in.Text)
		}
		t.Fatalf("unknown prefix %q", in.Prefix)
		return nil, nil
	})
}

// decode is generic over the kind, as ids.ID is: decode[ids.Deposit] decodes into a DepositID.
// The result is the UUID, the vectors' value.
func decode[K ids.Kind](text json.RawMessage) (string, error) {
	var id ids.ID[K]
	if err := json.Unmarshal(text, &id); err != nil {
		return "", fmt.Errorf("decoding %s: %w", text, err)
	}
	return id.UUID().String(), nil
}
