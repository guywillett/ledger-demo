# Shared test vectors

Inputs and the outcomes every implementation must give for them, for each rule that normalises,
parses or compares data (slice 3 decision 43). Schemas and contracts can't catch two
implementations that are both valid and still decide differently. In a mixed stack, a retry can
reach the other implementation, so two normalisations that are each reasonable but different
would turn a duplicate into a conflict, or accept a request one implementation would refuse.

The Node implementation (at `v0.2-node-reference`) is the reference: the vectors record what it
does, including where JavaScript's Unicode rules decide the outcome (see *Unicode* below). Both
implementations' tests read every file and must pass every case.

## Format

Each file is a JSON array of cases:

```json
{ "name": "a leading zero", "input": "01250", "ok": false, "why": "no leading zero: one spelling per amount" }
{ "name": "lower case", "input": "de75512108001245126199", "ok": true, "value": "DE75512108001245126199" }
```

- `name`: unique within the file; tests report it.
- `input`: the raw value exactly as it would arrive in a JSON body (so a JSON number where a
  string is expected is a case of its own). Non-ASCII characters are written as `\u` escapes, so
  invisible ones (no-break spaces, byte-order marks) can be seen.
- `ok: true` with `value`: accepted, and `value` is the normalised result (the stored form).
- `ok: false` with `why`: rejected. `why` explains the rule for readers and is never asserted:
  error messages may differ between implementations (decision 42); only the decision may not.

| File | Rule | `input` | `value` |
|---|---|---|---|
| `money.json` | `amount_minor` | the field's raw value | the amount as a digit string |
| `typeid.json` | TypeID fields, per prefix (`acct`, `dep`, `wd`, `evt`, `mov`) | `{"prefix", "text"}` | the UUID it holds |
| `iban.json` | IBAN normalisation and validation; one registry example per SEPA country, which pins the country table | the raw IBAN | the normalised IBAN |
| `sepa-text.json` | SEPA text (slice 2 decision 32), required or optional, with a maximum length | `{"max", "optional", "text"}`; `text` absent means the field is absent | the trimmed text, or `null` for none (optional only) |
| `withdrawal-same-details.json` | a repeated `POST …/withdrawals` with one `Idempotency-Key`: the same request, or a reuse (`409`) | `{"a", "b"}`: two raw request bodies | the differing fields; `[]` is a repeat |
| `deposit-same-details.json` | a repeated notification with one `network_transaction_id`: a duplicate, or a conflict | `{"a", "b"}`: two raw notification bodies | the differing fields; `[]` is a duplicate |

In the same-details files both bodies are valid, so every case is `ok: true`; each is parsed with
the real request schema first (so an IBAN spelled two ways compares equal), then compared.
`currency` is a compared field in both, but the only valid value is `EUR`, so no case can make it
differ.

## How each implementation reads them

- **Node:** `node/test/vectors.test.ts` runs every case through the real boundary code: the Zod
  schemas in `src/shared/schemas.ts`, and for the pairs the request schemas then
  `differingWithdrawalFields` / `differingDepositFields`.
- **Go:** each package's tests read the files it implements (`internal/money`, `internal/ids`,
  `internal/sepa`; the same-details files from steps 4 and 7) through `internal/vectortest`,
  decoding `input` with `encoding/json` into the same types a request body is decoded into.

## Unicode

The rules follow JavaScript, not Unicode in general, because Node is the reference:

- **Whitespace** (removed from IBANs, trimmed from SEPA text) is JavaScript's `\s` / `trim()`
  set: tab, LF, VT, FF, CR, space, U+00A0, U+1680, U+2000–U+200A, U+2028, U+2029, U+202F,
  U+205F, U+3000 and U+FEFF. Not U+0085 (next line) or U+200B (zero-width space), which other
  languages' definitions differ on.
- **Upper case** for IBANs is JavaScript's `toUpperCase()`, Unicode's full case mapping: ten
  non-ASCII characters become ASCII letters (`ß` → `SS`, `ı` → `I`, `ſ` → `S`, and the
  ligatures `ﬀ ﬁ ﬂ ﬃ ﬄ ﬅ ﬆ`), so an IBAN written with them is accepted as its ASCII form.
- **Lengths** count UTF-16 code units (JavaScript's `length`), before normalisation for IBANs
  (at most 64) and after trimming for SEPA text.
