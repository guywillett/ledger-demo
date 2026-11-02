package ids_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.jetify.com/typeid"

	"github.com/guywillett/ledger-demo/go/internal/ids"
)

func TestRoundTrip(t *testing.T) {
	id := ids.New[ids.Deposit]()
	if v := id.UUID().Version(); v != 7 {
		t.Fatalf("New() made a version %d UUID, want 7", v)
	}
	s := id.TypeID()
	if !strings.HasPrefix(s, "dep_") || len(s) != len("dep_")+26 {
		t.Fatalf("TypeID() = %q, want dep_ and 26 characters", s)
	}
	back, err := ids.FromTypeID[ids.Deposit](s)
	if err != nil {
		t.Fatalf("FromTypeID(%q) error: %v", s, err)
	}
	if back != id {
		t.Errorf("FromTypeID(TypeID()) = %v, want %v", back, id)
	}
}

// Each kind has its prefix.
func TestPrefixes(t *testing.T) {
	tests := []struct{ got, want string }{
		{ids.New[ids.Account]().TypeID(), "acct_"},
		{ids.New[ids.Deposit]().TypeID(), "dep_"},
		{ids.New[ids.Withdrawal]().TypeID(), "wd_"},
		{ids.New[ids.Event]().TypeID(), "evt_"},
		{ids.New[ids.Movement]().TypeID(), "mov_"},
	}
	for _, tt := range tests {
		if !strings.HasPrefix(tt.got, tt.want) {
			t.Errorf("TypeID() = %q, want prefix %q", tt.got, tt.want)
		}
	}
}

// The demo account alice, as seeded (db/migrations/…_seed_demo_accounts.sql) and as Node prints
// it: both implementations must agree on the encoding.
func TestMatchesNode(t *testing.T) {
	const typeID = "acct_01m564xr00fx7rjvqryn5dzwxx"
	const dbUUID = "01a14c4e-e000-7f4f-896e-f8f54adff3bd"
	id, err := ids.FromTypeID[ids.Account](typeID)
	if err != nil {
		t.Fatalf("FromTypeID error: %v", err)
	}
	if id.UUID().String() != dbUUID {
		t.Errorf("UUID() = %s, want %s", id.UUID(), dbUUID)
	}
	if id.TypeID() != typeID {
		t.Errorf("TypeID() = %s, want %s", id.TypeID(), typeID)
	}
}

func TestFromTypeIDRejects(t *testing.T) {
	v4 := uuid.New() // google/uuid's New is a random (version 4) UUID
	tests := []struct {
		name, input, want string
	}{
		{"another entity's prefix", ids.New[ids.Account]().TypeID(), `prefix "acct", want "dep"`},
		{"no prefix", strings.TrimPrefix(ids.New[ids.Deposit]().TypeID(), "dep_"), `prefix "", want "dep"`},
		{"a UUID that isn't v7", typeIDOf("dep", v4), "version 4"},
		{"not a TypeID", "dep_not-a-typeid", "invalid TypeID"},
		{"a raw UUID", ids.New[ids.Deposit]().UUID().String(), "invalid TypeID"},
		{"empty", "", "invalid TypeID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ids.FromTypeID[ids.Deposit](tt.input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("FromTypeID(%q) error = %v, want it to contain %q", tt.input, err, tt.want)
			}
		})
	}
}

// typeIDOf encodes any UUID as a TypeID, bypassing this package's checks to build bad input.
func typeIDOf(prefix string, u uuid.UUID) string {
	return typeid.Must(typeid.FromUUIDWithPrefix(prefix, u.String())).String()
}

// In JSON an ID is its TypeID, and decoding validates it.
func TestJSON(t *testing.T) {
	type body struct {
		Deposit ids.DepositID `json:"deposit_id"`
	}
	want := body{Deposit: ids.New[ids.Deposit]()}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(data) != `{"deposit_id":"`+want.Deposit.TypeID()+`"}` {
		t.Errorf("Marshal = %s", data)
	}
	var got body
	if err := json.Unmarshal(data, &got); err != nil || got != want {
		t.Errorf("Unmarshal = %+v, %v; want %+v", got, err, want)
	}
	wrong := `{"deposit_id":"` + ids.New[ids.Account]().TypeID() + `"}`
	if err := json.Unmarshal([]byte(wrong), &got); err == nil {
		t.Errorf("Unmarshal(%s) succeeded, want a prefix error", wrong)
	}
}

func TestIsZero(t *testing.T) {
	var zero ids.WithdrawalID
	if !zero.IsZero() || ids.New[ids.Withdrawal]().IsZero() {
		t.Error("IsZero: want true only for the zero value")
	}
}

// What the types are for: this must not compile, so it can't be a test that runs. Uncomment to
// see the error ("cannot use … (variable of type ids.AccountID) as ids.DepositID value").
//
//	var deposit ids.DepositID = ids.New[ids.Account]()
