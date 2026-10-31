package contracttest_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/contracttest"
)

// The checker must refuse what the contract doesn't allow, or every API test passing it would
// prove nothing.

func response(status int, contentType, body string) *http.Response {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", contentType)
	rec.WriteHeader(status)
	_, _ = rec.WriteString(body)
	return rec.Result()
}

const balancePath = "/accounts/acct_01m564xr00fx7rjvqryn5dzwxx/balance"

func TestAcceptsAValidResponse(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, balancePath, nil)
	resp := response(200, "application/json; charset=utf-8",
		`{"pending_in":"0","available":"1000","held":"0","currency":"EUR"}`)
	defer func() { _ = resp.Body.Close() }()
	if problems := contracttest.Errors(t, req, resp); problems != "" {
		t.Errorf("a valid balance was refused:\n%s", problems)
	}
}

func TestRefuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"a number where the contract wants a digit string", 200,
			`{"pending_in":0,"available":"1000","held":"0","currency":"EUR"}`},
		{"a missing field", 200, `{"pending_in":"0","available":"1000","currency":"EUR"}`},
		{"another currency", 200, `{"pending_in":"0","available":"1000","held":"0","currency":"USD"}`},
		{"an undocumented status", 418, `{}`},
	}
	for _, tt := range tests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, balancePath, nil)
		resp := response(tt.status, "application/json; charset=utf-8", tt.body)
		if problems := contracttest.Errors(t, req, resp); problems == "" {
			t.Errorf("%s: accepted, want refused", tt.name)
		}
		_ = resp.Body.Close()
	}
}

// The event checker must refuse payloads the event contract doesn't allow.
func TestEvents(t *testing.T) {
	valid := `{"event_id":"evt_01m6hcv5qgf0jbvtrsnm3m7m31","event_type":"deposit.created","schema_version":1,` +
		`"deposit_id":"dep_01m6hcv5qgf0jbvtrsnm3m7m32","network":"SEPA","network_transaction_id":"NTX-1",` +
		`"account_id":"acct_01m564xr00fx7rjvqryn5dzwxx","amount_minor":"1250","currency":"EUR",` +
		`"occurred_at":"2026-10-30T09:21:52.130Z"}`
	if problems := contracttest.EventErrors(t, []byte(valid)); problems != "" {
		t.Errorf("a valid deposit.created was refused:\n%s", problems)
	}
	for name, payload := range map[string]string{
		"a number amount": strings.Replace(valid, `"1250"`, `1250`, 1),
		"not a date-time": strings.Replace(valid, `"2026-10-30T09:21:52.130Z"`, `"yesterday"`, 1),
		"a missing field": strings.Replace(valid, `"currency":"EUR",`, ``, 1),
		"a wrong prefix":  strings.Replace(valid, `"dep_`, `"acct_`, 1),
	} {
		if problems := contracttest.EventErrors(t, []byte(payload)); problems == "" {
			t.Errorf("%s: accepted, want refused", name)
		}
	}
	// Payloads that name no contract are refused with the reason, not a test failure.
	for name, tt := range map[string]struct{ payload, want string }{
		"a wrong event_type":      {strings.Replace(valid, `deposit.created`, `deposit.creatd`, 1), `no event contract for event_type "deposit.creatd", schema_version 1`},
		"an unknown version":      {strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), `no event contract for event_type "deposit.created", schema_version 2`},
		"a string version":        {strings.Replace(valid, `"schema_version":1`, `"schema_version":"1"`, 1), "schema_version is 1, not a whole number"},
		"a fractional version":    {strings.Replace(valid, `"schema_version":1`, `"schema_version":1.5`, 1), "schema_version is 1.5, not a whole number"},
		"no event_type":           {strings.Replace(valid, `"event_type":"deposit.created",`, ``, 1), "event_type is <nil>, not a string"},
		"an array, not an object": {`[1]`, "not a JSON object"},
	} {
		if got := contracttest.EventErrors(t, []byte(tt.payload)); got != tt.want {
			t.Errorf("%s: got %q, want %q", name, got, tt.want)
		}
	}
}
