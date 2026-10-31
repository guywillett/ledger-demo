package provider_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/provider"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/submitter"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// The provider client over real HTTP, against httptest servers: every answer maps to its outcome,
// an unknown outcome resolves on the next call with the same key, and the request is byte for byte
// Node's. Node's tests (test/submitter/provider.test.ts) run against provider-sim; that, the real
// fake provider, is exercised end to end by the demo (step 8), against either implementation.
//
// httptest.NewServer starts a real HTTP server on a local port, its handler a plain function; its
// URL is the base URL, and Close stops it (t.Cleanup).

const foreignIBAN = "DE75512108001245126199"

func withdrawal() withdrawals.Withdrawal {
	remittance := "Rent October"
	return withdrawals.Withdrawal{
		ID:             ids.New[ids.Withdrawal](),
		Account:        ids.New[ids.Account](),
		IdempotencyKey: "client-key",
		Amount:         2500,
		Currency:       "EUR",
		CreditorIBAN:   sepa.IBAN(foreignIBAN),
		CreditorName:   "Jane Doe",
		RemittanceInfo: &remittance,
		Status:         withdrawals.StatusHeld,
	}
}

func server(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// submit starts a server with handler and submits a withdrawal to it, with timeout.
func submit(t *testing.T, handler http.HandlerFunc, timeout time.Duration) submitter.Outcome {
	t.Helper()
	return client(t, server(t, handler), timeout).SubmitTransfer(t.Context(), withdrawal())
}

func client(t *testing.T, baseURL string, timeout time.Duration) *provider.Client {
	t.Helper()
	c, err := provider.NewClient(baseURL, timeout, 10)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// answer is a handler answering every request with status and body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// idempotentProvider is a provider with provider-sim's idempotency: a new key creates a transfer
// (201), the same key again answers it (200), the same key with another body is a 409. delay, if
// set, is waited after creating a transfer and before answering (a slow provider).
type idempotentProvider struct {
	mu      sync.Mutex
	byKey   map[string]string // key -> body
	ids     map[string]string // key -> transfer ID
	created atomic.Int32
	delay   time.Duration
}

func (p *idempotentProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Header.Get("Idempotency-Key")
	p.mu.Lock()
	if p.byKey == nil {
		p.byKey, p.ids = map[string]string{}, map[string]string{}
	}
	stored, seen := p.byKey[key]
	status := http.StatusOK
	switch {
	case seen && stored != string(body):
		p.mu.Unlock()
		conflict := `{"error":"Idempotency-Key reused with a different request"}`
		answer(http.StatusConflict, conflict)(w, r)
		return
	case !seen:
		p.byKey[key], p.ids[key] = string(body), "tr_"+ids.New[ids.Event]().TypeID()[4:]
		p.created.Add(1)
		status = http.StatusCreated
	}
	id := p.ids[key]
	p.mu.Unlock()
	if status == http.StatusCreated && p.delay > 0 {
		time.Sleep(p.delay)
	}
	answer(status, `{"transfer_id":"`+id+`","status":"accepted"}`)(w, r)
}

func TestMaps201Then200ForARepeatOfTheSameKeyToAcceptedWithTheSameTransferID(t *testing.T) {
	c := client(t, server(t, (&idempotentProvider{}).ServeHTTP), 2*time.Second)
	w := withdrawal()
	first := c.SubmitTransfer(t.Context(), w)
	again := c.SubmitTransfer(t.Context(), w)
	if first.Kind != submitter.Accepted || !strings.HasPrefix(string(first.TransferID), "tr_") ||
		!reflect.DeepEqual(again, first) {
		t.Errorf("first %+v, again %+v", first, again)
	}
}

func TestMapsA422ToRefusedWithTheReason(t *testing.T) {
	got := submit(t, answer(422, `{"reason":"creditor account closed","extra":1}`), time.Second)
	if got.Kind != submitter.Refused || got.Reason != "creditor account closed" {
		t.Errorf("got %+v", got)
	}
	// An empty reason gets a default; one that isn't a string is an answer we can't read.
	got = submit(t, answer(422, `{"reason":""}`), time.Second)
	if got.Reason != "refused by the provider" {
		t.Errorf("empty reason: %+v", got)
	}
	got = submit(t, answer(422, `{"reason":null}`), time.Second)
	if got.Kind != submitter.Unknown || got.Cause != "unreadable 422 response" {
		t.Errorf("null reason: %+v", got)
	}
}

// A provider that repeats the IBAN in its error message, and in a path: only plain field paths
// and the body's size are kept.
func TestKeepsRequestValuesAProviderEchoesInA400OutOfTheOutcome(t *testing.T) {
	body := `{"errors":[{"path":"creditor_iban","message":"` + foreignIBAN + ` is not reachable"},` +
		`{"path":"iban ` + foreignIBAN + `","message":"unknown"}]}`
	got := submit(t, answer(400, body), time.Second)
	if got.Kind != submitter.Invalid || !reflect.DeepEqual(got.Fields, []string{"creditor_iban"}) ||
		got.BodyBytes != len(body) {
		t.Errorf("got %+v", got)
	}
	if text, _ := json.Marshal(got); strings.Contains(string(text), foreignIBAN) {
		t.Errorf("the outcome carries the IBAN: %s", text)
	}
	// A body that isn't the expected shape: no fields at all, only its size.
	for _, odd := range []string{`not json`, `{"errors":[{"path":5}]}`, `{"errors":null}`} {
		got := submit(t, answer(400, odd), time.Second)
		if got.Kind != submitter.Invalid || len(got.Fields) != 0 || got.BodyBytes != len(odd) {
			t.Errorf("%s: %+v", odd, got)
		}
	}
}

func TestMapsA409OurKeyWithAnotherBodyToKeyConflict(t *testing.T) {
	c := client(t, server(t, (&idempotentProvider{}).ServeHTTP), time.Second)
	w := withdrawal()
	c.SubmitTransfer(t.Context(), w)
	w.Amount = 2501
	if got := c.SubmitTransfer(t.Context(), w); got.Kind != submitter.KeyConflict {
		t.Errorf("got %+v", got)
	}
}

func TestMapsA503ToUnknownAndTheRetryWithTheSameKeyIsAccepted(t *testing.T) {
	var calls atomic.Int32
	sim := &idempotentProvider{}
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			answer(http.StatusServiceUnavailable, `{"error":"try later"}`)(w, r)
			return
		}
		sim.ServeHTTP(w, r)
	})
	c := client(t, url, time.Second)
	w := withdrawal()
	got := c.SubmitTransfer(t.Context(), w)
	if got.Kind != submitter.Unknown || got.Cause != "provider answered 503" {
		t.Errorf("first: %+v", got)
	}
	if got := c.SubmitTransfer(t.Context(), w); got.Kind != submitter.Accepted {
		t.Errorf("retry: %+v", got)
	}
}

func TestMapsAConnectionFailureToUnknown(t *testing.T) {
	nowhere := client(t, "http://127.0.0.1:1", time.Second)
	if got := nowhere.SubmitTransfer(t.Context(), withdrawal()); got.Kind != submitter.Unknown {
		t.Errorf("got %+v", got)
	}
}

// The unknown-outcome proof: the provider created the transfer but answered after our timeout.
// Retrying with the same key finds that transfer instead of creating a second one.
func TestMapsATimeoutToUnknownAndTheNextCallWithTheSameKeyGetsTheSameSingleTransfer(t *testing.T) {
	sim := &idempotentProvider{delay: 600 * time.Millisecond}
	c := client(t, server(t, sim.ServeHTTP), 100*time.Millisecond)
	w := withdrawal()
	if got := c.SubmitTransfer(t.Context(), w); got.Kind != submitter.Unknown ||
		!strings.Contains(got.Cause, "context deadline exceeded") {
		t.Errorf("timed out: %+v", got)
	}
	if n := sim.created.Load(); n != 1 {
		t.Fatalf("%d transfers created, want 1 (it arrived, only the answer was lost)", n)
	}
	if got := c.SubmitTransfer(t.Context(), w); got.Kind != submitter.Accepted {
		t.Errorf("retried: %+v", got)
	}
	time.Sleep(600 * time.Millisecond) // let the slow first answer finish
	if n := sim.created.Load(); n != 1 {
		t.Errorf("%d transfers created, want 1", n)
	}
}

// The timeout covers reading the body too: headers sent, then the body stalls.
func TestTimesOutABodyThatStallsAsUnknown(t *testing.T) {
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"transfer_id":`)
		w.(http.Flusher).Flush()
		// Stall until the client gives up and closes the connection, which cancels the request's
		// context on the server: so the server's Close (cleanup) doesn't wait on this handler.
		<-r.Context().Done()
	})
	started := time.Now()
	got := client(t, url, 200*time.Millisecond).SubmitTransfer(t.Context(), withdrawal())
	if got.Kind != submitter.Unknown || time.Since(started) > 2*time.Second {
		t.Errorf("got %+v after %v", got, time.Since(started))
	}
}

func TestMapsAnswersItCantReadToUnknown(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"transfer_id":null}`, `{"transfer_id":""}`,
		`{"transfer_id":5}`, `{"Transfer_ID":"tr_1"}`,
		`{"transfer_id":"` + strings.Repeat("t", 256) + `"}`} {
		got := submit(t, answer(201, body), time.Second)
		if got.Kind != submitter.Unknown || got.Cause != "unreadable 201 response" {
			t.Errorf("%.40s: %+v", body, got)
		}
	}
}

// A body over the read limit is not kept: an answer whose body must be read (a 2xx) is one we
// can't read, but a 400 is still Invalid, with its exact size and no fields, and a 409 a key
// conflict.
func TestABodyOverTheLimitIsUnknownUnlessTheStatusDecides(t *testing.T) {
	huge := `{"transfer_id":"tr_1","padding":"` + strings.Repeat("x", 1<<20) + `"}`
	got := submit(t, answer(201, huge), 2*time.Second)
	if got.Kind != submitter.Unknown || got.Cause != "provider response over 1048576 bytes" {
		t.Errorf("got %+v", got)
	}
	big400 := `{"errors":[{"path":"creditor_name","message":"` + strings.Repeat("x", 1<<20) + `"}]}`
	got = submit(t, answer(400, big400), 2*time.Second)
	if got.Kind != submitter.Invalid || len(got.Fields) != 0 || got.BodyBytes != len(big400) {
		t.Errorf("400: kind %v, fields %v, bytes %d (want %d)",
			got.Kind, got.Fields, got.BodyBytes, len(big400))
	}
	if got := submit(t, answer(409, huge), 2*time.Second); got.Kind != submitter.KeyConflict {
		t.Errorf("409: %+v", got)
	}
}

// The request, byte for byte what Node's providerClient sends: JSON.stringify's output for the same
// withdrawal (captured from Node), the withdrawal's TypeID as the key, POST /transfers from the
// base URL's root, as Node's new URL('/transfers', base) resolves it.
func TestSendsNodesExactRequest(t *testing.T) {
	type seen struct{ method, path, contentType, key, body string }
	var got seen
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.Path, r.Header.Get("Content-Type"),
			r.Header.Get("Idempotency-Key"), string(body)}
		answer(201, `{"transfer_id":"tr_1"}`)(w, r)
	})
	w := withdrawal()
	w.CreditorName = "O'Brien + Sons / Ltd."
	remittance := "Rent (October) 1/2"
	w.RemittanceInfo = &remittance
	const prefix = `{"amount_minor":"2500","currency":"EUR","creditor_iban":"DE75512108001245126199",` +
		`"creditor_name":"O'Brien + Sons / Ltd.",`

	client(t, url+"/api/v1", time.Second).SubmitTransfer(t.Context(), w)
	want := seen{"POST", "/transfers", "application/json", w.ID.TypeID(),
		prefix + `"remittance_info":"Rent (October) 1/2","reference":"` + w.ID.TypeID() + `"}`}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	w.RemittanceInfo = nil // null, as Node sends no remittance
	client(t, url, time.Second).SubmitTransfer(t.Context(), w)
	wantBody := prefix + `"remittance_info":null,"reference":"` + w.ID.TypeID() + `"}`
	if got.body != wantBody {
		t.Errorf("body %s\nwant %s", got.body, wantBody)
	}
}
