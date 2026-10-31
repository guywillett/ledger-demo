package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// Decoding events, for their consumers: hand-written validation into the typed event (slice 3
// decision 47), field by field as Node's event schemas (node/src/shared/events.ts) check them.
// Unknown fields are ignored (Node's z.object, not strictObject): a producer may add optional
// fields within a version. Keys match exactly, so the object is decoded into a map, not a struct
// (encoding/json's struct decoding matches keys case-insensitively).

// DecodeDepositCreatedV1 decodes and validates a deposit.created v1 message body: an error naming
// every field that fails Node's depositCreatedV1Schema, or the body if it isn't a JSON object.
func DecodeDepositCreatedV1(body []byte) (DepositCreatedV1, error) {
	f, err := newReader(body)
	if err != nil {
		return DepositCreatedV1{}, err
	}
	e := DepositCreatedV1{
		EventID:              readID[ids.Event](f, "event_id"),
		EventType:            f.literal("event_type", SubjectDepositCreated),
		SchemaVersion:        f.version("schema_version", 1),
		DepositID:            readID[ids.Deposit](f, "deposit_id"),
		Network:              f.literal("network", "SEPA"),
		NetworkTransactionID: f.stringLength("network_transaction_id", 1, 255),
		AccountID:            readID[ids.Account](f, "account_id"),
		AmountMinor:          f.amount("amount_minor"),
		Currency:             f.literal("currency", "EUR"),
		OccurredAt:           f.datetime("occurred_at"),
	}
	return e, f.err()
}

// DecodeWithdrawalHeldV1 decodes and validates a withdrawal.held v1 message body, as Node's
// withdrawalHeldV1Schema: IDs, the amount and the currency, no personal data.
func DecodeWithdrawalHeldV1(body []byte) (WithdrawalHeldV1, error) {
	f, err := newReader(body)
	if err != nil {
		return WithdrawalHeldV1{}, err
	}
	e := WithdrawalHeldV1{
		EventID:       readID[ids.Event](f, "event_id"),
		EventType:     f.literal("event_type", SubjectWithdrawalHeld),
		SchemaVersion: f.version("schema_version", 1),
		WithdrawalID:  readID[ids.Withdrawal](f, "withdrawal_id"),
		AccountID:     readID[ids.Account](f, "account_id"),
		AmountMinor:   f.amount("amount_minor"),
		Currency:      f.literal("currency", "EUR"),
		OccurredAt:    f.datetime("occurred_at"),
	}
	return e, f.err()
}

// reader reads a JSON object's fields, collecting a problem per field that fails.
type reader struct {
	object map[string]json.RawMessage
	errs   []error
}

func newReader(body []byte) (*reader, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil { // nil: the JSON null
		return nil, errors.New("not a JSON object")
	}
	return &reader{object: object}, nil
}

func (r *reader) fail(name, problem string) {
	r.errs = append(r.errs, fmt.Errorf("%s %s", name, problem))
}

func (r *reader) err() error { return errors.Join(r.errs...) }

// string reads a field that must be a JSON string. Its kind is checked first: json.Unmarshal of a
// JSON null into a string leaves it "" and returns no error, and null is no string (Zod: "expected
// string, received null").
func (r *reader) string(name string) (string, bool) {
	var s string
	raw, present := r.object[name]
	if !present || !isJSONString(raw) || json.Unmarshal(raw, &s) != nil {
		r.fail(name, "must be a string")
		return "", false
	}
	return s, true
}

// isJSONString reports whether raw is a JSON string (a value from the decoded object, so it begins
// with its first character).
func isJSONString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
}

// literal reads a field that must be exactly want (Zod's z.literal).
func (r *reader) literal(name, want string) string {
	var s string
	if raw, present := r.object[name]; !present || json.Unmarshal(raw, &s) != nil || s != want {
		r.fail(name, fmt.Sprintf("must be %q", want))
		return ""
	}
	return s
}

// version reads a field that must be the number want (z.literal(1)): any JSON number equal to it,
// 1.0 and 1e0 included, as JavaScript's === compares the parsed value. Into a float64, which a
// JSON string can't fill (a json.Number would also accept "1", a string).
func (r *reader) version(name string, want int) int {
	var f float64
	raw, present := r.object[name]
	if present && json.Unmarshal(raw, &f) == nil && f == float64(want) {
		return want
	}
	r.fail(name, fmt.Sprintf("must be %d", want))
	return 0
}

// stringLength reads a string of minLen to maxLen characters, counted as Zod counts them (code
// points).
func (r *reader) stringLength(name string, minLen, maxLen int) string {
	s, ok := r.string(name)
	if !ok {
		return ""
	}
	if n := utf8.RuneCountInString(s); n < minLen || n > maxLen {
		r.fail(name, fmt.Sprintf("must have %d to %d characters", minLen, maxLen))
		return ""
	}
	return s
}

// readID reads a TypeID of kind K (Node's typeIdSchema: well formed, K's prefix, a UUIDv7). A
// function, not a method: Go methods can't have type parameters of their own.
func readID[K ids.Kind](r *reader, name string) ids.ID[K] {
	s, ok := r.string(name)
	if !ok {
		return ids.ID[K]{}
	}
	id, err := ids.FromTypeID[K](s)
	if err != nil {
		r.fail(name, err.Error())
	}
	return id
}

// amount reads an amount (Node's amountMinorSchema: a string of digits; money.ParseAmount).
func (r *reader) amount(name string) money.Amount {
	s, ok := r.string(name)
	if !ok {
		return 0
	}
	a, err := money.ParseAmount(s)
	if err != nil {
		r.fail(name, err.Error())
	}
	return a
}

// datetime reads an ISO 8601 UTC timestamp as Node's schemas accept one (timestamp.ParseISO).
func (r *reader) datetime(name string) timestamp.Time {
	s, ok := r.string(name)
	if !ok {
		return timestamp.Time{}
	}
	t, ok := timestamp.ParseISO(s)
	if !ok {
		r.fail(name, "must be an ISO 8601 UTC timestamp")
		return timestamp.Time{}
	}
	return timestamp.Time(t)
}
