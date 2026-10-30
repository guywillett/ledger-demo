package api

import (
	"net/http"
)

// RFC 9457 problem details. Type identifies the kind of problem: a URN for our own types (an
// identifier, not a URL that pretends to resolve), or "about:blank" for plain HTTP errors, where
// the title is the HTTP status phrase. Node: node/src/api/problem.ts.
//
// The fields are in Node's order, so the JSON's keys are too; omitempty leaves out what a problem
// doesn't have (only 400s have errors).
type Problem struct {
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Status int               `json:"status"`
	Detail string            `json:"detail,omitempty"`
	Errors []ValidationError `json:"errors,omitempty"`
}

// ValidationError is one entry of a 400's errors: which input and what's wrong, never the input's
// value (it comes from the request).
type ValidationError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Every problem this API answers with, each defined once, as Node's problems object: the type,
// title and status live here, and call sites pass only what varies. The contract
// (contracts/openapi.yaml) documents the same set. The webhook and withdrawal problems come with
// their endpoints (steps 4b and 7).

func invalidRequest(errs []ValidationError) Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:invalid-request",
		Title:  "Invalid request",
		Status: http.StatusBadRequest,
		Errors: errs,
	}
}

func accountNotFound() Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:account-not-found",
		Title:  "Account not found",
		Status: http.StatusNotFound,
	}
}

// httpProblem is a plain HTTP error (unknown routes, unexpected 500s). http.StatusText gives the
// same phrases as Node's STATUS_CODES ("Not Found", "Internal Server Error").
func httpProblem(status int, detail string) Problem {
	return Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail}
}

// writeProblem answers with the problem, as application/problem+json.
func writeProblem(w http.ResponseWriter, p Problem) {
	writeJSON(w, p.Status, "application/problem+json; charset=utf-8", p)
}
