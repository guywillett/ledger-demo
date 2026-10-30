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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
)

// contractPath is the contract, found from this source file's own location (runtime.Caller gives
// the path the file was compiled from) rather than from the test's working directory, which is
// whichever package is being tested: so it works from a package at any depth (internal/api/webhooks
// in step 4b). go test doesn't use -trimpath, so the path is a real one.
var contractPath = func() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "../../../contracts/openapi.yaml")
}()

// load builds the validator once per test binary, however many tests use it. sync.OnceValues runs
// the function the first time it's called and returns the same results every time after,
// goroutine-safely.
var load = sync.OnceValues(func() (validator.Validator, error) {
	spec, err := os.ReadFile(contractPath)
	if err != nil {
		return nil, err
	}
	document, err := libopenapi.NewDocument(spec)
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
