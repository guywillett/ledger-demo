// Package sepa holds the SEPA scheme's field rules, shared by deposits (provider notifications)
// and withdrawals (client requests): IBANs and SEPA basic text. Node: node/src/shared/iban.ts and
// the text schemas in node/src/shared/schemas.ts.
//
// Every rule must decide exactly as Node does (pinned by contracts/vectors/iban.json and
// sepa-text.json), and Node's are JavaScript's: its definition of whitespace, its upper-casing,
// its string lengths. Go's own differ in the details, so they're not used here (decision 54):
//
//   - whitespace: Go's unicode.IsSpace (behind strings.TrimSpace and strings.Fields) includes
//     U+0085 and leaves out U+FEFF; JavaScript's \s and trim() do the opposite. Go's regexp \s is
//     ASCII only. jsWhitespace is JavaScript's set.
//   - upper case: Go's strings.ToUpper maps one character to one; JavaScript's toUpperCase uses
//     Unicode's full mapping, where ß becomes SS. jsUpperToASCII has the cases that matter.
//   - length: Go's len counts bytes; JavaScript's length counts UTF-16 code units. utf16Len.
package sepa

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"
)

// jsWhitespace reports whether r is whitespace to JavaScript (its \s and String.prototype.trim,
// which are the same set): ECMAScript's WhiteSpace and LineTerminator. Measured against Node, not
// taken from the spec alone; contracts/vectors/README.md lists the set.
func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// utf16Len is a string's length as JavaScript counts it, in UTF-16 code units: one for most
// characters, two for those outside the Basic Multilingual Plane (emoji, for instance).
func utf16Len(s string) int {
	n := 0
	// range over a string yields runes (Unicode code points), decoding the UTF-8 bytes.
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// IBAN is a normalised SEPA IBAN (no whitespace, upper case) that passed ParseIBAN. Decoding a JSON
// string into an IBAN parses it (UnmarshalText), so a request body's IBAN field is valid once
// decoded. Any string can still be converted with IBAN(s), as Node's brand can be cast; outside
// this package, only do that for a value read back from the database.
type IBAN string

// The reasons ParseIBAN gives: fixed texts, as Node's, that never repeat the input (they end up in
// 400 responses and logs).
var (
	ErrIBANTooLong     = errors.New("must be at most 64 characters")
	ErrIBANFormat      = errors.New("must be a country code, two check digits, then letters or digits")
	ErrIBANCountry     = errors.New("country is not in SEPA")
	ErrIBANLength      = errors.New("wrong length for its country")
	ErrIBANCheckDigits = errors.New("check digits are out of range")
	ErrIBANChecksum    = errors.New("checksum is invalid")
)

// maxIBANInput is the longest IBAN input accepted, before normalisation (Node's ibanSchema
// .max(64), in UTF-16 code units): enough for any IBAN with spaces, not unbounded work.
const maxIBANInput = 64

// ibanLengths is the IBAN length per IBAN country code, for the countries in the SEPA schemes: the
// same table as Node's SEPA_IBAN_LENGTHS (node/src/shared/iban.ts has the sources and the note
// that membership is not yet verified against the EPC's list). iban.json has a valid example for
// every country, so a country added to or missing from either table fails the vectors.
var ibanLengths = map[string]int{
	"AD": 24, "AL": 28, "AT": 20, "BE": 16, "BG": 22, "CH": 21, "CY": 28, "CZ": 24, "DE": 22,
	"DK": 18, "EE": 20, "ES": 24, "FI": 18, "FR": 27, "GB": 22, "GI": 23, "GR": 27, "HR": 21,
	"HU": 28, "IE": 22, "IS": 26, "IT": 27, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "MC": 27,
	"MD": 24, "ME": 22, "MK": 19, "MT": 31, "NL": 18, "NO": 15, "PL": 28, "PT": 25, "RO": 24,
	"RS": 22, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "VA": 22,
}

// ibanFormat is the shape after normalisation. Go's regexp package (RE2) runs in time linear in
// the input, so a pattern can't be made to backtrack for ever; [A-Z] and [0-9] are ASCII only.
var ibanFormat = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]+$`)

// jsUpperToASCII lists the non-ASCII characters that JavaScript's toUpperCase turns into ASCII
// letters (enumerated over every code point with Node). Every other non-ASCII character stays
// non-ASCII when upper-cased, and so fails the format check either way, so these ten are all that
// can change a decision: with them, ıt60… is accepted as IT60… in both implementations.
var jsUpperToASCII = map[rune]string{
	'ß': "SS", 'ı': "I", 'ſ': "S",
	'ﬀ': "FF", 'ﬁ': "FI", 'ﬂ': "FL", 'ﬃ': "FFI", 'ﬄ': "FFL", 'ﬅ': "ST", 'ﬆ': "ST",
}

// normaliseIBAN removes JavaScript whitespace and upper-cases as JavaScript does (Node's
// input.replace(/\s+/g, "").toUpperCase()).
func normaliseIBAN(input string) string {
	var b strings.Builder
	for _, r := range input {
		switch {
		case jsWhitespace(r):
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case jsUpperToASCII[r] != "":
			b.WriteString(jsUpperToASCII[r])
		default:
			// ASCII as it is; any other non-ASCII character fails the format check.
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParseIBAN normalises (whitespace removed, upper case), then checks the format, the country's
// length, the check digit range and the ISO 13616 mod-97 checksum, as Node's parseIban does.
func ParseIBAN(input string) (IBAN, error) {
	if utf16Len(input) > maxIBANInput {
		return "", ErrIBANTooLong
	}
	value := normaliseIBAN(input)
	if !ibanFormat.MatchString(value) {
		return "", ErrIBANFormat
	}
	length, ok := ibanLengths[value[:2]]
	if !ok {
		return "", ErrIBANCountry
	}
	if len(value) != length {
		return "", ErrIBANLength
	}
	// Issued check digits are always 02-98. 00, 01 and 99 can still pass mod-97 (they are
	// congruent to 97, 98 and 02), so they're rejected explicitly.
	if check := value[2:4]; check == "00" || check == "01" || check == "99" {
		return "", ErrIBANCheckDigits
	}
	if mod97(value) != 1 {
		return "", ErrIBANChecksum
	}
	return IBAN(value), nil
}

// mod97 moves the first four characters to the end, reads letters as numbers (A = 10 … Z = 35)
// and returns the whole number's remainder mod 97; a valid IBAN gives 1. Node builds the number
// as a BigInt; taking the remainder digit by digit gives the same result in an int.
func mod97(iban string) int {
	rearranged := iban[4:] + iban[:4]
	remainder := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		if c >= 'A' && c <= 'Z' {
			remainder = (remainder*100 + int(c-'A') + 10) % 97 // two digits
		} else {
			remainder = (remainder*10 + int(c-'0')) % 97
		}
	}
	return remainder
}

// UnmarshalText parses a JSON string into an IBAN (see ParseIBAN).
func (i *IBAN) UnmarshalText(text []byte) error {
	parsed, err := ParseIBAN(string(text))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}

// The reasons the text functions give.
var (
	ErrBlank      = errors.New("must not be blank")
	ErrTooLong    = errors.New("is too long")
	ErrCharacters = errors.New("may only contain letters, digits, spaces and / - ? : ( ) . , ' +")
)

// sepaBasic reports whether s uses only the EPC's SEPA basic Latin character set: a-z A-Z 0-9
// / - ? : ( ) . , ' + and space (Node's SEPA_BASIC_TEXT; the database CHECKs on withdrawals are
// the backstop). Byte by byte: every allowed character is ASCII, and the bytes of a multi-byte
// UTF-8 character are all above 127, so none of them can match.
func sepaBasic(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("/-?:().,'+ ", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// ParseText checks required SEPA text of at most maxLen characters (slice 2 decision 32), as Node's
// sepaTextSchema: whitespace at both ends is trimmed first (JavaScript's trim(), see the package
// comment), and the trimmed text is the result; whitespace inside is kept. Text outside the
// character set is rejected, never transliterated.
func ParseText(input string, maxLen int) (string, error) {
	text := strings.TrimFunc(input, jsWhitespace)
	if text == "" {
		return "", ErrBlank
	}
	if utf16Len(text) > maxLen {
		return "", fmt.Errorf("%w: at most %d characters", ErrTooLong, maxLen)
	}
	if !sepaBasic(text) {
		return "", ErrCharacters
	}
	return text, nil
}

// ParseOptionalText is ParseText for an optional field, as Node's optionalSepaTextSchema: absent
// (nil; a JSON null decodes to nil too), empty and only-whitespace all mean none, returned as nil,
// the one spelling of none (stored as NULL).
func ParseOptionalText(input *string, maxLen int) (*string, error) {
	if input == nil || strings.TrimFunc(*input, jsWhitespace) == "" {
		return nil, nil
	}
	text, err := ParseText(*input, maxLen)
	if err != nil {
		return nil, err
	}
	return &text, nil
}
