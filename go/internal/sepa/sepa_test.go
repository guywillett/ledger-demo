package sepa_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/sepa"
	"github.com/guywillett/ledger-demo/go/internal/vectortest"
)

// The shared vectors (contracts/vectors/iban.json): each input is decoded the way a request's
// IBAN field is, by encoding/json into a sepa.IBAN (UnmarshalText).
func TestIBANVectors(t *testing.T) {
	vectortest.Run(t, "iban.json", func(input json.RawMessage) (any, error) {
		var iban sepa.IBAN
		err := json.Unmarshal(input, &iban)
		return iban, err
	})
}

// The shared vectors (contracts/vectors/sepa-text.json). text is decoded into a *string, as a
// request field is: absent and null both leave it nil.
func TestTextVectors(t *testing.T) {
	vectortest.Run(t, "sepa-text.json", func(input json.RawMessage) (any, error) {
		var in struct {
			Max      int     `json:"max"`
			Optional bool    `json:"optional"`
			Text     *string `json:"text"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err // e.g. a JSON number as text
		}
		if in.Optional {
			return sepa.ParseOptionalText(in.Text, in.Max)
		}
		if in.Text == nil {
			return nil, errors.New("required")
		}
		return sepa.ParseText(*in.Text, in.Max)
	})
}

// What the vectors don't assert: the reason for each rejection, a fixed text without the input.
func TestIBANReasons(t *testing.T) {
	tests := []struct {
		input string
		want  error
	}{
		{"FR76-1234-5000", sepa.ErrIBANFormat},
		{"US64SVBKUS6S3300958879", sepa.ErrIBANCountry},
		{"DE7551210800124512619", sepa.ErrIBANLength},
		{"DE99123456780000000077", sepa.ErrIBANCheckDigits},
		{"DE76512108001245126199", sepa.ErrIBANChecksum},
		{strings.Repeat(" ", 65), sepa.ErrIBANTooLong},
	}
	for _, tt := range tests {
		if _, err := sepa.ParseIBAN(tt.input); !errors.Is(err, tt.want) {
			t.Errorf("ParseIBAN(%q) error = %v, want %v", tt.input, err, tt.want)
		}
	}
}

func TestTextReasons(t *testing.T) {
	if _, err := sepa.ParseText("  ", 70); !errors.Is(err, sepa.ErrBlank) {
		t.Errorf("blank: error = %v, want ErrBlank", err)
	}
	if _, err := sepa.ParseText(strings.Repeat("a", 71), 70); !errors.Is(err, sepa.ErrTooLong) {
		t.Errorf("too long: error = %v, want ErrTooLong", err)
	}
	if _, err := sepa.ParseText("Zoë", 70); !errors.Is(err, sepa.ErrCharacters) {
		t.Errorf("accent: error = %v, want ErrCharacters", err)
	}
}

// As Node's maskIban: country, check digits and the last four characters.
func TestMask(t *testing.T) {
	iban, err := sepa.ParseIBAN("FR7612345000010000000000193")
	if err != nil {
		t.Fatal(err)
	}
	if got := iban.Mask(); got != "FR76\u20260193" {
		t.Errorf("Mask() = %q", got)
	}
}
