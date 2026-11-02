package submitter

import "github.com/guywillett/ledger-demo/go/internal/withdrawals"

// The submitter's vocabulary for what a provider said about a submission (Node: SubmitOutcome in
// submitter/provider.ts). Defined here, with the code that acts on it, rather than in the provider
// package: what each outcome means for a withdrawal (submit it, release its funds, ask again, stop
// as our bug) is the submitter's decision, the same whichever provider answered. A provider's
// adapter (package provider, for provider-sim's API) translates its own answers into these kinds
// and depends on this package, never the reverse; a second provider would be a second adapter
// producing the same Outcome, with the handler unchanged.

// Kind is what a provider's answer to a submission means.
type Kind int

const (
	// Accepted: the provider has the transfer (TransferID), new or a repeat of our key.
	Accepted Kind = iota + 1
	// Refused: a permanent refusal (Reason); nothing was created at the provider. Only this
	// releases the funds.
	Refused
	// Invalid: the provider rejected our request as malformed: a bug on our side. Only a summary
	// of its answer, made by the adapter: a real provider may echo request values (an IBAN, a
	// name) in its errors, and those must not reach the logs. Fields: the field paths its errors
	// name, kept only if they are plain identifiers; BodyBytes: the answer's size.
	Invalid
	// KeyConflict: the provider reports our idempotency key reused with another request. We always
	// resend the identical request, so this is a bug. (An adapter whose provider also reports a key
	// still in progress this way maps that case to Unknown; see Known gaps in the slice 2 spec.)
	KeyConflict
	// Unknown: no usable answer (a timeout, a network error, a server error, an answer we can't
	// read), with its Cause. The important kind: the provider may or may not have created the
	// transfer (a timeout after the request was sent is the classic case), so the outcome is never
	// guessed (slice 2 decision 26). The call is made again later with the same Idempotency-Key,
	// and the provider's idempotency turns the retry into "tell me what happened to this one".
	// Never a new key for the same withdrawal.
	Unknown
)

// Outcome is a submission's outcome: one struct with a Kind, the fields a Kind doesn't use zero
// (Node: a union).
type Outcome struct {
	Kind       Kind
	TransferID withdrawals.TransferID // Accepted
	Reason     string                 // Refused
	Fields     []string               // Invalid
	BodyBytes  int                    // Invalid
	Cause      string                 // Unknown
}
