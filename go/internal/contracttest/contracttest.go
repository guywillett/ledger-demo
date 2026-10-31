// Package contracttest checks HTTP responses against the hand-written contract,
// contracts/openapi.yaml, as Node's test/contract.ts does: every Go API test response is checked
// (slice 3, Done when). Imported only by tests, like vectortest.
//
// The validator is pb33f's libopenapi-validator, test-only (it is never linked into a binary: Go
// only builds what the binaries import), pinned in go.mod. Chosen because the contract is OpenAPI
// 3.1, whose schemas are JSON Schema 2020-12 (type arrays such as [string, 'null'], const), and
// libopenapi-validator was built for 3.1 on a full 2020-12 validator (santhosh-tekuri's
// jsonschema, through pb33f's maintained fork). kin-openapi, the long-standing alternative, now
// also handles 3.1 by switching to a 2020-12 validator for 3.1 documents, but that path is recent
// and its README still lists cases where it behaves differently from its 3.0 path.
// libopenapi-validator also takes the *http.Request and *http.Response pair directly, which is
// what net/http/httptest gives a test.
package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pb33f/jsonschema/v6"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
)

// contractPath is the contract, found from this source file's own location (runtime.Caller gives
// the path the file was compiled from) rather than from the test's working directory, which is
// whichever package is being tested: so it works from a package at any depth. go test doesn't use
// -trimpath, so the path is a real one.
var contractPath = func() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "../../../contracts/openapi.yaml")
}()

// loadDocument reads and parses the contract once per test binary, however many tests use it.
// sync.OnceValues runs the function the first time it's called and returns the same results every
// time after, goroutine-safely.
var loadDocument = sync.OnceValues(func() (libopenapi.Document, error) {
	spec, err := os.ReadFile(contractPath)
	if err != nil {
		return nil, err
	}
	return libopenapi.NewDocument(spec)
})

// loadModel is the contract's OpenAPI 3 model, built once, for reading it (RequestExample).
var loadModel = sync.OnceValues(func() (*v3.Document, error) {
	document, err := loadDocument()
	if err != nil {
		return nil, err
	}
	model, err := document.BuildV3Model()
	if err != nil {
		return nil, err
	}
	return &model.Model, nil
})

// load builds the validator once, from the same parsed document.
var load = sync.OnceValues(func() (validator.Validator, error) {
	document, err := loadDocument()
	if err != nil {
		return nil, err
	}
	v, errs := validator.NewValidator(document)
	if len(errs) > 0 {
		return nil, errs[0]
	}
	if ok, problems := v.ValidateDocument(); !ok {
		return nil, fmtErrors(problems)
	}
	return v, nil
})

// Errors returns what's wrong with the response for the request, or "" if it matches the contract:
// its status must be documented for the operation, and its body (if the contract documents one)
// must match the schema for its content type. The response body is read and put back, so the
// caller can still read it.
func Errors(t testing.TB, req *http.Request, resp *http.Response) string {
	t.Helper()
	v, err := load()
	if err != nil {
		t.Fatalf("loading %s: %v", contractPath, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	defer func() { resp.Body = io.NopCloser(bytes.NewReader(body)) }()
	if ok, problems := v.ValidateHttpResponse(req, resp); !ok {
		return fmtErrors(problems).Error()
	}
	return ""
}

// Check fails the test if the response doesn't match the contract.
func Check(t testing.TB, req *http.Request, resp *http.Response) {
	t.Helper()
	if problems := Errors(t, req, resp); problems != "" {
		t.Errorf("%s %s answered %d, which doesn't match the contract:\n%s",
			req.Method, req.URL.Path, resp.StatusCode, problems)
	}
}

// RequestExample returns the request body example the contract gives for an operation, as Node's
// contractRequestExample: proof that the contract's own example is accepted.
func RequestExample(t testing.TB, method, path string) map[string]any {
	t.Helper()
	model, err := loadModel()
	if err != nil {
		t.Fatalf("loading %s: %v", contractPath, err)
	}
	item := model.Paths.PathItems.GetOrZero(path)
	if item == nil {
		t.Fatalf("contract has no path %s", path)
	}
	operation := item.GetOperations().GetOrZero(strings.ToLower(method))
	if operation == nil || operation.RequestBody == nil {
		t.Fatalf("contract has no %s %s request body", method, path)
	}
	media := operation.RequestBody.Content.GetOrZero("application/json")
	if media == nil || media.Example == nil {
		t.Fatalf("contract has no example for %s %s", method, path)
	}
	var example map[string]any
	if err := media.Example.Decode(&example); err != nil {
		t.Fatal(err)
	}
	return example
}

// The events' contracts: contracts/events/<event_type>.v<schema_version>.json, plain JSON Schema
// 2020-12 (Node's test/contract.ts checks every published event against them). Compiled once per
// event type and version with the JSON Schema validator libopenapi-validator uses, formats
// asserted (date-time for occurred_at), as Node's Ajv with ajv-formats.
var (
	eventSchemasMu sync.Mutex
	eventSchemas   = map[string]*jsonschema.Schema{}
)

// eventSchema is the compiled contract for an event type and version. An error if there is none,
// which EventErrors reports as what's wrong with the payload: a payload with a misspelt event_type
// has no contract.
func eventSchema(eventType string, version int) (*jsonschema.Schema, error) {
	eventSchemasMu.Lock() // a mutex: tests in one package may run in parallel goroutines
	defer eventSchemasMu.Unlock()
	key := eventType + ".v" + strconv.Itoa(version)
	if schema, ok := eventSchemas[key]; ok {
		return schema, nil
	}
	path := filepath.Join(filepath.Dir(contractPath), "events", key+".json")
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no event contract for event_type %q, schema_version %d", eventType, version)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	doc, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(path, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	schema, err := compiler.Compile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	eventSchemas[key] = schema
	return schema, nil
}

// EventErrors returns what's wrong with an event payload (JSON), checked against the contract for
// its event_type and schema_version, or "" if it matches. A payload that names no contract (not an
// object, event_type not a string, schema_version not a whole number, or no contract for the pair)
// is a problem returned like any other.
func EventErrors(t testing.TB, payload []byte) string {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return "not JSON: " + err.Error()
	}
	object, ok := instance.(map[string]any)
	if !ok {
		return "not a JSON object"
	}
	eventType, ok := object["event_type"].(string)
	if !ok {
		return fmt.Sprintf("event_type is %v, not a string", object["event_type"])
	}
	// The jsonschema package decodes numbers as json.Number (text), so a version is checked as an
	// integer here, not converted from a float64.
	number, _ := object["schema_version"].(json.Number)
	version, err := strconv.Atoi(number.String())
	if err != nil {
		return fmt.Sprintf("schema_version is %v, not a whole number", object["schema_version"])
	}
	schema, err := eventSchema(eventType, version)
	if err != nil {
		return err.Error()
	}
	if err := schema.Validate(instance); err != nil {
		return err.Error()
	}
	return ""
}

// CheckEvent fails the test if the event payload doesn't match its contract.
func CheckEvent(t testing.TB, payload []byte) {
	t.Helper()
	if problems := EventErrors(t, payload); problems != "" {
		t.Errorf("event doesn't match its contract:\n%s\npayload: %s", problems, payload)
	}
}

// validationErrors joins the validator's errors into one.
type validationErrors []string

func (e validationErrors) Error() string { return strings.Join(e, "\n") }

func fmtErrors[E interface{ Error() string }](problems []E) error {
	var out validationErrors
	for _, p := range problems {
		out = append(out, "  - "+p.Error())
	}
	return out
}
