package db_test

import (
	"slices"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/db"
)

// Each condition takes the next placeholder after the fixed parameters, and its value the matching
// parameter: the SQL never holds a value.
func TestConditionsNumbersEachValueAsTheNextParameter(t *testing.T) {
	c := db.NewConditions(51)
	c.Where("account_id", "=", "acct")
	c.Where("id", "<", "cursor")
	c.Where("status", "=", "held")
	if want := "account_id = $2 AND id < $3 AND status = $4"; c.SQL() != want {
		t.Errorf("SQL = %q, want %q", c.SQL(), want)
	}
	if want := []any{51, "acct", "cursor", "held"}; !slices.Equal(c.Params(), want) {
		t.Errorf("Params = %v, want %v", c.Params(), want)
	}
}

func TestConditionsWithoutAnyIsEmpty(t *testing.T) {
	c := db.NewConditions(1)
	if c.SQL() != "" || !slices.Equal(c.Params(), []any{1}) {
		t.Errorf("SQL %q, Params %v; want empty, [1]", c.SQL(), c.Params())
	}
}
