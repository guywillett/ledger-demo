package events_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/events"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

func validEvent() events.DepositCreatedV1 {
	return events.DepositCreatedV1{
		EventID:              ids.New[ids.Event](),
		EventType:            events.SubjectDepositCreated,
		SchemaVersion:        1,
		DepositID:            ids.New[ids.Deposit](),
		Network:              "SEPA",
		NetworkTransactionID: "NTX-1",
		AccountID:            ids.New[ids.Account](),
		AmountMinor:          1250,
		Currency:             "EUR",
		OccurredAt:           timestamp.Time(time.Date(2026, 10, 31, 9, 21, 52, 130e6, time.UTC)),
	}
}

// What the producer writes (ingestion marshals the same struct) decodes back to the same event.
func TestDecodesWhatTheProducerWrites(t *testing.T) {
	want := validEvent()
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := events.DecodeDepositCreatedV1(body)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// Each field as Node's depositCreatedV1Schema checks it.
func TestRefusesWhatNodesSchemaRefuses(t *testing.T) {
	body, _ := json.Marshal(validEvent())
	valid := string(body)
	set := func(field, value string) string {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(valid), &m)
		if value == "" {
			delete(m, field)
		} else {
			m[field] = json.RawMessage(value)
		}
		out, _ := json.Marshal(m)
		return string(out)
	}
	tests := []struct {
		name, body, field string // field "" means valid
	}{
		{"an unknown field", set("extra", `{"x":1}`), ""},
		{"schema_version 1.0", set("schema_version", `1.0`), ""},
		{"a leap day", set("occurred_at", `"2028-02-29T00:00:00Z"`), ""},
		{"nine fraction digits", set("occurred_at", `"2026-10-31T09:21:52.123456789Z"`), ""},
		{"not JSON", "{", "not a JSON object"},
		{"null", "null", "not a JSON object"},
		{"an array", "[]", "not a JSON object"},
		{"no event_id", set("event_id", ""), "event_id"},
		{"a null network transaction ID", set("network_transaction_id", `null`), "network_transaction_id must be a string"},
		{"a null event_id", set("event_id", `null`), "event_id must be a string"},
		{"a dep TypeID as event_id", set("event_id", `"dep_01m6hcv5qgf0jbvtrsnm3m7m32"`), "event_id"},
		{"another event type", set("event_type", `"deposit.credited"`), "event_type"},
		{"schema_version 2", set("schema_version", `2`), "schema_version"},
		{"schema_version as a string", set("schema_version", `"1"`), "schema_version"},
		{"another network", set("network", `"SWIFT"`), "network"},
		{"an empty network transaction ID", set("network_transaction_id", `""`), "network_transaction_id"},
		{"a 256-character network transaction ID", set("network_transaction_id", `"`+strings.Repeat("n", 256)+`"`), "network_transaction_id"},
		{"an acct TypeID that's a deposit's", set("account_id", `"dep_01m6hcv5qgf0jbvtrsnm3m7m32"`), "account_id"},
		{"a number amount", set("amount_minor", `1250`), "amount_minor"},
		{"a zero amount", set("amount_minor", `"0"`), "amount_minor"},
		{"another currency", set("currency", `"USD"`), "currency"},
		{"an offset", set("occurred_at", `"2026-10-31T10:21:52+01:00"`), "occurred_at"},
		{"no seconds", set("occurred_at", `"2026-10-31T09:21Z"`), "occurred_at"},
		{"a 30 February", set("occurred_at", `"2026-02-30T00:00:00Z"`), "occurred_at"},
		{"a date only", set("occurred_at", `"2026-10-31"`), "occurred_at"},
	}
	for _, tt := range tests {
		_, err := events.DecodeDepositCreatedV1([]byte(tt.body))
		switch {
		case tt.field == "" && err != nil:
			t.Errorf("%s: refused: %v", tt.name, err)
		case tt.field != "" && (err == nil || !strings.Contains(err.Error(), tt.field)):
			t.Errorf("%s: err = %v, want one naming %s", tt.name, err, tt.field)
		}
	}
}

// withdrawal.held as the withdrawal request writes it decodes back; another event, or a field
// that fails Node's withdrawalHeldV1Schema, doesn't.
func TestDecodesWithdrawalHeldV1(t *testing.T) {
	want := events.WithdrawalHeldV1{
		EventID:       ids.New[ids.Event](),
		EventType:     events.SubjectWithdrawalHeld,
		SchemaVersion: 1,
		WithdrawalID:  ids.New[ids.Withdrawal](),
		AccountID:     ids.New[ids.Account](),
		AmountMinor:   400,
		Currency:      "EUR",
		OccurredAt:    timestamp.Time(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)),
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := events.DecodeWithdrawalHeldV1(body)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	for name, broken := range map[string]string{
		"a deposit.created":   strings.Replace(string(body), `"withdrawal.held"`, `"deposit.created"`, 1),
		"a dep withdrawal_id": strings.Replace(string(body), `"wd_`, `"dep_`, 1),
		"a number amount":     strings.Replace(string(body), `"400"`, `400`, 1),
		"not JSON":            "not json",
	} {
		if _, err := events.DecodeWithdrawalHeldV1([]byte(broken)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
