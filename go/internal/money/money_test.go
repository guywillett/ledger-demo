package money_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/money"
)

func TestParseAmount(t *testing.T) {
	tests := []struct {
		input string
		want  money.Amount
	}{
		{"1", 1},
		{"1250", 1250},
		// Above 2^53: exact, which a float64 (or a JavaScript number) is not.
		{"9007199254740993", 9007199254740993},
		{"9223372036854775807", 9223372036854775807}, // the largest bigint
	}
	for _, tt := range tests {
		got, err := money.ParseAmount(tt.input)
		if err != nil || got != tt.want {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", tt.input, got, err, tt.want)
		}
	}
}

func TestParseAmountRejects(t *testing.T) {
	tests := []struct {
		input string
		want  error
	}{
		{"", money.ErrNotPositiveWholeNumber},
		{"0", money.ErrNotPositiveWholeNumber},
		{"01250", money.ErrNotPositiveWholeNumber},
		{"-1250", money.ErrNotPositiveWholeNumber},
		{"+1250", money.ErrNotPositiveWholeNumber},
		{"12.50", money.ErrNotPositiveWholeNumber},
		{"1e3", money.ErrNotPositiveWholeNumber},
		{" 1250", money.ErrNotPositiveWholeNumber},
		{"1250 ", money.ErrNotPositiveWholeNumber},
		{"١٢٥٠", money.ErrNotPositiveWholeNumber},                 // Arabic-Indic digits: not ASCII 0-9
		{"10000000000000000000", money.ErrNotPositiveWholeNumber}, // 20 characters
		{"9223372036854775808", money.ErrTooLarge},                // the largest bigint + 1
		{"9999999999999999999", money.ErrTooLarge},
	}
	for _, tt := range tests {
		// errors.Is matches a sentinel error value, the Go counterpart of checking an error's
		// class in Node (instanceof); it also sees through wrapping (fmt.Errorf with %w).
		if _, err := money.ParseAmount(tt.input); !errors.Is(err, tt.want) {
			t.Errorf("ParseAmount(%q) error = %v, want %v", tt.input, err, tt.want)
		}
	}
}

func TestJSON(t *testing.T) {
	type body struct {
		Amount money.Amount `json:"amount_minor"`
	}
	data, err := json.Marshal(body{Amount: 9007199254740993})
	if err != nil || string(data) != `{"amount_minor":"9007199254740993"}` {
		t.Errorf("Marshal = %s, %v", data, err)
	}
	// Zero is written (a balance can be zero), though it isn't accepted as input.
	if data, _ := json.Marshal(money.Amount(0)); string(data) != `"0"` {
		t.Errorf("Marshal(0) = %s, want \"0\"", data)
	}

	var got body
	if err := json.Unmarshal([]byte(`{"amount_minor":"1250"}`), &got); err != nil || got.Amount != 1250 {
		t.Errorf("Unmarshal = %d, %v; want 1250", got.Amount, err)
	}
	for _, input := range []string{`{"amount_minor":1250}`, `{"amount_minor":"0"}`, `{"amount_minor":null}`} {
		if err := json.Unmarshal([]byte(input), &got); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want an error", input)
		}
	}
}
