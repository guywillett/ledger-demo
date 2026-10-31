package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Request bodies, decoded by hand into validated types (slice 3 decision 47), in two stages, each
// matching what Node's API does (measured against its server, case by case):
//
//  1. readJSONBody: the body as JSON, or a framing error, as Fastify's content-type parser and
//     body limit answer: 415 for a body that isn't application/json, 413 over 1 MiB, 400 for an
//     empty or malformed one. The client API answers with that status; the provider webhooks
//     answer every one with 400 (the webhook contract).
//  2. fields: each known field read from the JSON object through its validating type, every
//     problem collected with its path and Zod's message, as Node's schemas report them.

// maxBodyBytes is Fastify's default body limit, 1 MiB: a body of exactly this size is accepted.
const maxBodyBytes = 1 << 20

// framingError is a body Node's API rejects before its schema sees it.
type framingError struct {
	status int    // the status Node's client API answers with (the webhooks answer 400)
	detail string // Fastify's message
}

var (
	errUnsupportedMediaType = &framingError{
		http.StatusUnsupportedMediaType, "Unsupported Media Type",
	}
	errBodyTooLarge = &framingError{
		http.StatusRequestEntityTooLarge, "Request body is too large",
	}
	errEmptyBody = &framingError{
		http.StatusBadRequest, "Body cannot be empty when content-type is set to 'application/json'",
	}
	errInvalidJSON = &framingError{
		http.StatusBadRequest, "Body is not valid JSON but content-type is set to 'application/json'",
	}
)

// readJSONBody reads the request body as JSON. It returns the body (nil if there was none), or the
// framing error Node's API gives:
//
//   - a Content-Type other than application/json (case-insensitive, any parameters, any charset):
//     415, before the body is read. application/vnd.api+json and the like are other types.
//   - no Content-Type: an empty body is no body (nil, and the schema reports it), any other 415.
//   - over 1 MiB: 413 (http.MaxBytesReader stops reading past the limit).
//   - empty: 400.
//   - a leading byte-order mark is skipped, as Fastify's parser does (encoding/json refuses it).
//   - not JSON, trailing data after it, or a "__proto__" key or a "constructor" object with a
//     "prototype" key at any depth: 400. Fastify's parser (secure-json-parse) rejects those keys,
//     which could alter object prototypes in JavaScript; harmless in Go, rejected to answer alike.
//
// A body that can't be read for any other reason (the request deadline, a dropped connection) is
// no fault of the body's: it is returned as an error, which the handler answers with 500, the
// status that says a retry can help.
func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, *framingError, error) {
	contentType, hasContentType := r.Header["Content-Type"]
	if hasContentType {
		mediaType, _, err := mime.ParseMediaType(contentType[0])
		if err != nil { // a malformed parameter: judge the type alone
			mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType[0], ";", 2)[0]))
		}
		if mediaType != "application/json" {
			return nil, errUnsupportedMediaType, nil
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if maxErr := new(http.MaxBytesError); errors.As(err, &maxErr) {
		if !hasContentType {
			return nil, errUnsupportedMediaType, nil
		}
		return nil, errBodyTooLarge, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the request body: %w", err)
	}
	if !hasContentType {
		if len(body) == 0 {
			return nil, nil, nil
		}
		return nil, errUnsupportedMediaType, nil
	}
	if len(body) == 0 {
		return nil, errEmptyBody, nil
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")) // UTF-8 byte-order mark
	if !isAcceptableJSON(body) {
		return nil, errInvalidJSON, nil
	}
	return body, nil, nil
}

// isAcceptableJSON reports whether body is exactly one JSON value, with no prototype keys (see
// readJSONBody), in one pass over its tokens: json.Decoder.Token returns each delimiter, key and
// value in turn, and fails on anything that isn't JSON.
//
// Numbers stay text (UseNumber): never converted to float64, so a number too large for one, such
// as 1e400, is still JSON, as it is to JavaScript's JSON.parse (which reads it as Infinity).
// json.Unmarshal into an any would convert them, and refuse that body.
func isAcceptableJSON(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if !acceptableValue(decoder, false) {
		return false
	}
	_, err := decoder.Token() // only the end may follow
	return err == io.EOF
}

// acceptableValue reads one value from the decoder, objects and arrays to their end, and reports
// whether it was JSON without prototype keys. underConstructor is true for the value of a
// "constructor" key: as an object, it may not have a "prototype" key.
func acceptableValue(decoder *json.Decoder, underConstructor bool) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false
			}
			key, _ := keyToken.(string)
			if key == "__proto__" || (underConstructor && key == "prototype") {
				return false
			}
			if !acceptableValue(decoder, key == "constructor") {
				return false
			}
		}
	case json.Delim('['):
		for decoder.More() {
			if !acceptableValue(decoder, false) {
				return false
			}
		}
	default:
		return true // a string, number, boolean or null
	}
	_, err = decoder.Token() // the closing } or ]
	return err == nil
}

// fields reads a JSON object's fields one by one, collecting a ValidationError per problem, in the
// order they are read (call in the schema's order: Zod reports in its shape's order).
//
// Keys match exactly. encoding/json's struct decoding matches keys case-insensitively
// (Amount_Minor would fill AmountMinor); Node's schemas don't, so the object is decoded into a map
// and each field looked up by its exact name. A key given twice counts once, its last value, as in
// JavaScript's JSON.parse (and encoding/json's map decoding).
type fields struct {
	body   []byte
	object map[string]json.RawMessage
	errs   []ValidationError
}

// newFields decodes body as an object. A body that isn't one (an array, a string, null, or no body
// at all) is a problem at path "", as Zod reports it, and leaves no fields to read.
func newFields(body []byte) *fields {
	f := &fields{body: body}
	if body == nil {
		f.fail("", "Invalid input: expected object, received undefined")
		return f
	}
	if kind := jsonKind(body); kind != "object" {
		f.fail("", "Invalid input: expected object, received "+kind)
		return f
	}
	// Into json.RawMessage values: each field's JSON text, decoded only when the field is read
	// (numbers included, so a 1e400 in a field nobody reads costs nothing).
	if err := json.Unmarshal(body, &f.object); err != nil {
		f.fail("", "Invalid input: expected object, received "+jsonKind(body))
		return f
	}
	return f
}

// objectKeys lists a JSON object's keys in the order they appear (a Go map has no order), each once,
// reading the body as a stream of tokens. Only rejectUnknown needs it, so only it calls it.
func objectKeys(body []byte) []string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if _, err := decoder.Token(); err != nil { // the opening {
		return nil
	}
	var keys []string
	seen := map[string]struct{}{} // a set: struct{} values take no memory
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return keys
		}
		key, _ := token.(string)
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		var skip json.RawMessage // the key's value, whatever it is
		if err := decoder.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}

func (f *fields) fail(path, message string) {
	f.errs = append(f.errs, ValidationError{Path: path, Message: message})
}

// valid reports whether the body was an object; if not, no field is read (or reported).
func (f *fields) valid() bool { return f.object != nil }

// rejectUnknown reports every key not in known, as Zod's strictObject does: for client bodies (step
// 7, withdrawals), where a misspelt optional field must fail rather than be dropped (slice 2
// decision 33). Webhook bodies don't call it: providers add fields over time (tolerant reader).
func (f *fields) rejectUnknown(known ...string) {
	var unknown []string
	for _, key := range objectKeys(f.body) {
		if !slices.Contains(known, key) {
			unknown = append(unknown, `"`+key+`"`)
		}
	}
	switch len(unknown) {
	case 0:
	case 1:
		f.fail("", "Unrecognized key: "+unknown[0])
	default:
		f.fail("", "Unrecognized keys: "+strings.Join(unknown, ", "))
	}
}

// rawString reads a field that must be a JSON string: absent, null or another type is a problem
// with Zod's message. ok is false if it isn't a string (and the problem is recorded).
func (f *fields) rawString(name string) (string, bool) {
	raw, present := f.object[name]
	if !present {
		f.fail(name, "Invalid input: expected string, received undefined")
		return "", false
	}
	var s string
	if kind := jsonKind(raw); kind != "string" || json.Unmarshal(raw, &s) != nil {
		f.fail(name, "Invalid input: expected string, received "+jsonKind(raw))
		return "", false
	}
	return s, true
}

// stringLength reads a required string of minLen to maxLen characters, counted as Zod counts them
// (code points, see zodLen): Zod's .min() and .max(). Both bounds are checked, so a value can
// fail either; ok is false if it failed anything.
func (f *fields) stringLength(name string, minLen, maxLen int) (string, bool) {
	s, ok := f.rawString(name)
	if !ok {
		return "", false
	}
	return s, f.checkLength(name, s, minLen, maxLen)
}

// optionalStringLength is stringLength for an optional field: absent is nil (no problem); null, as
// in Zod's .optional(), is a problem ("expected string, received null").
func (f *fields) optionalStringLength(name string, minLen, maxLen int) *string {
	if _, present := f.object[name]; !present {
		return nil
	}
	s, ok := f.stringLength(name, minLen, maxLen)
	if !ok {
		return nil
	}
	return &s
}

func (f *fields) checkLength(name, s string, minLen, maxLen int) bool {
	ok := true
	if n := zodLen(s); n < minLen {
		f.fail(name, "Too small: expected string to have >="+strconv.Itoa(minLen)+" characters")
		ok = false
	} else if n > maxLen {
		f.fail(name, "Too big: expected string to have <="+strconv.Itoa(maxLen)+" characters")
		ok = false
	}
	return ok
}

// literal reads a field that must be exactly want (Zod's z.literal): anything else, absent and
// other types included, is the same problem.
func (f *fields) literal(name, want string) (string, bool) {
	var s string
	if raw, present := f.object[name]; present && json.Unmarshal(raw, &s) == nil && s == want {
		return s, true
	}
	f.fail(name, `Invalid input: expected "`+want+`"`)
	return "", false
}

// jsonKind names the JSON value's type as Zod's messages do (JSON true and false are "boolean"). A
// number too large for a float64 is JavaScript's Infinity or -Infinity, and Zod names it so
// (measured: "expected string, received Infinity"); one too small, such as 1e-400, is 0, a number.
func jsonKind(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "undefined"
	}
	switch trimmed[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	if f, err := strconv.ParseFloat(string(trimmed), 64); err != nil && math.IsInf(f, 0) {
		if f > 0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	return "number"
}

// zodLen is a string's length as Zod's .min() and .max() count it: in code points (measured with
// Zod 4.6.7: an emoji counts 1, a flag or a letter with a combining accent 2), not JavaScript's
// UTF-16 .length and not graphemes. Go's utf8.RuneCountInString counts the same.
func zodLen(s string) int {
	return utf8.RuneCountInString(s)
}
