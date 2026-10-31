package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ingestion"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
)

// The provider webhooks: the files named webhook_*. The seam where another provider's adapter would
// plug in (slice 2 decision 38): each maps its provider's payloads onto the same provider-neutral
// ingestion functions. In Node a separate directory and Fastify plugin with its own error handler;
// here, files of the API's package that share its HTTP machinery and answer every framing error
// with the webhook contract's 400 (rejectWebhookBody). When a second provider comes, its adapter
// becomes a package of its own, and that machinery moves beneath both.

// webhookRoutes are the provider webhooks' handlers.
type webhookRoutes struct {
	// record records a notification in its own transaction: ingestion.RecordNotification in
	// db.InTx. A field so a test can replace it, to reach the outcome the real database can't
	// produce (Inconsistent), as Node's test stubs the pool.
	record func(ctx context.Context, n ingestion.Notification) (ingestion.Result, error)
}

func newWebhookRoutes(pool *pgxpool.Pool) webhookRoutes {
	return webhookRoutes{
		record: func(ctx context.Context, n ingestion.Notification) (ingestion.Result, error) {
			var result ingestion.Result
			err := db.InTx(ctx, pool, func(tx db.Tx) error {
				var err error
				result, err = ingestion.RecordNotification(ctx, tx, n)
				return err
			})
			return result, err
		},
	}
}

// rejectWebhookBody answers a framing error (non-JSON, oversized, malformed) with 400, the webhook
// contract's one 4xx (slice 2 decisions 37-38), whatever status the client API would use, and logs
// it as Node's error handler does.
func rejectWebhookBody(w http.ResponseWriter, r *http.Request, framing *framingError) {
	loggerFrom(r.Context()).Warn("request rejected",
		"status_code", http.StatusBadRequest, "reason", framing.detail)
	writeProblem(w, httpProblem(http.StatusBadRequest, framing.detail))
}

// decodeNotification decodes POST /provider/incoming-payments' body as Node's
// notificationBodySchema does, field by field in the schema's order. Unknown fields are ignored
// (a tolerant reader: providers add fields to webhooks over time); they are neither stored nor
// logged.
func decodeNotification(body []byte) (ingestion.Notification, []ValidationError) {
	f := newFields(body)
	if !f.valid() {
		return ingestion.Notification{}, f.errs
	}
	var n ingestion.Notification
	n.NetworkTransactionID, _ = f.stringLength("network_transaction_id", 1, 255)
	n.CreditorIBAN = decodeIBAN(f, "creditor_iban")
	n.ProviderAccountID = f.optionalStringLength("provider_account_id", 1, 255)
	n.Amount = decodeAmount(f, "amount_minor")
	n.Currency, _ = f.literal("currency", "EUR")
	n.Network, _ = f.literal("network", "SEPA")
	return n, f.errs
}

// decodeIBAN reads an IBAN field as Node's ibanSchema: a string of at most sepa.MaxIBANInput
// characters as received (with Zod's message), then sepa.ParseIBAN, whose reasons are Node's texts.
func decodeIBAN(f *fields, name string) sepa.IBAN {
	s, ok := f.rawString(name)
	if !ok {
		return ""
	}
	if zodLen(s) > sepa.MaxIBANInput {
		f.fail(name, "Too big: expected string to have <="+strconv.Itoa(sepa.MaxIBANInput)+" characters")
		return ""
	}
	iban, err := sepa.ParseIBAN(s)
	if err != nil {
		f.fail(name, "invalid IBAN: "+err.Error())
		return ""
	}
	return iban
}

// decodeAmount reads an amount field as Node's sepaAmountSchema (requests and notifications, slice 3
// decision 60): a string, then its length (at most money.MaxAmountLength) and its digits (no
// leading zero), both reported if both fail, as Zod does; only if both pass, the SEPA Credit
// Transfer maximum. sepa.ParseAmount decides acceptance (the shared vectors pin it); the checks
// before it only choose Zod's messages.
func decodeAmount(f *fields, name string) money.Amount {
	s, ok := f.rawString(name)
	if !ok {
		return 0
	}
	tooLong := zodLen(s) > money.MaxAmountLength
	if tooLong {
		f.fail(name, "Too big: expected string to have <="+strconv.Itoa(money.MaxAmountLength)+
			" characters")
	}
	amount, err := sepa.ParseAmount(s)
	switch {
	case err == nil && !tooLong:
		return amount
	case errors.Is(err, sepa.ErrAmountTooLarge) && !tooLong:
		f.fail(name, sepa.ErrAmountTooLarge.Error())
	case errors.Is(err, money.ErrNotPositiveWholeNumber) && !isPositiveDigits(s):
		f.fail(name, "must be a positive whole number of minor units, as a string")
	}
	return 0
}

// isPositiveDigits is Node's pattern alone, ^[1-9][0-9]*$, without the length limit: for choosing
// messages only (a 20-digit number fails the length, not the pattern).
func isPositiveDigits(s string) bool {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// POST /provider/incoming-payments: the provider's notification of an incoming payment (its
// webhook). A new deposit and its deposit.created event are stored in one transaction (the event in
// the outbox, published by the relay); this request never talks to NATS. Node:
// node/src/api/webhooks/incoming-payments.ts.
//
// The webhook contract (slice 2 decisions 37-38): a provider retries anything but a 2xx, so
//   - 204, no body, for everything recorded or deliberately handled: created, duplicate, unmatched
//     (recorded in suspense: money can't be refused over HTTP) and conflict (recorded). No outcome
//     in the response: the endpoint is no oracle for which accounts or deposits exist.
//   - 400 only for a body that can't be used, an invalid IBAN included: if our validation is the
//     problem, the provider's retries deliver the notification once it's fixed.
//   - 500 when a retry can help once something is fixed: our inconsistency, or a transient failure.
//
// Unmatched and conflicting notifications are logged as errors with alert: true: a person should
// look, but a non-2xx would only make the provider resend them.
func (h webhookRoutes) incomingPayment(w http.ResponseWriter, r *http.Request) error {
	logger := loggerFrom(r.Context())
	body, framing, err := readJSONBody(w, r)
	if err != nil {
		return err
	}
	if framing != nil {
		rejectWebhookBody(w, r, framing)
		return nil
	}
	notification, invalid := decodeNotification(body)
	if invalid != nil {
		logger.Warn("invalid deposit notification", "errors", invalid)
		writeProblem(w, invalidNotification(invalid))
		return nil
	}

	result, err := h.record(r.Context(), notification)
	if err != nil {
		return err
	}
	// The fields every outcome logs: the network ID and the IBAN masked, never in full. (Not named
	// context: that would hide the context package for the rest of the function.)
	logFields := []any{
		"network_transaction_id", notification.NetworkTransactionID,
		"creditor_iban", notification.CreditorIBAN.Mask(),
	}
	log := logger.With("deposit_id", result.Deposit)
	switch result.Kind {
	case ingestion.Created:
		log.Info("deposit created",
			append(logFields, "amount_minor", notification.Amount, "event_id", result.Event)...)
	case ingestion.Duplicate:
		log.Info("duplicate deposit notification", append(logFields, "duplicate", true,
			"status", result.Status, "notification_count", result.NotificationCount)...)
	case ingestion.Unmatched:
		log.Error("deposit notification matches none of our accounts; recorded in suspense",
			append(logFields, "alert", true, "amount_minor", notification.Amount)...)
	case ingestion.Conflict:
		// Field names only, never the values (received or stored).
		log.Error("deposit notification conflicts with the stored deposit; recorded, nothing changed",
			append(logFields, "alert", true, "differing_fields", result.DifferingFields)...)
	case ingestion.Inconsistent:
		// Our bug or our data, not the provider's: 500, which also makes the provider retry, so the
		// problem keeps surfacing until someone looks.
		log.Error("notification refused but no field differs from the stored deposit: "+
			"duplicate rule drift or a changed account IBAN", append(logFields, "alert", true)...)
		writeProblem(w, httpProblem(http.StatusInternalServerError, ""))
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
