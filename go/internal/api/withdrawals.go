package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/requests"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
	"github.com/guywillett/ledger-demo/go/internal/withdrawals"
)

// Client withdrawals. Node: node/src/api/withdrawals.ts. POST holds the funds in the request's own
// transaction (slice 2 decision 25), together with the withdrawal.held event in the outbox, so the
// client learns at once whether its withdrawal is accepted (201) or refused for insufficient funds
// (422). The relay publishes the event; nothing here talks to NATS.
//
// Logs never contain the creditor name or remittance text, and the IBAN only masked.

type withdrawalRoutes struct {
	pool *pgxpool.Pool
}

// withdrawalResponse is a withdrawal on the wire, field for field Node's (toWithdrawalResponse), in
// Node's order: type first (slice 4 decision 67), in every response that carries one. No
// provider_transfer_id: provider identifiers stay at the edge (decision 69).
type withdrawalResponse struct {
	Type           string             `json:"type"`
	ID             ids.WithdrawalID   `json:"id"`
	AccountID      ids.AccountID      `json:"account_id"`
	IdempotencyKey string             `json:"idempotency_key"`
	AmountMinor    money.Amount       `json:"amount_minor"`
	Currency       string             `json:"currency"`
	CreditorIBAN   sepa.IBAN          `json:"creditor_iban"`
	CreditorName   string             `json:"creditor_name"`
	RemittanceInfo *string            `json:"remittance_info"`
	Status         withdrawals.Status `json:"status"`
	FailureReason  *string            `json:"failure_reason"`
	CreatedAt      timestamp.Time     `json:"created_at"`
	UpdatedAt      timestamp.Time     `json:"updated_at"`
	// Only with ?include=history (see depositResponse.History).
	History *[]withdrawalStatusChangeResponse `json:"history,omitempty"`
}

// withdrawalStatusChangeResponse is a withdrawal's history row as a client sees it. Unlike a
// deposit's, its reason is shown: the only one recorded is on the change to failed, and it is the
// provider's (its transfer update's reason, or its refusal's), or a generic text of ours when it
// gave none; meant for the customer (slice 4 decision 76).
type withdrawalStatusChangeResponse struct {
	FromStatus *withdrawals.Status `json:"from_status"`
	ToStatus   withdrawals.Status  `json:"to_status"`
	Reason     *string             `json:"reason"`
	At         timestamp.Time      `json:"at"`
}

func toWithdrawalResponse(w withdrawals.Withdrawal) withdrawalResponse {
	return withdrawalResponse{
		Type:           "withdrawal",
		ID:             w.ID,
		AccountID:      w.Account,
		IdempotencyKey: w.IdempotencyKey,
		AmountMinor:    w.Amount,
		Currency:       w.Currency,
		CreditorIBAN:   w.CreditorIBAN,
		CreditorName:   w.CreditorName,
		RemittanceInfo: w.RemittanceInfo,
		Status:         w.Status,
		FailureReason:  w.FailureReason,
		CreatedAt:      timestamp.Time(w.CreatedAt),
		UpdatedAt:      timestamp.Time(w.UpdatedAt),
	}
}

// withdrawalListResponse is the withdrawals list's body (see listResponse).
type withdrawalListResponse = listResponse[withdrawalResponse, ids.WithdrawalID]

// idempotencyKeyPattern is Node's: 1-255 characters from a plain set (the key ends up in logs).
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,255}$`)

// idempotencyKeyHeader reads the Idempotency-Key header, as Node's idempotencyKeyHeadersSchema:
// required, and matching the pattern.
//
// A header sent twice is one value, the two joined with ", ": what Node's HTTP parser does with a
// repeated header (measured: both values, even equal ones, then fail the pattern). net/http keeps
// them apart (Header.Values), so they are joined here to answer alike.
func idempotencyKeyHeader(r *http.Request) (string, []ValidationError) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", []ValidationError{{Path: "idempotency-key", Message: "is required"}}
	}
	key := strings.Join(values, ", ")
	if !idempotencyKeyPattern.MatchString(key) {
		return "", []ValidationError{{
			Path: "idempotency-key", Message: "must be 1-255 characters from A-Z a-z 0-9 . _ : -",
		}}
	}
	return key, nil
}

// withdrawalBody is the decoded POST body.
type withdrawalBody struct {
	amount         money.Amount
	creditorIBAN   sepa.IBAN
	creditorName   string
	remittanceInfo *string
}

// The body's fields, in Node's schema order.
var withdrawalFields = []string{
	"amount_minor", "currency", "creditor_iban", "creditor_name", "remittance_info",
}

// decodeWithdrawalBody decodes the body as Node's withdrawalBodySchema, a strict object: every
// field's problems in the schema's order, then any unknown key (measured: Zod reports them after
// the fields, wherever they are in the body). Strict, unlike the provider webhook: this is our own
// client's request to move money, so a misspelt optional field (remitance_info) must fail rather
// than be dropped (slice 2 decision 33).
func decodeWithdrawalBody(body []byte) (withdrawalBody, []ValidationError) {
	f := newFields(body)
	if !f.valid() {
		return withdrawalBody{}, f.errs
	}
	var b withdrawalBody
	b.amount = decodeAmount(f, "amount_minor")
	f.literal("currency", "EUR")
	b.creditorIBAN = decodeIBAN(f, "creditor_iban")
	b.creditorName = decodeSEPAText(f, "creditor_name", 70)
	b.remittanceInfo = decodeOptionalSEPAText(f, "remittance_info", 140)
	f.rejectUnknown(withdrawalFields...)
	return b, f.errs
}

// decodeSEPAText reads required SEPA text of at most maxLen characters, as Node's sepaTextSchema:
// separators trimmed from both ends, then blank (that problem alone, Zod's abort), else at most
// maxLen characters and the basic character set, both reported if both fail. sepa.ParseText decides
// acceptance (the shared vectors pin it); the checks before it only choose Zod's messages.
func decodeSEPAText(f *fields, name string, maxLen int) string {
	s, ok := f.rawString(name)
	if !ok {
		return ""
	}
	text := sepa.TrimText(s)
	if text == "" {
		f.fail(name, "must not be blank")
		return ""
	}
	failed := false
	if zodLen(text) > maxLen {
		f.fail(name, fmt.Sprintf("must be at most %d characters", maxLen))
		failed = true
	}
	if !sepa.IsBasicText(text) {
		f.fail(name, sepa.ErrCharacters.Error())
		failed = true
	}
	parsed, err := sepa.ParseText(s, maxLen)
	if failed || err != nil {
		return ""
	}
	return parsed
}

// decodeOptionalSEPAText reads optional SEPA text, as Node's optionalSepaTextSchema: absent, null,
// empty or only separators all mean none (nil); another type is a problem; otherwise as
// decodeSEPAText.
func decodeOptionalSEPAText(f *fields, name string, maxLen int) *string {
	raw, present := f.object[name]
	if !present || jsonKind(raw) == "null" {
		return nil
	}
	s, ok := f.rawString(name)
	if !ok || sepa.TrimText(s) == "" {
		return nil
	}
	text := decodeSEPAText(f, name, maxLen)
	if text == "" {
		return nil
	}
	return &text
}

// rejectClientBody answers a framing error (non-JSON, oversized, malformed) with the client API's
// exact status (415, 413 or 400), and logs it as Node's error handler does. The webhooks answer
// every one with 400 instead (rejectWebhookBody).
func rejectClientBody(w http.ResponseWriter, r *http.Request, framing *framingError) {
	loggerFrom(r.Context()).Warn("request rejected",
		"status_code", framing.status, "reason", framing.detail)
	writeProblem(w, httpProblem(framing.status, framing.detail))
}

// POST /accounts/{account_id}/withdrawals, with an Idempotency-Key header.
func (h withdrawalRoutes) create(w http.ResponseWriter, r *http.Request) error {
	logger := loggerFrom(r.Context())
	// The body first: Fastify parses it before any handler runs, so a framing error comes before
	// every other problem.
	body, framing, err := readJSONBody(w, r)
	if err != nil {
		return err
	}
	if framing != nil {
		rejectClientBody(w, r, framing)
		return nil
	}

	// Every problem with the request at once: path, header and body, in that order.
	account, pathErrs := idParam[ids.Account](r, "account_id")
	key, headerErrs := idempotencyKeyHeader(r)
	decoded, bodyErrs := decodeWithdrawalBody(body)
	// A new slice: appending to pathErrs could write into its backing array (append reuses spare
	// capacity), so the three are copied into one of their own.
	errs := make([]ValidationError, 0, len(pathErrs)+len(headerErrs)+len(bodyErrs))
	errs = append(append(append(errs, pathErrs...), headerErrs...), bodyErrs...)
	if len(errs) > 0 {
		logger.Warn("invalid withdrawal request", "errors", errs)
		writeProblem(w, invalidRequest(errs))
		return nil
	}

	request := withdrawals.NewWithdrawal{
		Account:        account,
		IdempotencyKey: key,
		Amount:         decoded.amount,
		CreditorIBAN:   decoded.creditorIBAN,
		CreditorName:   decoded.creditorName,
		RemittanceInfo: decoded.remittanceInfo,
	}
	log := logger.With("account_id", account, "idempotency_key", key)

	var result requests.Result
	err = db.InTx(r.Context(), h.pool, func(tx db.Tx) error {
		var err error
		result, err = requests.RequestWithdrawal(r.Context(), tx, request)
		return err
	})
	// Insufficient funds and an unknown account come back from inside the transaction and are told
	// apart here, outside it: by then it has rolled back, nothing is stored and the key is still
	// free. errors.AsType finds the error anywhere in the chain of wrapped errors.
	if _, ok := errors.AsType[*ledger.InsufficientFundsError](err); ok {
		// A business outcome, not a problem with our system or a client bug.
		log.Info("withdrawal refused: insufficient funds", "amount_minor", request.Amount)
		writeProblem(w, insufficientFunds())
		return nil
	}
	if _, ok := errors.AsType[*withdrawals.AccountNotFoundError](err); ok {
		log.Warn("withdrawal request for an unknown account")
		writeProblem(w, accountNotFound())
		return nil
	}
	if err != nil {
		return err
	}

	switch result.Kind {
	case requests.Created:
		created := result.Withdrawal
		log.Info("withdrawal created", "withdrawal_id", created.ID, "amount_minor", created.Amount,
			"creditor_iban", created.CreditorIBAN.Mask(), "event_id", result.Event)
		w.Header().Set("Location",
			"/accounts/"+created.Account.TypeID()+"/withdrawals/"+created.ID.TypeID())
		writeJSON(w, http.StatusCreated, "application/json; charset=utf-8",
			toWithdrawalResponse(created))
	case requests.Repeat:
		log.Info("repeated withdrawal request",
			"withdrawal_id", result.Withdrawal.ID, "status", result.Withdrawal.Status)
		writeJSON(w, http.StatusOK, "application/json; charset=utf-8",
			toWithdrawalResponse(result.Withdrawal))
	case requests.Conflict:
		// The client reused a key for another withdrawal: its bug, not ours.
		log.Warn("idempotency key reused with different details",
			"withdrawal_id", result.Withdrawal.ID, "differing_fields", result.DifferingFields)
		writeProblem(w, idempotencyKeyConflict(result.DifferingFields))
	default:
		return fmt.Errorf("unexpected withdrawal request result %d", result.Kind)
	}
	return nil
}

// withdrawalsQuery is the list's validated ?limit, ?before, ?include, and ?status.
type withdrawalsQuery struct {
	limit   int
	before  *ids.WithdrawalID
	history bool
	status  *withdrawals.Status
}

// parseWithdrawalsQuery validates the withdrawals list's query, as Node's withdrawalsQuerySchema:
// the list's limit, before (a withdrawal's ID) and include (query.go), then its status filter
// (decision 70), one of withdrawals.Statuses(). Unknown parameters are ignored. Every problem is
// reported at once, in the contract's parameter order (slice 4 decision 68).
func parseWithdrawalsQuery(raw string) (withdrawalsQuery, []ValidationError) {
	query := parseQuery(raw)
	parsed := withdrawalsQuery{}
	var errs, invalid []ValidationError

	parsed.limit, invalid = parseLimit(query)
	errs = append(errs, invalid...)

	parsed.before, invalid = parseBefore[ids.Withdrawal](query)
	errs = append(errs, invalid...)

	parsed.history, invalid = includeHistory(query)
	errs = append(errs, invalid...)

	parsed.status, invalid = parseStatusFilter(query, withdrawals.Statuses())
	errs = append(errs, invalid...)

	return parsed, errs
}

// withWithdrawalHistory adds the withdrawal's history (nil for none, written as []), its reasons
// shown (see withdrawalStatusChangeResponse). Node: toWithdrawalResponse's history.
func withWithdrawalHistory(
	item withdrawalResponse,
	changes []withdrawals.StatusChange,
) withdrawalResponse {
	history := make([]withdrawalStatusChangeResponse, 0, len(changes))
	for _, c := range changes {
		history = append(history, withdrawalStatusChangeResponse{
			FromStatus: c.From, ToStatus: c.To, Reason: c.Reason, At: timestamp.Time(c.At),
		})
	}
	item.History = &history
	return item
}

// GET /accounts/{account_id}/withdrawals/{withdrawal_id}: the withdrawal in its current status,
// read with its account in one query: another account's withdrawal is a 404, the same as one that
// doesn't exist, whether or not the account exists (decision 64). ?include=history adds its status
// history. Every problem with the request at once, the IDs then the query (decision 68).
func (h withdrawalRoutes) get(w http.ResponseWriter, r *http.Request) error {
	account, accountErrs := idParam[ids.Account](r, "account_id")
	id, idErrs := idParam[ids.Withdrawal](r, "withdrawal_id")
	history, queryErrs := includeHistory(parseQuery(r.URL.RawQuery))
	if invalid := slices.Concat(accountErrs, idErrs, queryErrs); len(invalid) > 0 {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}

	withdrawal, found, err := withdrawals.GetInAccount(r.Context(), h.pool, account, id)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, withdrawalNotFound())
		return nil
	}
	body := toWithdrawalResponse(withdrawal)
	if history {
		changes, err := withdrawals.StatusHistories(
			r.Context(), h.pool, []ids.WithdrawalID{withdrawal.ID})
		if err != nil {
			return err
		}
		body = withWithdrawalHistory(body, changes[withdrawal.ID])
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", body)
	return nil
}

// GET /accounts/{account_id}/withdrawals: newest first, a page at a time (?limit,
// ?before=<next_before from the previous page>), as {data, next_before} (decision 66).
// ?include=history adds each withdrawal's status history, read for the whole page in one query.
// Every problem with the request at once, the path then the query (decision 68). A list checks
// its account: an unknown one is a 404, an empty list an account without withdrawals.
func (h withdrawalRoutes) list(w http.ResponseWriter, r *http.Request) error {
	account, pathErrs := idParam[ids.Account](r, "account_id")
	query, queryErrs := parseWithdrawalsQuery(r.URL.RawQuery)
	if invalid := slices.Concat(pathErrs, queryErrs); len(invalid) > 0 {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}
	filter := withdrawals.Filter{
		Status: query.status,
	}

	page, found, err := withdrawals.ListPage(
		r.Context(), h.pool, account, query.limit, query.before, filter)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, accountNotFound())
		return nil
	}

	var histories map[ids.WithdrawalID][]withdrawals.StatusChange
	if query.history {
		withdrawalIDs := make([]ids.WithdrawalID, len(page.Withdrawals))
		for i, wd := range page.Withdrawals {
			withdrawalIDs[i] = wd.ID
		}
		if histories, err = withdrawals.StatusHistories(r.Context(), h.pool, withdrawalIDs); err != nil {
			return err
		}
	}

	response := withdrawalListResponse{
		// [] rather than null when empty.
		Data:       make([]withdrawalResponse, 0, len(page.Withdrawals)),
		NextBefore: page.NextBefore,
	}
	for _, wd := range page.Withdrawals {
		item := toWithdrawalResponse(wd)
		if query.history {
			item = withWithdrawalHistory(item, histories[wd.ID])
		}
		response.Data = append(response.Data, item)
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", response)
	return nil
}
