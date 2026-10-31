package api

import (
	"net/http"

	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ingestion"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/logging"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// decodeTransferUpdate decodes POST /provider/transfer-updates' body as Node's
// transferUpdateBodySchema, field by field in the schema's order, tolerantly (unknown fields
// ignored, as the deposit webhook). Then, only if every field passed (Zod runs an object's
// .refine on valid values only), the rule across fields: a reason exactly for failed.
func decodeTransferUpdate(body []byte) (ingestion.TransferUpdate, []ValidationError) {
	f := newFields(body)
	if !f.valid() {
		return ingestion.TransferUpdate{}, f.errs
	}
	var u ingestion.TransferUpdate
	u.ProviderEventID, _ = f.stringLength("provider_event_id", 1, 255)
	transferID, _ := f.stringLength("transfer_id", 1, 255)
	u.TransferID = withdrawals.TransferID(transferID)
	u.Reference, _ = f.stringLength("reference", 1, 255)
	status, _ := f.enum("status",
		string(withdrawals.StatusExecuted), string(withdrawals.StatusFailed))
	u.Status = withdrawals.Status(status)
	u.Reason = f.optionalStringLength("reason", 1, 1000)
	if s, ok := f.rawString("occurred_at"); ok {
		t, valid := timestamp.ParseISO(s)
		if !valid {
			f.fail("occurred_at", "Invalid ISO datetime")
		}
		u.OccurredAt = t
	}
	if len(f.errs) > 0 {
		return ingestion.TransferUpdate{}, f.errs
	}
	if (u.Status == withdrawals.StatusFailed) != (u.Reason != nil) {
		f.fail("reason", "is required for failed, and must be absent for executed")
		return ingestion.TransferUpdate{}, f.errs
	}
	// The reference as a withdrawal ID if it is a wd_ TypeID holding a UUIDv7; otherwise nil, and
	// the update is recorded as unmatched, not refused.
	if id, err := ids.FromTypeID[ids.Withdrawal](u.Reference); err == nil {
		u.WithdrawalID = &id
	}
	return u, nil
}

// POST /provider/transfer-updates: the provider's result for a transfer we sent (its webhook).
// Recorded and settled in one transaction (ingestion.RecordTransferUpdate). Node:
// node/src/api/webhooks/transfer-updates.ts.
//
// The webhook contract (slice 2 decision 37): a provider retries anything but a 2xx, so
//   - 204, no body, for everything recorded or deliberately handled: applied, duplicate,
//     conflict, event conflict, unmatched. No outcome in the response: echoing it would let any
//     caller probe which withdrawals exist and their state, and providers ignore webhook bodies.
//   - 400 only for a body that doesn't parse: if the bug is ours, the provider's retries deliver
//     the update once it's fixed.
//   - 500 when a retry can help once something is fixed: an invariant violation (rolled back,
//     alerted) or a transient failure.
//
// Contradictions and unmatched updates are logged as errors with alert: true: a person should
// look, but answering non-2xx would only make the provider resend them.
func (h webhookRoutes) transferUpdate(w http.ResponseWriter, r *http.Request) error {
	logger := loggerFrom(r.Context())
	body, framing, err := readJSONBody(w, r)
	if err != nil {
		return err
	}
	if framing != nil {
		rejectWebhookBody(w, r, framing)
		return nil
	}
	update, invalid := decodeTransferUpdate(body)
	if invalid != nil {
		logger.Warn("invalid transfer update", "errors", invalid)
		writeProblem(w, invalidTransferUpdate(invalid))
		return nil
	}

	fields := []any{
		"provider_event_id", update.ProviderEventID,
		"transfer_id", update.TransferID,
		"status", update.Status,
	}
	if update.WithdrawalID != nil {
		fields = append(fields, "withdrawal_id", *update.WithdrawalID)
	}
	log := logger.With(fields...)

	result, err := h.recordUpdate(r.Context(), update)
	if err != nil {
		if ledger.IsInvariantViolation(err) {
			// A ledger bug: rolled back, nothing recorded. 500, so the provider keeps resending it
			// (and it stays visible) until someone has looked.
			logging.Fatal(r.Context(), log, "invariant violated applying a transfer update",
				"err", err, "alert", true)
			writeProblem(w, httpProblem(http.StatusInternalServerError, ""))
			return nil
		}
		return err
	}

	switch result.Kind {
	case ingestion.UpdateApplied:
		log.Info("transfer update applied")
	case ingestion.UpdateDuplicate:
		log.Info("duplicate transfer update", "notification_count", result.NotificationCount)
	case ingestion.UpdateConflict:
		log.Error("transfer update contradicts the withdrawal; recorded, nothing moved",
			"alert", true, "stored_status", result.Stored.Status,
			"stored_transfer_id", result.Stored.TransferID)
	case ingestion.UpdateEventConflict:
		log.Error("provider reused an event ID with other details; nothing changed", "alert", true)
	case ingestion.UpdateUnmatched:
		log.Error("transfer update matches no withdrawal; recorded",
			"alert", true, "reference", update.Reference)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
