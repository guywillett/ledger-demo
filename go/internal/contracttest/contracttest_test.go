package contracttest_test

import (
	"net/http"
	"net/http/httptest"
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
