package api

import (
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// An account's deposits, as a list and singly, within the account (slice 4 decision 64: every
// query takes the account). Node: node/src/api/deposits.ts. No authentication yet (see Known gaps).

type depositRoutes struct {
	db db.Querier
}

// The deposit as a client sees it, field for field Node's DepositResponse
// (node/src/api/schemas.ts) in Node's order: type first (decision 67), no provider_account_id (a
// provider identifier, decision 69), failure_reason after status.
type depositResponse struct {
	Type                 string          `json:"type"`
	ID                   ids.DepositID   `json:"id"`
	AccountID            ids.AccountID   `json:"account_id"`
	CreditorIBAN         sepa.IBAN       `json:"creditor_iban"`
	AmountMinor          money.Amount    `json:"amount_minor"`
	Currency             string          `json:"currency"`
	Network              string          `json:"network"`
	NetworkTransactionID string          `json:"network_transaction_id"`
	Status               deposits.Status `json:"status"`
	FailureReason        *string         `json:"failure_reason"`
	NotificationCount    int             `json:"notification_count"`
	CreatedAt            timestamp.Time  `json:"created_at"`
	UpdatedAt            timestamp.Time  `json:"updated_at"`
	// Only with ?include=history. A pointer to a slice, so "absent" (nil, left out by omitempty)
	// differs from "no rows" (an empty slice, written as []), as Node's optional history does.
	History *[]depositStatusChangeResponse `json:"history,omitempty"`
}

// depositStatusChangeResponse is a deposit's history row as a client sees it. Reason is always
// null: the only reasons a deposit's history holds are the compliance decision's, which are
// internal (slice 4 decision 76: telling a customer why a payment was rejected can be tipping
// off). Hidden here, at the boundary, rather than by transition: a reason added to another
// transition later stays hidden until it is deliberately made client-facing. Node:
// toDepositStatusChangeResponse.
type depositStatusChangeResponse struct {
	FromStatus *deposits.Status `json:"from_status"`
	ToStatus   deposits.Status  `json:"to_status"`
	Reason     *string          `json:"reason"` // always nil (see above)
	At         timestamp.Time   `json:"at"`
}

type depositListResponse struct {
	Data       []depositResponse `json:"data"`
	NextBefore *ids.DepositID    `json:"next_before"`
}

// depositFailureReason is the one client-facing reason for a deposit that failed (slice 4 decisions
// 69 and 76): the same fixed, non-disclosing text for rejected and returned, whatever the internal
// reason. A rejected deposit is returned next (the ledger's return posting), so the text is true
// of both. Node: DEPOSIT_FAILURE_REASON.
const depositFailureReason = "returned to the sender"

// toDepositResponse maps a deposit to its client representation, without history (see
// withHistory). failure_reason is set from the status alone.
func toDepositResponse(d deposits.Deposit) depositResponse {
	var failureReason *string
	if d.Status == deposits.StatusRejected || d.Status == deposits.StatusReturned {
		reason := depositFailureReason
		failureReason = &reason
	}
	return depositResponse{
		Type:                 "deposit",
		ID:                   d.ID,
		AccountID:            d.Account,
		CreditorIBAN:         d.CreditorIBAN,
		AmountMinor:          d.Amount,
		Currency:             d.Currency,
		Network:              d.Network,
		NetworkTransactionID: d.NetworkTransactionID,
		Status:               d.Status,
		FailureReason:        failureReason,
		NotificationCount:    d.NotificationCount,
		CreatedAt:            timestamp.Time(d.CreatedAt),
		UpdatedAt:            timestamp.Time(d.UpdatedAt),
	}
}

// withHistory adds the deposit's history (as deposits.StatusHistories returns it for the deposit:
// nil for none, written as []), its reasons left out (see depositStatusChangeResponse).
func withHistory(item depositResponse, changes []deposits.StatusChange) depositResponse {
	history := make([]depositStatusChangeResponse, 0, len(changes))
	for _, c := range changes {
		history = append(history, depositStatusChangeResponse{
			FromStatus: c.From, ToStatus: c.To, At: timestamp.Time(c.At),
		})
	}
	item.History = &history
	return item
}

// depositParam parses the {deposit_id} path value, as Node's depositParamsSchema does it (and
// accountParam an account's).
func depositParam(r *http.Request) (ids.DepositID, []ValidationError) {
	deposit, err := ids.FromTypeID[ids.Deposit](r.PathValue("deposit_id"))
	if err != nil {
		return ids.DepositID{}, []ValidationError{{Path: "deposit_id", Message: "must be a dep_ ID"}}
	}
	return deposit, nil
}

// depositsQuery is the list's validated ?limit, ?before, ?include, ?status and
// ?network_transaction_id.
type depositsQuery struct {
	limit                int
	before               *ids.DepositID
	history              bool
	status               *deposits.Status
	networkTransactionID *string
}

// statusFilterMessage: a ValidationError message with all deposits.BookedStatuses values
// correctly formatted.
var statusFilterMessage = func() string {
	statuses := deposits.BookedStatuses()
	quoted := make([]string, len(statuses))
	for i, s := range statuses {
		quoted[i] = `"` + string(s) + `"`
	}
	return "Invalid option: expected one of " + strings.Join(quoted, "|")
}()

// parseDepositsQuery validates the query as Node's depositsQuerySchema: limit a whole number from 1
// to 200 (default 50), read as JavaScript's Number() reads it (see jsNumber); before a deposit's
// TypeID; include "history". Unknown parameters are ignored. A parameter given twice is invalid
// (Node's parser makes it an array, which no rule accepts). Every problem is reported, in the
// contract's parameter order (slice 4 decision 68), as Node's schema lists its keys; the messages
// are Zod's, though only the paths are part of the contract.
func parseDepositsQuery(raw string) (depositsQuery, []ValidationError) {
	query := parseQuery(raw)
	parsed := depositsQuery{limit: 50}
	var errs []ValidationError

	if values, ok := query["limit"]; ok {
		n := math.NaN()
		if len(values) == 1 {
			n = jsNumber(values[0])
		}
		invalid := limitErrors(n)
		errs = append(errs, invalid...)
		if len(invalid) == 0 {
			parsed.limit = int(n)
		}
	}

	if values, ok := query["before"]; ok {
		before, err := ids.FromTypeID[ids.Deposit](values[0])
		if len(values) == 1 && err == nil {
			parsed.before = &before
		} else {
			errs = append(errs, ValidationError{Path: "before", Message: "must be a dep_ ID"})
		}
	}

	history, invalid := includeHistory(query)
	parsed.history = history
	errs = append(errs, invalid...)

	if values, ok := query["status"]; ok {
		s := deposits.Status(values[0])
		if len(values) == 1 && slices.Contains(deposits.BookedStatuses(), s) {
			parsed.status = &s
		} else {
			errs = append(errs, ValidationError{Path: "status", Message: statusFilterMessage})
		}
	}

	if values, ok := query["network_transaction_id"]; ok {
		length := zodLen(values[0])
		path := "network_transaction_id"
		switch {
		case len(values) > 1:
			errs = append(errs, ValidationError{Path: path, Message: "Invalid input: expected string, received array"})
		case length == 0:
			errs = append(errs, ValidationError{Path: path, Message: "Too small: expected string to have >=1 characters"})
		case length > 255:
			errs = append(errs, ValidationError{Path: path, Message: "Too big: expected string to have <=255 characters"})
		default:
			parsed.networkTransactionID = &values[0]
		}
	}
	return parsed, errs
}

// GET /accounts/{account_id}/deposits/{deposit_id}: the deposit itself, as a client sees it.
func (d depositRoutes) get(w http.ResponseWriter, r *http.Request) error {
	account, acctErrs := accountParam(r)
	depositID, depErrs := depositParam(r)
	history, queryErrs := includeHistory(parseQuery(r.URL.RawQuery))
	if invalid := slices.Concat(acctErrs, depErrs, queryErrs); len(invalid) > 0 {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}
	deposit, found, err := deposits.Get(r.Context(), d.db, account, depositID)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, depositNotFound())
		return nil
	}
	body := toDepositResponse(deposit)
	if history {
		histories, err := deposits.StatusHistories(r.Context(), d.db, []ids.DepositID{deposit.ID})
		if err != nil {
			return err
		}
		body = withHistory(body, histories[deposit.ID])
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", body)
	return nil
}

// GET /accounts/{account_id}/deposits: newest first, a page at a time (?limit,
// ?before=<next_before from the previous page>), as {data, next_before} (decision 66).
// ?include=history adds each deposit's status history, read for the whole page in one query.
// Every problem with the request at once, the path then the query (decision 68). A list checks
// its account: an unknown one is a 404, an empty list an account without deposits.
func (d depositRoutes) list(w http.ResponseWriter, r *http.Request) error {
	account, pathErrs := accountParam(r)
	query, queryErrs := parseDepositsQuery(r.URL.RawQuery)
	if invalid := slices.Concat(pathErrs, queryErrs); len(invalid) > 0 {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}
	filter := deposits.Filter{
		Status:               query.status,
		NetworkTransactionID: query.networkTransactionID,
	}

	page, found, err := deposits.ListPage(
		r.Context(), d.db, account, query.limit, query.before, filter)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, accountNotFound())
		return nil
	}

	var histories map[ids.DepositID][]deposits.StatusChange
	if query.history {
		depositIDs := make([]ids.DepositID, len(page.Deposits))
		for i, dep := range page.Deposits {
			depositIDs[i] = dep.ID
		}
		if histories, err = deposits.StatusHistories(r.Context(), d.db, depositIDs); err != nil {
			return err
		}
	}

	response := depositListResponse{
		Data:       make([]depositResponse, 0, len(page.Deposits)), // [] rather than null when empty
		NextBefore: page.NextBefore,
	}
	for _, dep := range page.Deposits {
		item := toDepositResponse(dep)
		if query.history {
			item = withHistory(item, histories[dep.ID])
		}
		response.Data = append(response.Data, item)
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", response)
	return nil
}
