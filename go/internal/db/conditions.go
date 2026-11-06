package db

import (
	"fmt"
	"strings"
)

// Conditions is a statement's WHERE, assembled from fixed fragments with each value a parameter:
// the lists' optional conditions (a cursor, filters) without building SQL from values. A condition
// left out is no condition at all, rather than ($n IS NULL OR …), so each statement stays as
// simple to plan as a fixed one. Node builds its lists' statements the same way.
//
// column and op are always constants in the caller's code, never input: they become SQL text. Only
// values are input, and they only ever become parameters.
type Conditions struct {
	params []any
	parts  []string
}

// NewConditions starts with the parameters that come before any condition's, numbered from $1 in
// the order given (a list's limit + 1, typically).
func NewConditions(params ...any) *Conditions {
	return &Conditions{params: params}
}

// Where adds "column op $n", value the next parameter.
func (c *Conditions) Where(column, op string, value any) {
	c.params = append(c.params, value)
	c.parts = append(c.parts, fmt.Sprintf("%s %s $%d", column, op, len(c.params)))
}

// SQL is the conditions joined with AND, for after WHERE.
func (c *Conditions) SQL() string {
	return strings.Join(c.parts, " AND ")
}

// Params are every parameter, the fixed ones first, in placeholder order.
func (c *Conditions) Params() []any {
	return c.params
}
