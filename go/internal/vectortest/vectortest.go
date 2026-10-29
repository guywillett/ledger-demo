// Package vectortest reads the shared test vectors (contracts/vectors/, see its README) for Go's
// tests. Imported only by tests, like the standard library's httptest.
package vectortest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Case is one vector. Input and Value stay raw JSON (json.RawMessage holds the bytes undecoded),
// so each test decodes them into the types a request body is decoded into: the decoding is part
// of what's tested. Why is for readers and never asserted.
type Case struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Why   string          `json:"why"`
}

// dir is the vectors directory relative to a package directory: `go test` runs each package's
// tests in that package's directory (go/internal/<package>), and go:embed, the usual way to ship
// test data, can't reach files outside the module (go/).
const dir = "../../../contracts/vectors"

// Load reads one vector file, failing the test if it can't, or if it holds no cases.
func Load(t testing.TB, file string) []Case {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	if len(cases) == 0 {
		t.Fatalf("%s has no cases", file)
	}
	return cases
}

// Run runs fn as a subtest per case, named by the case, and checks its outcome: for an ok case,
// that fn succeeds and its result, written as JSON, equals the case's value; otherwise that fn
// fails. fn returns the parsed result in its JSON form (e.g. an amount as "1250").
func Run(t *testing.T, file string, fn func(input json.RawMessage) (any, error)) {
	for _, c := range Load(t, file) {
		t.Run(c.Name, func(t *testing.T) {
			got, err := fn(c.Input)
			if !c.OK {
				if err == nil {
					t.Errorf("input %s: accepted as %v, want rejected (%s)", c.Input, got, c.Why)
				}
				return
			}
			if err != nil {
				t.Fatalf("input %s: rejected (%v), want %s", c.Input, err, c.Value)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshalling the result: %v", err)
			}
			if !equalJSON(t, gotJSON, c.Value) {
				t.Errorf("input %s: got %s, want %s", c.Input, gotJSON, c.Value)
			}
		})
	}
}

// equalJSON compares two JSON documents by value, not by spelling (spacing, escapes).
func equalJSON(t testing.TB, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	// Decoded JSON is maps, slices, strings, float64s, bools and nil; re-encoding sorts map keys,
	// so equal values give equal bytes.
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}
