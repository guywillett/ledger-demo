package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/contracttest"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// POST /provider/incoming-payments: the cases of node/test/api/incoming-payments.test.ts, the
// outbox half of deposit-events.test.ts (the relay half comes with the relay, step 5), and the
// webhook rows of body-errors.test.ts. Every notification it can parse gets 204 with no body,
// whatever it did, so each test asserts the recorded effects. Every response is checked against the
// contract (postNotification), and every outbox payload against its event contract.

func TestRecordsADepositAsCreatedWithItsEvent(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"provider_account_id": "prov_acc_0001"})
	r := a.postNotification(t, body, nil)
	if r.status != 204 || r.body != "" {
		t.Fatalf("got %d %q, want 204 and no body", r.status, r.body)
	}
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if *d.AccountID != seedIDs[aliceIBAN] || d.Status != "created" || d.CreditorIBAN != aliceIBAN ||
		*d.ProviderAccountID != "prov_acc_0001" || d.Amount != 1250 || d.NotificationCount != 1 {
		t.Errorf("deposit = %+v", d)
	}
	if n := historyCount(t, a.pool, d.ID); n != 1 {
		t.Errorf("%d history rows, want 1", n)
	}
	if n := outboxCount(t, a.pool, d.ID); n != 1 {
		t.Errorf("%d outbox rows, want 1", n)
	}
	line := a.logLine(t, body["network_transaction_id"], "deposit created")
	if line["level"] != "info" || !strings.HasPrefix(line["deposit_id"].(string), "dep_") {
		t.Errorf("log line = %v", line)
	}
}

func TestAcceptsTheContractsRequestExample(t *testing.T) {
	a := newTestAPI(t)
	example := contracttest.RequestExample(t, http.MethodPost, incomingPayments)
	example["network_transaction_id"] = newNetworkTransactionID()
	if r := a.postNotification(t, example, nil); r.status != 204 {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
	if d := mustDeposit(t, a.pool, example["network_transaction_id"]); d.Status != "created" {
		t.Errorf("status = %s", d.Status)
	}
}

func TestNormalisesTheIBANBeforeMatching(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"creditor_iban": "fr76 1234 5000 0100 0000 0000 290"})
	a.postNotification(t, body, nil)
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if *d.AccountID != seedIDs[bobIBAN] || d.CreditorIBAN != bobIBAN {
		t.Errorf("deposit = %+v", d)
	}
}

func TestWorksWithoutAProviderAccountID(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"creditor_iban": carolIBAN})
	a.postNotification(t, body, nil)
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if *d.AccountID != seedIDs[carolIBAN] || d.ProviderAccountID != nil {
		t.Errorf("deposit = %+v", d)
	}
}

func TestKeepsAmountsBeyond2To53Exact(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"amount_minor": "9007199254740993"})
	a.postNotification(t, body, nil)
	if d := mustDeposit(t, a.pool, body["network_transaction_id"]); d.Amount != 9007199254740993 {
		t.Errorf("amount = %d", d.Amount)
	}
}

func TestIgnoresUnknownFields(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"payer_name": "Jane Doe"})
	if r := a.postNotification(t, body, nil); r.status != 204 {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
	if strings.Contains(a.logs.String(), "Jane") {
		t.Error("an unknown field reached the log")
	}
}

func TestRecordsTheSameNotificationOnceCountingRepeats(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	for range 5 {
		if r := a.postNotification(t, body, nil); r.status != 204 {
			t.Fatalf("status = %d", r.status)
		}
	}
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if d.NotificationCount != 5 || historyCount(t, a.pool, d.ID) != 1 || outboxCount(t, a.pool, d.ID) != 1 {
		t.Errorf("deposit %+v, history %d, outbox %d", d, historyCount(t, a.pool, d.ID), outboxCount(t, a.pool, d.ID))
	}
	if line := a.logLine(t, body["network_transaction_id"], "duplicate deposit notification"); line["level"] != "info" ||
		line["duplicate"] != true || line["notification_count"] != float64(5) {
		t.Errorf("log line = %v", line)
	}
}

// Ten identical notifications at once, from ten goroutines released together: one deposit.
func TestRecordsConcurrentIdenticalNotificationsOnce(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	const n = 10
	statuses := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			statuses[i] = a.postNotification(t, body, nil).status
		})
	}
	close(start)
	wg.Wait()
	for _, s := range statuses {
		if s != 204 {
			t.Errorf("statuses = %v, want all 204", statuses)
			break
		}
	}
	ntx := body["network_transaction_id"]
	if count := countOf(t, a.pool, `SELECT count(*)::int FROM deposits WHERE network_transaction_id = $1`, ntx); count != 1 {
		t.Errorf("%d deposits, want 1", count)
	}
	d := mustDeposit(t, a.pool, ntx)
	if d.NotificationCount != n || historyCount(t, a.pool, d.ID) != 1 {
		t.Errorf("notification_count %d, history %d", d.NotificationCount, historyCount(t, a.pool, d.ID))
	}
}

func TestADuplicateKeepsTheFirstProviderAccountID(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"provider_account_id": "prov_acc_0001"})
	a.postNotification(t, body, nil)
	without := notificationBody(map[string]any{"network_transaction_id": body["network_transaction_id"]})
	a.postNotification(t, without, nil)
	other := notificationBody(map[string]any{
		"network_transaction_id": body["network_transaction_id"], "provider_account_id": "something-else",
	})
	a.postNotification(t, other, nil)
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if *d.ProviderAccountID != "prov_acc_0001" || d.NotificationCount != 3 {
		t.Errorf("deposit = %+v", d)
	}
}

func TestTreatsFormattingDifferencesAsTheSameDetails(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	a.postNotification(t, body, nil)
	reformatted := `{ "network": "SEPA", "currency": "EUR",
      "creditor_iban": "fr76 1234 5000 0100 0000 0000 193",
      "amount_minor": "1250", "network_transaction_id": "` + body["network_transaction_id"].(string) + `" }`
	if r := a.postNotification(t, reformatted, nil); r.status != 204 {
		t.Fatalf("status = %d", r.status)
	}
	if d := mustDeposit(t, a.pool, body["network_transaction_id"]); d.NotificationCount != 2 {
		t.Errorf("notification_count = %d", d.NotificationCount)
	}
}

func TestRecordsAConflictChangingNothing(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
		differing []string
	}{
		{"a different amount", map[string]any{"amount_minor": "1251"}, []string{"amount_minor"}},
		{"a different IBAN of ours", map[string]any{"creditor_iban": bobIBAN}, []string{"creditor_iban"}},
		{"an IBAN that is not ours", map[string]any{"creditor_iban": foreignIBAN}, []string{"creditor_iban"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAPI(t)
			body := notificationBody(nil)
			a.postNotification(t, body, nil)
			before := mustDeposit(t, a.pool, body["network_transaction_id"])

			again := notificationBody(tt.overrides)
			again["network_transaction_id"] = body["network_transaction_id"]
			if r := a.postNotification(t, again, nil); r.status != 204 {
				t.Fatalf("status = %d", r.status)
			}
			// reflect.DeepEqual follows the pointer fields (== would compare their addresses).
			if after := mustDeposit(t, a.pool, body["network_transaction_id"]); !reflect.DeepEqual(after, before) {
				t.Errorf("deposit changed:\n%+v\nwas\n%+v", after, before)
			}
			conflicts := conflictsOf(t, a.pool, before.ID)
			if len(conflicts) != 1 || !slices.Equal(conflicts[0].DifferingFields, tt.differing) {
				t.Errorf("conflicts = %+v", conflicts)
			}
			line := a.logLine(t, body["network_transaction_id"],
				"deposit notification conflicts with the stored deposit; recorded, nothing changed")
			fields, _ := json.Marshal(line["differing_fields"])
			want, _ := json.Marshal(tt.differing)
			if line["level"] != "error" || line["alert"] != true || string(fields) != string(want) {
				t.Errorf("log line = %v", line)
			}
			// Field names, never the values.
			if raw, _ := json.Marshal(line); strings.Contains(string(raw), "1251") {
				t.Error("the log line holds a value")
			}
		})
	}
}

// Five notifications with one network ID and five amounts, at once: one deposit, four conflicts.
func TestExactlyOneOfConcurrentConflictingNotificationsCreates(t *testing.T) {
	a := newTestAPI(t)
	ntx := newNetworkTransactionID()
	amounts := []string{"100", "200", "300", "400", "500"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, len(amounts))
	for i, amount := range amounts {
		wg.Go(func() {
			<-start
			body := notificationBody(map[string]any{"network_transaction_id": ntx, "amount_minor": amount})
			statuses[i] = a.postNotification(t, body, nil).status
		})
	}
	close(start)
	wg.Wait()
	if !slices.Equal(statuses, []int{204, 204, 204, 204, 204}) {
		t.Errorf("statuses = %v", statuses)
	}
	d := mustDeposit(t, a.pool, ntx)
	conflicts := conflictsOf(t, a.pool, d.ID)
	if len(conflicts) != 4 {
		t.Fatalf("%d conflicts, want 4", len(conflicts))
	}
	all := []string{strconv.FormatInt(d.Amount, 10)}
	for _, c := range conflicts {
		if !slices.Equal(c.DifferingFields, []string{"amount_minor"}) {
			t.Errorf("conflict differs in %v", c.DifferingFields)
		}
		all = append(all, strconv.FormatInt(c.Amount, 10))
	}
	slices.Sort(all)
	if !slices.Equal(all, amounts) {
		t.Errorf("amounts = %v, want %v", all, amounts)
	}
}

func TestRecordsAnUnknownIBANInSuspense(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"creditor_iban": foreignIBAN})
	if r := a.postNotification(t, body, nil); r.status != 204 {
		t.Fatalf("status = %d", r.status)
	}
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if d.Status != "unmatched" || d.AccountID != nil || d.NotificationCount != 1 {
		t.Errorf("deposit = %+v", d)
	}
	if historyCount(t, a.pool, d.ID) != 1 || outboxCount(t, a.pool, d.ID) != 0 {
		t.Errorf("history %d, outbox %d; want 1 and 0", historyCount(t, a.pool, d.ID), outboxCount(t, a.pool, d.ID))
	}
	if n := countOf(t, a.pool, `SELECT count(*)::int FROM balance_movements WHERE reference_id = $1`, d.ID); n != 0 {
		t.Errorf("%d movements, want 0", n)
	}
	line := a.logLine(t, body["network_transaction_id"],
		"deposit notification matches none of our accounts; recorded in suspense")
	if line["level"] != "error" || line["alert"] != true || line["creditor_iban"] != "DE75\u20266199" {
		t.Errorf("log line = %v", line)
	}
	// A repeat is a duplicate of the suspense record.
	a.postNotification(t, body, nil)
	if d := mustDeposit(t, a.pool, body["network_transaction_id"]); d.Status != "unmatched" || d.NotificationCount != 2 {
		t.Errorf("after a repeat: %+v", d)
	}
}

// Resolving suspense is never automatic: an account created later with the IBAN doesn't match it.
func TestStaysUnmatchedAfterAnAccountWithThatIBANIsCreated(t *testing.T) {
	a := newTestAPI(t)
	iban := newTestIBAN()
	body := notificationBody(map[string]any{"creditor_iban": iban})
	a.postNotification(t, body, nil)
	if _, err := a.pool.Exec(context.Background(), `INSERT INTO accounts (id, name, iban) VALUES ($1, $2, $3)`,
		ids.New[ids.Account](), "late-"+iban, iban); err != nil {
		t.Fatal(err)
	}
	a.postNotification(t, body, nil)
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	if d.Status != "unmatched" || d.AccountID != nil || d.NotificationCount != 2 {
		t.Errorf("deposit = %+v", d)
	}
}

func TestAnswers400ForAnInvalidNotificationRecordingNothing(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
		path      string
	}{
		{"a bad IBAN checksum", map[string]any{"creditor_iban": "FR7612345000010000000000194"}, "creditor_iban"},
		{"a non-SEPA IBAN", map[string]any{"creditor_iban": "BR1800360305000010009795493C1"}, "creditor_iban"},
		{"a decimal amount", map[string]any{"amount_minor": "12.50"}, "amount_minor"},
		{"a numeric amount", map[string]any{"amount_minor": 1250}, "amount_minor"},
		{"a zero amount", map[string]any{"amount_minor": "0"}, "amount_minor"},
		{"a leading zero", map[string]any{"amount_minor": "0125"}, "amount_minor"},
		{"an amount above int8", map[string]any{"amount_minor": "9223372036854775808"}, "amount_minor"},
		{"another currency", map[string]any{"currency": "USD"}, "currency"},
		{"another network", map[string]any{"network": "SWIFT"}, "network"},
		{"an empty network transaction ID", map[string]any{"network_transaction_id": ""}, "network_transaction_id"},
		{"a missing IBAN", map[string]any{"creditor_iban": nil}, "creditor_iban"},
	}
	a := newTestAPI(t)
	for _, tt := range tests {
		body := notificationBody(tt.overrides)
		r := a.postNotification(t, body, nil)
		got := r.json(t)
		errs, _ := got["errors"].([]any)
		if r.status != 400 || got["type"] != "urn:ledger-demo:problem:invalid-notification" ||
			len(errs) != 1 || errs[0].(map[string]any)["path"] != tt.path {
			t.Errorf("%s: got %d %s", tt.name, r.status, r.body)
		}
		if _, ok := depositByNetworkID(t, a.pool, body["network_transaction_id"]); ok {
			t.Errorf("%s: a deposit was stored", tt.name)
		}
	}
}

// Every body case measured against Node's API (its server, case by case): which are framing errors
// (about:blank, Fastify's detail) and which reach the schema (invalid-notification with errors), all
// 400 on this webhook.
func TestAnswersBodyProblemsAsNodeDoes(t *testing.T) {
	valid, _ := json.Marshal(notificationBody(map[string]any{"creditor_iban": "FR7612345000010000000000194"}))
	bad := string(valid) // reaches the schema: a checksum error
	withKey := func(extra string) string { return bad[:len(bad)-1] + "," + extra + "}" }
	notJSON := "Body is not valid JSON but content-type is set to 'application/json'"
	tests := []struct {
		name        string
		body        string
		contentType []string // nil: no Content-Type header
		detail      string   // about:blank's detail; "" means invalid-notification
		errorPaths  []any    // invalid-notification's errors' paths
	}{
		{"no content-type", bad, nil, "Unsupported Media Type", nil},
		{"text/plain", bad, []string{"text/plain"}, "Unsupported Media Type", nil},
		{"application/xml", "<deposit/>", []string{"application/xml"}, "Unsupported Media Type", nil},
		{"application/vnd.api+json", bad, []string{"application/vnd.api+json"}, "Unsupported Media Type", nil},
		{"empty, text/plain", "", []string{"text/plain"}, "Unsupported Media Type", nil},
		{"charset=utf-8", bad, []string{"application/json; charset=utf-8"}, "", []any{"creditor_iban"}},
		{"charset=latin1", bad, []string{"application/json; charset=latin1"}, "", []any{"creditor_iban"}},
		{"upper case type", bad, []string{"APPLICATION/JSON"}, "", []any{"creditor_iban"}},
		{"empty, json", "", []string{"application/json"}, "Body cannot be empty when content-type is set to 'application/json'", nil},
		{"empty, no content-type", "", nil, "", []any{""}},
		{"whitespace", "   ", []string{"application/json"}, notJSON, nil},
		{"malformed", `{"network_transaction_id": `, []string{"application/json"}, notJSON, nil},
		{"trailing data", bad + "x", []string{"application/json"}, notJSON, nil},
		{"a byte-order mark first", "\ufeff" + bad, []string{"application/json"}, "", []any{"creditor_iban"}},
		{"an array", "[]", []string{"application/json"}, "", []any{""}},
		{"a string", `"x"`, []string{"application/json"}, "", []any{""}},
		{"null", "null", []string{"application/json"}, "", []any{""}},
		{"a __proto__ key", withKey(`"__proto__":{"x":1}`), []string{"application/json"}, notJSON, nil},
		{"an escaped __proto__ key", withKey(`"\u005f_proto__":{}`), []string{"application/json"}, notJSON, nil},
		{"a nested __proto__ key", withKey(`"extra":{"a":{"__proto__":{}}}`), []string{"application/json"}, notJSON, nil},
		{"constructor.prototype", withKey(`"extra":[{"constructor":{"prototype":null}}]`), []string{"application/json"}, notJSON, nil},
		{"__proto__ as a value", withKey(`"extra":"__proto__"`), []string{"application/json"}, "", []any{"creditor_iban"}},
		{"a constructor without prototype", withKey(`"constructor":1`), []string{"application/json"}, "", []any{"creditor_iban"}},
		// Numbers JavaScript's JSON.parse reads as Infinity (or 0) are still JSON, wherever they are;
		// numbers that aren't JSON aren't.
		{"an extra 1e400", withKey(`"fee":1e400`), []string{"application/json"}, "", []any{"creditor_iban"}},
		{"an extra -1e400", withKey(`"fee":-1e400`), []string{"application/json"}, "", []any{"creditor_iban"}},
		{"an extra 1e-400", withKey(`"fee":1e-400`), []string{"application/json"}, "", []any{"creditor_iban"}},
		{"an extra 400-digit integer", withKey(`"fee":1` + strings.Repeat("0", 400)), []string{"application/json"}, "", []any{"creditor_iban"}},
		{"a nested 1e400", withKey(`"x":{"y":[1e400]}`), []string{"application/json"}, "", []any{"creditor_iban"}},
		{"a leading zero", withKey(`"fee":01`), []string{"application/json"}, notJSON, nil},
		{"a trailing point", withKey(`"fee":1.`), []string{"application/json"}, notJSON, nil},
		{"over 1 MiB", withKey(`"pad":"` + strings.Repeat("x", 1100000) + `"`), []string{"application/json"},
			"Request body is too large", nil},
	}
	a := newTestAPI(t)
	for _, tt := range tests {
		header := http.Header{"Content-Type": tt.contentType}
		if tt.contentType == nil {
			header = http.Header{"Content-Type": nil}
		}
		r := a.postFramed(t, tt.body, header)
		got := r.json(t)
		if r.status != 400 {
			t.Errorf("%s: status %d", tt.name, r.status)
			continue
		}
		if tt.detail != "" {
			if got["type"] != "about:blank" || got["detail"] != tt.detail {
				t.Errorf("%s: %s\nwant about:blank with %q", tt.name, r.body, tt.detail)
			}
			continue
		}
		var paths []any
		errs, _ := got["errors"].([]any)
		for _, e := range errs {
			paths = append(paths, e.(map[string]any)["path"])
		}
		if got["type"] != "urn:ledger-demo:problem:invalid-notification" || !slices.Equal(paths, tt.errorPaths) {
			t.Errorf("%s: %s\nwant invalid-notification at %v", tt.name, r.body, tt.errorPaths)
		}
	}
}

// Zod's messages and paths for the fields, as Node answers them (measured).
func TestReportsFieldProblemsAsZodDoes(t *testing.T) {
	type issue struct{ path, message string }
	tests := []struct {
		name      string
		overrides map[string]any
		want      []issue
	}{
		{"every field missing", map[string]any{
			"network_transaction_id": nil, "creditor_iban": nil, "amount_minor": nil, "currency": nil, "network": nil,
		}, []issue{
			{"network_transaction_id", "Invalid input: expected string, received undefined"},
			{"creditor_iban", "Invalid input: expected string, received undefined"},
			{"amount_minor", "Invalid input: expected string, received undefined"},
			{"currency", `Invalid input: expected "EUR"`},
			{"network", `Invalid input: expected "SEPA"`},
		}},
		{"a 20-character amount of letters", map[string]any{"amount_minor": strings.Repeat("x", 20)}, []issue{
			{"amount_minor", "Too big: expected string to have <=19 characters"},
			{"amount_minor", "must be a positive whole number of minor units, as a string"},
		}},
		{"a 20-digit amount", map[string]any{"amount_minor": strings.Repeat("1", 20)}, []issue{
			{"amount_minor", "Too big: expected string to have <=19 characters"},
		}},
		{"an amount above int8", map[string]any{"amount_minor": "9223372036854775808"}, []issue{
			{"amount_minor", "is larger than the maximum amount"},
		}},
		{"a boolean amount", map[string]any{"amount_minor": true}, []issue{
			{"amount_minor", "Invalid input: expected string, received boolean"},
		}},
		// A number beyond a float64 is Infinity to JavaScript, and Zod names it so.
		{"a 1e400 amount", map[string]any{"amount_minor": json.RawMessage("1e400")}, []issue{
			{"amount_minor", "Invalid input: expected string, received Infinity"},
		}},
		{"a -1e400 amount", map[string]any{"amount_minor": json.RawMessage("-1e400")}, []issue{
			{"amount_minor", "Invalid input: expected string, received -Infinity"},
		}},
		{"a 1.8e308 amount", map[string]any{"amount_minor": json.RawMessage("1.8e308")}, []issue{
			{"amount_minor", "Invalid input: expected string, received Infinity"},
		}},
		{"a 1e308 amount", map[string]any{"amount_minor": json.RawMessage("1e308")}, []issue{
			{"amount_minor", "Invalid input: expected string, received number"},
		}},
		{"a 1e-400 amount", map[string]any{"amount_minor": json.RawMessage("1e-400")}, []issue{
			{"amount_minor", "Invalid input: expected string, received number"},
		}},
		{"a 1e400 network transaction ID", map[string]any{"network_transaction_id": json.RawMessage("1e400")}, []issue{
			{"network_transaction_id", "Invalid input: expected string, received Infinity"},
		}},
		{"a 1e400 currency", map[string]any{"currency": json.RawMessage("1e400")}, []issue{
			{"currency", `Invalid input: expected "EUR"`},
		}},
		{"a null IBAN", map[string]any{"creditor_iban": nil}, nil}, // replaced below: null, not absent
		{"a 65-character IBAN", map[string]any{"creditor_iban": strings.Repeat("x", 65)}, []issue{
			{"creditor_iban", "Too big: expected string to have <=64 characters"},
		}},
		{"a short IBAN", map[string]any{"creditor_iban": "FR76 1234"}, []issue{
			{"creditor_iban", "invalid IBAN: wrong length for its country"},
		}},
		{"a 256-character provider account ID", map[string]any{"provider_account_id": strings.Repeat("p", 256)}, []issue{
			{"provider_account_id", "Too big: expected string to have <=255 characters"},
		}},
		{"an empty provider account ID", map[string]any{"provider_account_id": ""}, []issue{
			{"provider_account_id", "Too small: expected string to have >=1 characters"},
		}},
		{"a 255-character network transaction ID", map[string]any{"network_transaction_id": strings.Repeat("n", 255)}, nil},
	}
	a := newTestAPI(t)
	for _, tt := range tests {
		body := notificationBody(tt.overrides)
		if tt.name == "a null IBAN" {
			body["creditor_iban"] = json.RawMessage("null") // present, and null
			tt.want = []issue{{"creditor_iban", "Invalid input: expected string, received null"}}
		}
		if tt.want == nil {
			body["creditor_iban"] = foreignIBAN // valid: let it through to a 204
			if r := a.postNotification(t, body, nil); r.status != 204 {
				t.Errorf("%s: got %d %s, want 204", tt.name, r.status, r.body)
			}
			continue
		}
		r := a.postNotification(t, body, nil)
		var got []issue
		errs, _ := r.json(t)["errors"].([]any)
		for _, e := range errs {
			m := e.(map[string]any)
			got = append(got, issue{m["path"].(string), m["message"].(string)})
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s:\ngot  %v\nwant %v", tt.name, got, tt.want)
		}
	}
}

// A null optional field is a problem, as Zod's .optional() (absent is fine).
func TestANullProviderAccountIDIsInvalid(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	body["provider_account_id"] = json.RawMessage("null")
	r := a.postNotification(t, body, nil)
	errs, _ := r.json(t)["errors"].([]any)
	if r.status != 400 || len(errs) != 1 ||
		errs[0].(map[string]any)["message"] != "Invalid input: expected string, received null" {
		t.Errorf("got %d %s", r.status, r.body)
	}
}

// Keys match exactly (not encoding/json's case-insensitive struct matching); a key given twice is
// its last value, as in JavaScript.
func TestKeysMatchExactlyAndTheLastDuplicateWins(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	encoded, _ := json.Marshal(body)
	renamed := strings.Replace(string(encoded), `"amount_minor"`, `"Amount_minor"`, 1)
	r := a.postNotification(t, renamed, nil)
	errs, _ := r.json(t)["errors"].([]any)
	if r.status != 400 || len(errs) != 1 || errs[0].(map[string]any)["path"] != "amount_minor" {
		t.Errorf("Amount_minor: got %d %s, want amount_minor missing", r.status, r.body)
	}

	twice := strings.Replace(string(encoded), `"amount_minor":"1250"`, `"amount_minor":"zero","amount_minor":"1250"`, 1)
	if r := a.postNotification(t, twice, nil); r.status != 204 {
		t.Errorf("duplicate key, last valid: got %d %s, want 204", r.status, r.body)
	}
}

// The webhook's log lines: each outcome at its level, IBANs masked, never the body; the access line
// with the path only.
func TestLogsEachOutcomeAndNeverTheBody(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(map[string]any{"payer_name": "Jane Doe"})
	a.postNotification(t, body, nil) // created
	a.postNotification(t, body, nil) // duplicate
	conflicting := notificationBody(map[string]any{"amount_minor": "1"})
	conflicting["network_transaction_id"] = body["network_transaction_id"]
	a.postNotification(t, conflicting, nil)                                                    // conflict
	a.postNotification(t, notificationBody(map[string]any{"creditor_iban": foreignIBAN}), nil) // unmatched
	a.postNotification(t, notificationBody(map[string]any{"amount_minor": "lots"}), nil)       // 400

	var domain [][2]any
	var access []any
	for _, line := range a.logLines(t) {
		if line["msg"] == "request completed" {
			access = append(access, line["status_code"])
			if line["path"] != incomingPayments {
				t.Errorf("access path = %v", line["path"])
			}
			continue
		}
		domain = append(domain, [2]any{line["level"], line["msg"]})
	}
	want := [][2]any{
		{"info", "deposit created"},
		{"info", "duplicate deposit notification"},
		{"error", "deposit notification conflicts with the stored deposit; recorded, nothing changed"},
		{"error", "deposit notification matches none of our accounts; recorded in suspense"},
		{"warn", "invalid deposit notification"},
	}
	if !slices.Equal(domain, want) {
		t.Errorf("log lines = %v\nwant       %v", domain, want)
	}
	if !slices.Equal(access, []any{204.0, 204.0, 204.0, 204.0, 400.0}) {
		t.Errorf("access statuses = %v", access)
	}
	created := a.logLine(t, body["network_transaction_id"], "deposit created")
	if created["creditor_iban"] != "FR76\u20260193" || created["amount_minor"] != "1250" ||
		!strings.HasPrefix(created["event_id"].(string), "evt_") {
		t.Errorf("created line = %v", created)
	}
	everything := a.logs.String()
	for _, secret := range []string{"Jane Doe", aliceIBAN, foreignIBAN} {
		if strings.Contains(everything, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

func TestAnswersAFramingErrorWithRequestRejected(t *testing.T) {
	a := newTestAPI(t)
	a.postFramed(t, "hello", http.Header{"Content-Type": {"text/plain"}})
	for _, line := range a.logLines(t) {
		if line["msg"] == "request rejected" {
			if line["level"] != "warn" || line["status_code"] != 400.0 || line["reason"] != "Unsupported Media Type" {
				t.Errorf("line = %v", line)
			}
			return
		}
	}
	t.Error("no request rejected line")
}

// A body that can't be read (here, the connection fails mid-body; or the request deadline) is no
// fault of the body's: 500, so the provider retries, and an error line, not a 400.
func TestAnswers500WhenTheBodyCantBeRead(t *testing.T) {
	a := newTestAPI(t)
	body := io.MultiReader(strings.NewReader(`{"network_transaction_id":`), failingReader{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, incomingPayments, body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	if want := `{"type":"about:blank","title":"Internal Server Error","status":500}`; rec.Code != 500 || rec.Body.String() != want {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	contracttest.Check(t, req, rec.Result())
	found := false
	for _, line := range a.logLines(t) {
		// An error is logged as {message, type}, as Node's logger serialises one.
		err, _ := line["err"].(map[string]any)
		if line["msg"] == "unexpected error" && line["level"] == "error" &&
			err["message"] == "reading the request body: connection reset by peer" {
			found = true
		}
		if line["msg"] == "request rejected" {
			t.Errorf("logged as a rejected body: %v", line)
		}
	}
	if !found {
		t.Errorf("no unexpected error line in %v", a.logLines(t))
	}
}

// failingReader fails every read, as a dropped connection does.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// deposit.created in the outbox: stored once, with the deposit, in its transaction, with the payload
// Node writes, matching the event contract.
func TestStoresDepositCreatedInTheDepositsTransaction(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	a.postNotification(t, body, nil)
	d := mustDeposit(t, a.pool, body["network_transaction_id"])
	depositID := idFromUUID[ids.Deposit](t, d.ID)
	rows := outboxRowsFor(t, a.pool, d.ID)
	if len(rows) != 1 {
		t.Fatalf("%d outbox rows, want 1", len(rows))
	}
	row := rows[0]
	if row.EntityType != "deposit" || row.Subject != "deposit.created" || row.Attempts != 0 ||
		row.PublishedAt != nil || row.MsgID != depositID.TypeID()+".created" {
		t.Errorf("row = %+v", row)
	}
	eventID := idFromUUID[ids.Event](t, row.ID).TypeID()
	want := map[string]any{
		"event_id": eventID, "event_type": "deposit.created", "schema_version": 1.0,
		"deposit_id": depositID.TypeID(), "network": "SEPA", "network_transaction_id": body["network_transaction_id"],
		"account_id": "acct_01m564xr00fx7rjvqryn5dzwxx", "amount_minor": "1250", "currency": "EUR",
		"occurred_at": d.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	var payload map[string]any
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(payload)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("payload = %s\nwant      %s", gotJSON, wantJSON)
	}
	contracttest.CheckEvent(t, row.Payload)
	// No provider identifiers downstream.
	if strings.Contains(string(row.Payload), aliceIBAN) {
		t.Error("the payload holds the IBAN")
	}
	// xmin is the ID of the transaction that wrote a row version: the same for both rows, so one
	// transaction committed them.
	same := countOf(t, a.pool, `
SELECT count(*)::int FROM deposits p JOIN outbox o ON o.entity_id = p.id
WHERE p.id = $1 AND p.xmin::text = o.xmin::text`, d.ID)
	if same != 1 {
		t.Error("the deposit and its event were written by different transactions")
	}
}

func TestStoresNoEventForDuplicatesConflictsUnmatchedOrInvalid(t *testing.T) {
	a := newTestAPI(t)
	body := notificationBody(nil)
	a.postNotification(t, body, nil)
	d := mustDeposit(t, a.pool, body["network_transaction_id"])

	a.postNotification(t, body, nil) // duplicate
	conflicting := notificationBody(map[string]any{"amount_minor": "1"})
	conflicting["network_transaction_id"] = body["network_transaction_id"]
	a.postNotification(t, conflicting, nil) // conflict
	if n := outboxCount(t, a.pool, d.ID); n != 1 {
		t.Errorf("%d outbox rows for the deposit, want 1", n)
	}
	unmatched := notificationBody(map[string]any{"creditor_iban": foreignIBAN})
	a.postNotification(t, unmatched, nil)
	if n := outboxCount(t, a.pool, mustDeposit(t, a.pool, unmatched["network_transaction_id"]).ID); n != 0 {
		t.Errorf("%d outbox rows for the unmatched deposit, want 0", n)
	}
	invalid := notificationBody(map[string]any{"amount_minor": "0"})
	if r := a.postNotification(t, invalid, nil); r.status != 400 {
		t.Errorf("status = %d", r.status)
	}
	if _, ok := depositByNetworkID(t, a.pool, invalid["network_transaction_id"]); ok {
		t.Error("an invalid notification stored a deposit")
	}
}

// Lengths are counted as Zod counts them, in code points (measured against Node): 255 emoji are 510
// UTF-16 units but 255 characters, within network_transaction_id's 255; 256 are one too many.
func TestCountsLengthsInCodePointsAsZodDoes(t *testing.T) {
	a := newTestAPI(t)
	emoji := "\U0001F600"
	within := notificationBody(map[string]any{
		"network_transaction_id": strings.Repeat(emoji, 255), "creditor_iban": foreignIBAN,
	})
	if r := a.postNotification(t, within, nil); r.status != 204 {
		t.Errorf("255 emoji: got %d %s, want 204", r.status, r.body)
	}
	over := notificationBody(map[string]any{"network_transaction_id": strings.Repeat(emoji, 256)})
	r := a.postNotification(t, over, nil)
	errs, _ := r.json(t)["errors"].([]any)
	if r.status != 400 || len(errs) != 1 ||
		errs[0].(map[string]any)["message"] != "Too big: expected string to have <=255 characters" {
		t.Errorf("256 emoji: got %d %s", r.status, r.body)
	}
}
