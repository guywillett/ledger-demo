package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/guywillett/ledger-demo/go/internal/telemetrytest"
	"github.com/guywillett/ledger-demo/go/internal/testdb"
)

// The API's telemetry, against contracts/telemetry.md: the cases of
// node/test/telemetry/api.telemetry.test.ts. Each test records through in-memory providers
// (telemetrytest.Record) set before it builds the handler.

func serverSpans(rec *telemetrytest.Recorder) []sdktrace.ReadOnlySpan {
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.Spans() {
		if s.SpanKind() == trace.SpanKindServer {
			found = append(found, s)
		}
	}
	return found
}

// nameAndRoute is a server span's name and http.route ("" if it has none).
func nameAndRoute(s sdktrace.ReadOnlySpan) [2]string {
	return [2]string{s.Name(), telemetrytest.Attributes(s)["http.route"].AsString()}
}

func TestServerSpansAreNamedByTheRouteTemplateNeverTheRawPath(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	a.get(t, "/accounts/"+account.ID.TypeID()+"/balance")
	a.get(t, depositsPath(account.ID, "limit=1"))

	var got [][2]string
	for _, s := range serverSpans(rec) {
		got = append(got, nameAndRoute(s))
		if strings.Contains(s.Name(), account.ID.TypeID()) {
			t.Errorf("span name %q holds the raw path", s.Name())
		}
	}
	want := [][2]string{
		{"GET /accounts/{account_id}/balance", "/accounts/{account_id}/balance"},
		{"GET /accounts/{account_id}/deposits", "/accounts/{account_id}/deposits"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("spans %v\nwant  %v", got, want)
	}
}

func TestUnknownRoutesAndEmptyPathParametersAreNamedByTheMethodAlone(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	// a.do, not a.get: an unknown path isn't in the contract to check against.
	a.do(t, http.MethodGet, "/nope/"+newKey(), nil)
	a.do(t, http.MethodGet, "/accounts//balance", nil)
	var got [][2]string
	for _, s := range serverSpans(rec) {
		got = append(got, nameAndRoute(s))
	}
	if want := [][2]string{{"GET", ""}, {"GET", ""}}; !slices.Equal(got, want) {
		t.Errorf("spans %v, want %v", got, want)
	}
}

// Decision 84: the API's callers are outside the system.
func TestEveryRequestStartsANewTraceWhateverTraceparentItBrings(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	const sent = "0af7651916cd43dd8448eb211c80319c"
	_, r := a.do(t, http.MethodGet, "/health", http.Header{
		"Traceparent": {"00-" + sent + "-b7ad6b7169203331-01"},
		"Baggage":     {"user=someone"},
	})
	if r.status != 200 {
		t.Fatalf("status %d", r.status)
	}
	spans := serverSpans(rec)
	if len(spans) != 1 {
		t.Fatalf("%d server spans, want 1", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got == sent {
		t.Error("the request's trace is the one its traceparent named")
	}
	if spans[0].Parent().IsValid() {
		t.Errorf("parent %v, want none", spans[0].Parent())
	}
}

// IP addresses are personal data under the GDPR, a user agent nearly so (the contract's privacy
// rule): kept out though the conventions record them (telemetry.WithoutPersonalData, at export).
func TestServerSpansCarryNoClientAddressAndNoUserAgent(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	const agent = "Mozilla/5.0 (Macintosh; a person)"
	a.do(t, http.MethodGet, "/health", http.Header{
		"User-Agent":      {agent},
		"X-Forwarded-For": {"203.0.113.7"},
	})
	spans := serverSpans(rec)
	if len(spans) != 1 {
		t.Fatalf("%d server spans, want 1", len(spans))
	}
	attrs := telemetrytest.Attributes(spans[0])
	for _, key := range []attribute.Key{"client.address", "network.peer.address", "user_agent.original"} {
		if v, has := attrs[key]; has {
			t.Errorf("%s = %v", key, v.String())
		}
	}
	if raw, _ := json.Marshal(spans[0].Attributes()); strings.Contains(string(raw), "Mozilla") ||
		strings.Contains(string(raw), "203.0.113.7") {
		t.Errorf("personal data in %s", raw)
	}
}

func TestServerSpansCarryNoQueryString(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	a.get(t, depositsPath(account.ID, "network_transaction_id="+newKey()))
	spans := serverSpans(rec)
	if len(spans) != 1 {
		t.Fatalf("%d server spans, want 1", len(spans))
	}
	attrs := telemetrytest.Attributes(spans[0])
	if _, has := attrs["url.query"]; has {
		t.Errorf("url.query = %v", attrs["url.query"])
	}
	if got := attrs["url.path"].AsString(); got != "/accounts/"+account.ID.TypeID()+"/deposits" {
		t.Errorf("url.path = %q", got)
	}
}

func TestDatabaseSpansAreTheRequestsChildrenNamedByOperationWithTheStatementAndNoParameters(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	account := testdb.NewAccount(t, a.pool)
	rec.Reset() // NewAccount's own statements run outside any span; nothing to forget, but be sure
	a.get(t, "/accounts/"+account.ID.TypeID())

	server := serverSpans(rec)
	var queries []sdktrace.ReadOnlySpan
	for _, s := range rec.Spans() {
		if s.SpanKind() == trace.SpanKindClient {
			queries = append(queries, s)
		}
	}
	if len(server) != 1 || len(queries) == 0 {
		t.Fatalf("%d server spans and %d database spans", len(server), len(queries))
	}
	for _, q := range queries {
		attrs := telemetrytest.Attributes(q)
		if q.Parent().SpanID() != server[0].SpanContext().SpanID() || q.Name() != "SELECT" ||
			attrs["db.system.name"].AsString() != "postgresql" ||
			attrs["db.operation.name"].AsString() != "SELECT" ||
			!strings.Contains(attrs["db.query.text"].AsString(), "$1") {
			t.Errorf("database span %s %v", q.Name(), attrs)
		}
		// The account's UUID was the query's argument: nowhere in the span.
		if raw, _ := json.Marshal(q.Attributes()); strings.Contains(string(raw), account.ID.UUID().String()) {
			t.Errorf("an argument in %s", raw)
		}
	}
}

// Through every route that receives personal data: an incoming payment (an IBAN), a withdrawal (an
// IBAN, a name, remittance text), each as sent and as normalised.
func TestNoSpanCarriesAnIBANANameOrRemittanceText(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	if r := a.postNotification(t, notificationBody(map[string]any{
		"creditor_iban": "fr76 1234 5000 0100 0000 0000 193",
	}), nil); r.status != 204 {
		t.Fatalf("notification: %d %s", r.status, r.body)
	}
	account := fundedAccount(t, a.pool, 10_000)
	created := a.withdraw(t, account, map[string]any{
		"creditor_iban":   "de75 5121 0800 1245 1261 99",
		"creditor_name":   "Jane Q. Payee",
		"remittance_info": "Rent for flat 4B",
	}, newKey())
	if created.status != 201 {
		t.Fatalf("withdrawal: %d %s", created.status, created.body)
	}
	a.get(t, created.header.Get("Location"))

	type view struct {
		Name       string
		Attributes []attribute.KeyValue
		Events     []sdktrace.Event
		Resource   []attribute.KeyValue
	}
	var all []view
	for _, s := range rec.Spans() {
		all = append(all, view{s.Name(), s.Attributes(), s.Events(), s.Resource().Attributes()})
	}
	if len(all) <= 3 {
		t.Fatalf("only %d spans", len(all))
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	for _, personal := range []string{aliceIBAN, "fr76 1234", foreignIBAN, "de75 5121", "Jane Q. Payee", "Rent for flat 4B"} {
		if strings.Contains(string(raw), personal) {
			t.Errorf("%q in a span", personal)
		}
	}
}

// Decision 86: bound in withRequestID, from the span traced started.
func TestLogLinesCarryTheTraceIDAndSpanIDOfTheirRequest(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	requestID := newKey()
	a.do(t, http.MethodGet, "/accounts/"+testdb.NewAccount(t, a.pool).ID.TypeID(),
		http.Header{"X-Request-Id": {requestID}})
	server := serverSpans(rec)
	if len(server) != 1 {
		t.Fatalf("%d server spans", len(server))
	}
	for _, line := range a.logLines(t) {
		if line["request_id"] != requestID || line["msg"] != "request completed" {
			continue
		}
		if line["trace_id"] != server[0].SpanContext().TraceID().String() ||
			line["span_id"] != server[0].SpanContext().SpanID().String() {
			t.Errorf("access line %v, span %v", line, server[0].SpanContext())
		}
		return
	}
	t.Fatal("no access line for the request")
}

// Without a recording provider (telemetry off), no trace fields: the default the other tests run on.
func TestLogLinesCarryNoTraceIDsWithTelemetryOff(t *testing.T) {
	a := newTestAPI(t)
	a.get(t, "/health?x")
	a.get(t, "/accounts/"+testdb.NewAccount(t, a.pool).ID.TypeID())
	for _, line := range a.logLines(t) {
		if _, has := line["trace_id"]; has {
			t.Errorf("trace_id on %v", line)
		}
	}
}

// The route as an attribute of the request duration (otelhttp's labeler). Which metrics are exported,
// and with which attributes only, is telemetry.Setup's views (internal/telemetry's tests).
func TestTheRequestDurationCarriesTheRoute(t *testing.T) {
	rec := telemetrytest.Record(t)
	a := newTestAPI(t)
	a.get(t, "/accounts/"+testdb.NewAccount(t, a.pool).ID.TypeID()+"/balance")
	for _, sm := range rec.Metrics(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				route, _ := point.Attributes.Value("http.route")
				method, _ := point.Attributes.Value("http.request.method")
				status, _ := point.Attributes.Value("http.response.status_code")
				if route.AsString() == "/accounts/{account_id}/balance" && method.AsString() == "GET" &&
					status.AsInt64() == 200 {
					return
				}
			}
		}
	}
	t.Errorf("no request duration with the route in %+v", rec.Metrics(t))
}
