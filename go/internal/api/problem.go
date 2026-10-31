package api

import (
	"net/http"

	"github.com/guywillett/ledger-demo/go/internal/requests"
)

// RFC 9457 problem details. Type identifies the kind of problem: a URN for our own types (an
// identifier, not a URL that pretends to resolve), or "about:blank" for plain HTTP errors, where
// the title is the HTTP status phrase. Node: node/src/api/problem.ts.
//
// The fields are in Node's order, so the JSON's keys are too; omitempty leaves out what a problem
// doesn't have (only 400s have errors, only the idempotency key conflict differing_fields).
type Problem struct {
	Type            string                   `json:"type"`
	Title           string                   `json:"title"`
	Status          int                      `json:"status"`
	Detail          string                   `json:"detail,omitempty"`
	DifferingFields []requests.ComparedField `json:"differing_fields,omitempty"`
	Errors          []ValidationError        `json:"errors,omitempty"`
}

// ValidationError is one entry of a 400's errors: which input and what's wrong, never the input's
// value (it comes from the request).
type ValidationError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Every problem this API answers with, each defined once, as Node's problems object: the type,
// title and status live here, and call sites pass only what varies. The contract
// (contracts/openapi.yaml) documents the same set.

func invalidRequest(errs []ValidationError) Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:invalid-request",
		Title:  "Invalid request",
		Status: http.StatusBadRequest,
		Errors: errs,
	}
}

// invalidNotification: a deposit notification (the provider's webhook) whose body doesn't parse.
func invalidNotification(errs []ValidationError) Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:invalid-notification",
		Title:  "Invalid deposit notification",
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

func withdrawalNotFound() Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:withdrawal-not-found",
		Title:  "Withdrawal not found",
		Status: http.StatusNotFound,
	}
}

func insufficientFunds() Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:insufficient-funds",
		Title:  "Insufficient funds",
		Status: http.StatusUnprocessableEntity,
		Detail: "The available balance does not cover the withdrawal. Nothing was stored, so the same " +
			"Idempotency-Key can be used again.",
	}
}

func idempotencyKeyConflict(differing []requests.ComparedField) Problem {
	return Problem{
		Type:   "urn:ledger-demo:problem:idempotency-key-conflict",
		Title:  "Idempotency key reused with different details",
		Status: http.StatusConflict,
		Detail: "This account already has a withdrawal with this Idempotency-Key and different " +
			"details. Use a new key for a new withdrawal.",
		DifferingFields: differing,
	}
}

// httpProblem is a plain HTTP error (unknown routes, body framing errors, unexpected 500s), its
// title Node's phrase for the status (Node's STATUS_CODES).
func httpProblem(status int, detail string) Problem {
	return Problem{Type: "about:blank", Title: statusPhrase(status), Status: status, Detail: detail}
}

// statusPhrase is Node's phrase for a status. http.StatusText gives the same phrases for every
// status this API answers with but 413: Go keeps RFC 7231's "Request Entity Too Large", Node has
// RFC 9110's "Payload Too Large" (measured: Node's 413 problem's title).
func statusPhrase(status int) string {
	if status == http.StatusRequestEntityTooLarge {
		return "Payload Too Large"
	}
	return http.StatusText(status)
}

// writeProblem answers with the problem, as application/problem+json.
func writeProblem(w http.ResponseWriter, p Problem) {
	writeJSON(w, p.Status, "application/problem+json; charset=utf-8", p)
}
