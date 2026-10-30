package api

import (
	"math"
	"net/http"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// Read-only account endpoints. Node: node/src/api/accounts.ts. No authentication yet (see Known
// gaps): anyone who can reach the API can read any account.

type accountRoutes struct {
	db db.Querier
}

// The response bodies, field for field Node's (node/src/api/schemas.ts) in Node's order. Amounts
// are money.Amount (digit strings), IDs ids.* (TypeIDs), times timestamp.Time (Node's format),
// and a nullable field a pointer (null when nil).

type balanceResponse struct {
	PendingIn money.Amount `json:"pending_in"`
	Available money.Amount `json:"available"`
	Held      money.Amount `json:"held"`
	Currency  string       `json:"currency"`
}

type depositResponse struct {
	ID                   ids.DepositID   `json:"id"`
	AccountID            ids.AccountID   `json:"account_id"`
	CreditorIBAN         sepa.IBAN       `json:"creditor_iban"`
	ProviderAccountID    *string         `json:"provider_account_id"`
	AmountMinor          money.Amount    `json:"amount_minor"`
	Currency             string          `json:"currency"`
	Network              string          `json:"network"`
	NetworkTransactionID string          `json:"network_transaction_id"`
	Status               deposits.Status `json:"status"`
	NotificationCount    int             `json:"notification_count"`
	CreatedAt            timestamp.Time  `json:"created_at"`
	UpdatedAt            timestamp.Time  `json:"updated_at"`
	// Only with ?include=history. A pointer to a slice, so "absent" (nil, left out by omitempty)
	// differs from "no rows" (an empty slice, written as []), as Node's optional history does.
	History *[]statusChangeResponse `json:"history,omitempty"`
}

type statusChangeResponse struct {
	FromStatus *deposits.Status `json:"from_status"`
	ToStatus   deposits.Status  `json:"to_status"`
	Reason     *string          `json:"reason"`
	At         timestamp.Time   `json:"at"`
}

type transactionListResponse struct {
	Deposits   []depositResponse `json:"deposits"`
	NextBefore *ids.DepositID    `json:"next_before"`
}

func toDepositResponse(d deposits.Deposit) depositResponse {
	return depositResponse{
		ID:                   d.ID,
		AccountID:            d.Account,
		CreditorIBAN:         d.CreditorIBAN,
		ProviderAccountID:    d.ProviderAccountID,
		AmountMinor:          d.Amount,
		Currency:             d.Currency,
		Network:              d.Network,
		NetworkTransactionID: d.NetworkTransactionID,
		Status:               d.Status,
		NotificationCount:    d.NotificationCount,
		CreatedAt:            timestamp.Time(d.CreatedAt),
		UpdatedAt:            timestamp.Time(d.UpdatedAt),
	}
}

func toStatusChangeResponse(c deposits.StatusChange) statusChangeResponse {
	return statusChangeResponse{
		FromStatus: c.From,
		ToStatus:   c.To,
		Reason:     c.Reason,
		At:         timestamp.Time(c.At),
	}
}

// accountParam parses the {account_id} path value, as Node's accountParamsSchema.
func accountParam(r *http.Request) (ids.AccountID, []ValidationError) {
	account, err := ids.FromTypeID[ids.Account](r.PathValue("account_id"))
	if err != nil {
		return ids.AccountID{}, []ValidationError{{Path: "account_id", Message: "must be a acct_ ID"}}
	}
	return account, nil
}

// GET /accounts/{account_id}/balance
func (a accountRoutes) balance(w http.ResponseWriter, r *http.Request) error {
	account, invalid := accountParam(r)
	if invalid != nil {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}
	balance, found, err := ledger.GetBalance(r.Context(), a.db, account)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, accountNotFound())
		return nil
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", balanceResponse{
		PendingIn: balance.PendingIn,
		Available: balance.Available,
		Held:      balance.Held,
		Currency:  balance.Currency,
	})
	return nil
}

// transactionsQuery is ?limit, ?before and ?include, validated.
type transactionsQuery struct {
	limit   int
	before  *ids.DepositID
	history bool
}

// parseTransactionsQuery validates the query as Node's transactionsQuerySchema: include must be
// "history"; limit a whole number from 1 to 200 (default 50), read as JavaScript's Number() reads
// it (see jsNumber); before a deposit's TypeID. Unknown parameters are ignored. A parameter given
// twice is invalid (Node's parser makes it an array, which no rule accepts). Every problem is
// reported, in the schema's order; the messages are Zod's, though only the paths are part of the
// contract.
func parseTransactionsQuery(raw string) (transactionsQuery, []ValidationError) {
	query := parseQuery(raw)
	parsed := transactionsQuery{limit: 50}
	var errs []ValidationError

	if values, ok := query["include"]; ok {
		if len(values) == 1 && values[0] == "history" {
			parsed.history = true
		} else {
			errs = append(errs, ValidationError{
				Path: "include", Message: `Invalid input: expected "history"`,
			})
		}
	}

	if values, ok := query["limit"]; ok {
		n := math.NaN()
		if len(values) == 1 {
			n = jsNumber(values[0])
		}
		errs = append(errs, limitErrors(n)...)
		if len(errs) == 0 {
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
	return parsed, errs
}

// limitErrors are Zod's errors for a limit read as n, in Zod's order, measured against Node. A
// value that isn't a finite number, or isn't whole, gets that one error. A whole number outside
// JavaScript's safe range (int() refuses it) gets that error and is still checked against the
// bounds, so 1e300 has two errors, as in Node; within the range, only the bounds are checked.
func limitErrors(n float64) []ValidationError {
	one := func(message string) []ValidationError {
		return []ValidationError{{Path: "limit", Message: message}}
	}
	var messages []string
	switch {
	case math.IsNaN(n):
		return one("Invalid input: expected number, received NaN")
	case math.IsInf(n, 1):
		return one("Invalid input: expected number, received Infinity")
	case math.IsInf(n, -1):
		return one("Invalid input: expected number, received -Infinity")
	case n != math.Trunc(n):
		return one("Invalid input: expected int, received number")
	case n > maxSafeInteger:
		messages = append(messages, "Too big: expected int to be <=9007199254740991")
	case n < -maxSafeInteger:
		messages = append(messages, "Too small: expected int to be >=-9007199254740991")
	}
	if n < 1 {
		messages = append(messages, "Too small: expected number to be >=1")
	}
	if n > 200 {
		messages = append(messages, "Too big: expected number to be <=200")
	}
	errs := make([]ValidationError, len(messages))
	for i, m := range messages {
		errs[i] = ValidationError{Path: "limit", Message: m}
	}
	return errs
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, 2^53 - 1: Zod's int() accepts no larger.
const maxSafeInteger = 1<<53 - 1

// GET /accounts/{account_id}/transactions: deposits newest first, a page at a time (?limit,
// ?before=<next_before from the previous page>). ?include=history adds each deposit's status
// history, read for the whole page in one query.
func (a accountRoutes) transactions(w http.ResponseWriter, r *http.Request) error {
	account, invalid := accountParam(r)
	if invalid != nil {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}
	query, invalid := parseTransactionsQuery(r.URL.RawQuery)
	if invalid != nil {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}

	page, found, err := deposits.ListPage(r.Context(), a.db, account, query.limit, query.before)
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
		for i, d := range page.Deposits {
			depositIDs[i] = d.ID
		}
		if histories, err = deposits.StatusHistories(r.Context(), a.db, depositIDs); err != nil {
			return err
		}
	}

	response := transactionListResponse{
		Deposits:   make([]depositResponse, 0, len(page.Deposits)), // [] rather than null when empty
		NextBefore: page.NextBefore,
	}
	for _, d := range page.Deposits {
		item := toDepositResponse(d)
		if query.history {
			history := make([]statusChangeResponse, 0, len(histories[d.ID]))
			for _, c := range histories[d.ID] {
				history = append(history, toStatusChangeResponse(c))
			}
			item.History = &history
		}
		response.Deposits = append(response.Deposits, item)
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", response)
	return nil
}
