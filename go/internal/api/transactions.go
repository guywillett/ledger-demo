package api

import (
	"net/http"
	"slices"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/transactions"
)

// GET /accounts/{account_id}/transactions: the account's deposits and withdrawals together, newest
// first (slice 4 decision 67; how the page is read: package transactions). Node:
// node/src/api/transactions.ts.

type transactionRoutes struct {
	db db.Querier
}

type transactionsQuery struct {
	limit  int
	before *transactions.Cursor
}

// transactionItem is what the list's data holds: a depositResponse or a withdrawalResponse and
// nothing else (Go has no union types; the unexported method seals the set to this package, as
// ids.Kind does). encoding/json encodes each item by its own type.
type transactionItem interface {
	isTransactionItem()
}

func (depositResponse) isTransactionItem()    {}
func (withdrawalResponse) isTransactionItem() {}

// transactionsResponse is the list's body: each item exactly its own endpoint's representation
// (depositResponse or withdrawalResponse, telling themselves apart by type), never with history.
type transactionsResponse struct {
	Data       []transactionItem    `json:"data"`
	NextBefore *transactions.Cursor `json:"next_before"`
}

// parseTransactionsQuery validates the transactions list's query, as Node's
// transactionsQuerySchema: limit, and before, a deposit's or a withdrawal's ID (parseCursor). No
// include and no filters (decision 67): unknown parameters here, ignored as every unknown
// parameter is. Every problem is reported at once, in the contract's parameter order (slice 4
// decision 68).
func parseTransactionsQuery(raw string) (transactionsQuery, []ValidationError) {
	query := parseQuery(raw)
	parsed := transactionsQuery{}
	var errs, invalid []ValidationError

	parsed.limit, invalid = parseLimit(query)
	errs = append(errs, invalid...)

	parsed.before, invalid = parseCursor(query)
	errs = append(errs, invalid...)

	return parsed, errs
}

// GET /accounts/{account_id}/transactions: newest first, a page at a time (?limit,
// ?before=<next_before from the previous page>), as {data, next_before} (decision 66). Every
// problem with the request at once, the path then the query (decision 68). A list checks its
// account: an unknown one is a 404, an empty list an account without transactions.
func (h transactionRoutes) list(w http.ResponseWriter, r *http.Request) error {
	account, pathErrs := idParam[ids.Account](r, "account_id")
	query, queryErrs := parseTransactionsQuery(r.URL.RawQuery)
	if invalid := slices.Concat(pathErrs, queryErrs); len(invalid) > 0 {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}

	page, found, err := transactions.ListPage(
		r.Context(), h.db, account, query.limit, query.before)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, accountNotFound())
		return nil
	}

	response := transactionsResponse{
		Data:       make([]transactionItem, 0, len(page.Items)), // [] when no data
		NextBefore: page.NextBefore,
	}
	for _, item := range page.Items {
		if item.Deposit != nil {
			response.Data = append(response.Data, toDepositResponse(*item.Deposit))
		} else {
			response.Data = append(response.Data, toWithdrawalResponse(*item.Withdrawal))
		}
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", response)
	return nil
}
