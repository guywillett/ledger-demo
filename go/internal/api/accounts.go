package api

import (
	"net/http"

	"github.com/guywillett/ledger-demo/go/internal/accounts"
	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/ids"
	"github.com/guywillett/ledger-demo/go/internal/ledger"
	"github.com/guywillett/ledger-demo/go/internal/money"
	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/timestamp"
)

// Read-only account endpoints: the account and its balance (its deposits are in deposits.go).
// Node: node/src/api/accounts.ts. No authentication yet (see Known gaps): anyone who can reach the
// API can read any account.

type accountRoutes struct {
	db db.Querier
}

// The response bodies, field for field Node's (node/src/api/schemas.ts) in Node's order. Amounts
// are money.Amount (digit strings), IDs ids.* (TypeIDs), times timestamp.Time (Node's format),
// and a nullable field a pointer (null when nil).

type balanceResponse struct {
	AccountID ids.AccountID `json:"account_id"`
	PendingIn money.Amount  `json:"pending_in"`
	Available money.Amount  `json:"available"`
	Held      money.Amount  `json:"held"`
	Currency  string        `json:"currency"`
}

type accountResponse struct {
	ID        ids.AccountID  `json:"id"`
	Name      string         `json:"name"`
	IBAN      sepa.IBAN      `json:"iban"`
	CreatedAt timestamp.Time `json:"created_at"`
}

// GET /accounts/{account_id}/balance
func (a accountRoutes) balance(w http.ResponseWriter, r *http.Request) error {
	account, invalid := idParam[ids.Account](r, "account_id")
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
		AccountID: account,
		PendingIn: balance.PendingIn,
		Available: balance.Available,
		Held:      balance.Held,
		Currency:  balance.Currency,
	})
	return nil
}

func (a accountRoutes) account(w http.ResponseWriter, r *http.Request) error {
	accountID, invalid := idParam[ids.Account](r, "account_id")
	if invalid != nil {
		writeProblem(w, invalidRequest(invalid))
		return nil
	}
	account, found, err := accounts.Get(r.Context(), a.db, accountID)
	if err != nil {
		return err
	}
	if !found {
		writeProblem(w, accountNotFound())
		return nil
	}
	writeJSON(w, http.StatusOK, "application/json; charset=utf-8", accountResponse{
		ID:        account.ID,
		Name:      account.Name,
		IBAN:      account.IBAN,
		CreatedAt: timestamp.Time(account.CreatedAt),
	})
	return nil
}
