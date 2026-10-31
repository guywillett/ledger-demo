// Package provider is the provider's HTTP API from the submitter's side: one call, POST /transfers,
// mapped to an outcome. The boundary to the outside world, so the submitter's handler stays pure
// logic and its tests use a fake. Node: node/src/submitter/provider.ts.
//
// A package of its own rather than part of the submitter: it is the only code that speaks HTTP to
// the provider. It is provider-sim's adapter: it translates that API's answers into the submitter's
// vocabulary (submitter.Outcome, defined there because what an outcome means is the submitter's
// decision) and satisfies the interface the submitter defines (submitter.Provider) without the
// submitter knowing this package exists. A second provider would be a second adapter like this one.
//
// The mapping (outcomeOf), as Node's: 201, or 200 for a repeat of our key, is Accepted; 422 is
// Refused; 400 is Invalid; 409 is KeyConflict (provider-sim's meaning: our key with another body);
// anything else, and any answer that can't be read, is Unknown.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/submitter"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// maxResponseBytes is the most of a response body read. The provider's answers are a few hundred
// bytes; a larger body is read no further, so a misbehaving provider can't make us buffer without
// limit (Node reads it all). It ends as Unknown, as any answer we can't read.
const maxResponseBytes = 1 << 20

// fieldPath is what a field path from a 400 must look like to be kept (Node's FIELD_PATH): a plain
// identifier, so nothing else from the body can slip through.
var fieldPath = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Client calls the provider. One per process, safe for concurrent use: its *http.Client keeps a
// pool of connections to the provider (in its Transport), which every call reuses, so it is made
// once, never per call.
type Client struct {
	transfers *url.URL
	http      *http.Client
	timeout   time.Duration
}

// NewClient is a client for the provider at baseURL, each call abandoned after timeout
// (PROVIDER_TIMEOUT_MS), for up to concurrency calls at once (SUBMITTER_CONCURRENCY).
//
// Its *http.Client has no Timeout of its own: each call gets its deadline from its context instead
// (see SubmitTransfer), which also lets the caller's cancellation (shutdown, decision 59) end it.
// Never http.DefaultClient: it is shared by the whole process and has no timeout at all, so a
// provider that never answers would hold the call for ever.
//
// Its Transport (the connection pool) is a clone of http.DefaultTransport keeping up to
// concurrency idle connections to the provider: the default keeps only 2 per host, so with more
// calls at once most connections would be closed after each response and dialled again for the
// next (Node's fetch keeps as many as it uses). The clone keeps the default's Proxy,
// ProxyFromEnvironment: HTTP_PROXY / HTTPS_PROXY / NO_PROXY, if set, route these calls, as for any
// Go HTTP client. Node's fetch ignores those variables unless NODE_USE_ENV_PROXY is set, so a
// deployment that sets them routes the two implementations differently: a configuration choice,
// left to whoever sets them.
func NewClient(baseURL string, timeout time.Duration, concurrency int) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("provider URL: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = concurrency
	transport.MaxIdleConns = max(transport.MaxIdleConns, concurrency)
	return &Client{
		// /transfers from the root, as Node's new URL('/transfers', baseUrl) resolves it: a path
		// in PROVIDER_URL is replaced, not extended (url.JoinPath would extend it).
		transfers: base.ResolveReference(&url.URL{Path: "/transfers"}),
		http:      &http.Client{Transport: transport},
		timeout:   timeout,
	}, nil
}

// transferRequest is POST /transfers' body, its fields in Node's order: encoding/json writes a
// struct's fields in declaration order, so the bytes are JSON.stringify's (see requestBody).
type transferRequest struct {
	AmountMinor    money.Amount `json:"amount_minor"`
	Currency       string       `json:"currency"`
	CreditorIBAN   sepa.IBAN    `json:"creditor_iban"`
	CreditorName   string       `json:"creditor_name"`
	RemittanceInfo *string      `json:"remittance_info"` // null when none, as Node sends it
	Reference      string       `json:"reference"`
}

// requestBody is the request's JSON, byte for byte what Node's JSON.stringify writes for the same
// withdrawal: the same fields in the same order, null for no remittance, no HTML escaping (Go
// escapes <, > and & by default; Node doesn't) and no trailing newline.
//
// It matters because the same Idempotency-Key may be retried by the other implementation (a
// redelivery can reach either in a mixed stack). provider-sim compares the parsed fields, but many
// real providers store a hash of the body with the key and answer a retry whose bytes differ with
// 409, which the submitter treats as a bug.
func requestBody(w withdrawals.Withdrawal) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(transferRequest{
		AmountMinor:    w.Amount,
		Currency:       w.Currency,
		CreditorIBAN:   w.CreditorIBAN,
		CreditorName:   w.CreditorName,
		RemittanceInfo: w.RemittanceInfo,
		Reference:      w.ID.TypeID(),
	})
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), err
}

// SubmitTransfer sends a withdrawal to the provider and maps the answer to an Outcome. The request
// is built only from the stored withdrawal, so every attempt sends the identical request, with the
// withdrawal's TypeID as its Idempotency-Key.
func (c *Client) SubmitTransfer(ctx context.Context, w withdrawals.Withdrawal) submitter.Outcome {
	body, err := requestBody(w)
	if err != nil {
		// Can't happen: every field encodes infallibly (strings, an amount, an IBAN). Were it
		// ever to, nothing has been sent, so Unknown's "the provider may have it" doesn't apply;
		// but it is the safe failure (the withdrawal stays held, nothing moves, the message is
		// retried), where Invalid would log that the provider rejected a request it never saw.
		return unknown(err.Error())
	}
	// The deadline covers the whole call, the body's read included: the response body is read
	// before this function returns, while ctx is still live (Node's AbortSignal.timeout likewise
	// aborts a body still arriving).
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.transfers.String(),
		bytes.NewReader(body))
	if err != nil {
		return unknown(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", w.ID.TypeID())

	resp, err := c.http.Do(req)
	if err != nil {
		// A timeout, connection refused, reset mid-request: the request may have arrived.
		return unknown(err.Error())
	}
	// Always closed, whatever happens next: an unclosed body holds its connection, which then
	// can't be reused (and leaks, if it happens on every call).
	defer func() { _ = resp.Body.Close() }()
	text, size, err := readBody(resp.Body)
	if err != nil {
		return unknown(err.Error()) // e.g. the timeout during the body
	}
	return outcomeOf(resp.StatusCode, text, size)
}

// readBody reads a response body: its first maxResponseBytes, kept, and its whole size, the rest
// counted without being kept (io.Copy to io.Discard), so memory stays bounded and a 400's size is
// still exact. Time is bounded by the call's deadline. One byte more than the limit is kept, to
// tell "exactly the limit" from "over it".
func readBody(body io.Reader) (text []byte, size int, err error) {
	text, err = io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, err
	}
	rest, err := io.Copy(io.Discard, body)
	if err != nil {
		return nil, 0, err
	}
	return text, len(text) + int(rest), nil
}

// outcomeOf maps a status and its body (text, at most the limit plus one byte, and its whole size)
// to an Outcome, as Node does. The status decides first: a 409 is KeyConflict and a 400 Invalid
// whatever their bodies, however large. Only the answers whose body must be read (2xx, 422) are
// Unknown when it is over the limit, where Node reads any size: a deliberate difference, so a
// provider can't make us buffer without bound.
//
// Bodies are decoded tolerantly (unknown fields ignored: providers add fields over time) but
// strictly typed, field names matched exactly (into a map, not a struct: encoding/json matches
// struct fields case-insensitively).
func outcomeOf(status int, text []byte, size int) submitter.Outcome {
	switch {
	case status == http.StatusConflict:
		return submitter.Outcome{Kind: submitter.KeyConflict}
	case status == http.StatusBadRequest:
		fields := []string{} // a body over the limit isn't parsed: its size only
		if size <= maxResponseBytes {
			fields = errorFields(objectOf(text))
		}
		return submitter.Outcome{Kind: submitter.Invalid, Fields: fields, BodyBytes: size}
	case size > maxResponseBytes:
		return unknown("provider response over " + strconv.Itoa(maxResponseBytes) + " bytes")
	}
	object := objectOf(text)
	switch status {
	case http.StatusOK, http.StatusCreated:
		// A 2xx we can't read is not an acceptance: the next attempt with the same key gets the
		// transfer ID.
		if id, ok := stringField(object, "transfer_id"); ok && withinLength(id, 1, 255) {
			transferID := withdrawals.TransferID(id)
			return submitter.Outcome{Kind: submitter.Accepted, TransferID: transferID}
		}
		return unknown(fmt.Sprintf("unreadable %d response", status))
	case http.StatusUnprocessableEntity:
		reason, ok := stringField(object, "reason")
		if !ok || !withinLength(reason, 0, 1000) {
			return unknown("unreadable 422 response")
		}
		if reason == "" {
			reason = "refused by the provider"
		}
		return submitter.Outcome{Kind: submitter.Refused, Reason: reason}
	default:
		return unknown(fmt.Sprintf("provider answered %d", status))
	}
}

// objectOf decodes a body as a JSON object: nil if it isn't one.
func objectOf(text []byte) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(text, &object)
	return object
}

// unknown is an Unknown outcome with its cause.
func unknown(cause string) submitter.Outcome {
	return submitter.Outcome{Kind: submitter.Unknown, Cause: cause}
}

// stringField reads a field that must be a JSON string (not null, not another type).
func stringField(object map[string]json.RawMessage, name string) (string, bool) {
	raw, present := object[name]
	var s string
	if !present || len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// withinLength checks a length as Zod's .min() and .max() count it: in code points, not
// JavaScript's UTF-16 .length (measured with Zod 4.6.7 in step 4b: 128 emoji pass max(255), a flag
// counts 2; see api's zodLen).
func withinLength(s string, minLen, maxLen int) bool {
	n := utf8.RuneCountInString(s)
	return n >= minLen && n <= maxLen
}

// errorFields are the field paths a 400's errors name, as Node's errorFields: only if the body
// has errors, an array of at most 50 objects each with a string path (otherwise none at all), and
// only the paths that look like field names. Never the messages: they may repeat the input.
func errorFields(object map[string]json.RawMessage) []string {
	var errs []map[string]json.RawMessage
	if json.Unmarshal(object["errors"], &errs) != nil || errs == nil || len(errs) > 50 {
		return []string{}
	}
	paths := make([]string, 0, len(errs))
	for _, e := range errs {
		path, ok := stringField(e, "path")
		if !ok {
			return []string{} // one malformed error: the whole schema fails, as Zod's would
		}
		if fieldPath.MatchString(path) {
			paths = append(paths, path)
		}
	}
	return paths
}
