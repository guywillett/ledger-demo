# 0002 — Deposits and withdrawals

Status: accepted (after slice 2, before the Go implementation)

## Context

Slices 1 and 2 named the two money flows from different vocabularies: incoming money was a
"payment" (`incoming_payments`, `Payment`, `payment.created`, the `PAYMENTS` stream, `pay_`
IDs) and outgoing money a "withdrawal". "Payment" is too general for one direction (a
withdrawal is a payment too), and the pair mixed bank language with custody language.

## Decision

- **Our vocabulary:** money in is a **deposit**, money out a **withdrawal**: tables, types,
  events (`deposit.created`, `withdrawal.held`), streams (`DEPOSITS`, `WITHDRAWALS`) and TypeID
  prefixes (`dep`, `wd`).

  The pair was chosen for consistency and an obvious direction, not to model a particular
  business: a bank would more often say incoming and outgoing payments, but either vocabulary
  works (a platform licensed for first-party deposits and withdrawals that later becomes a bank
  rarely renames its flows).
- **The provider's vocabulary stays at the edge:** the provider notifies us of an incoming
  payment (`POST /provider/incoming-payments`) and reports on transfers
  (`POST /provider/transfer-updates`, `transfer_updates`). Ingestion maps its words to ours, as
  it maps its identifiers (slice 1 decision 14).
- **The deposit-worker** keeps a generic role name with the flow in front of it. It runs the
  whole deposit lifecycle (book, review, post the decision) as one consumer for now; the review
  is the natural first thing to split into its own consumer, and the name stays true after
  that.
- **The rename edits the existing migrations in place.** Nothing is deployed: the only
  databases are development and test, reset with one command. The `v0.1-simple-ledger` tag
  keeps the old names as a consistent snapshot. The specs keep the names they were written with.

## In production

A deployed system would not do this. A rename there is expand–contract: add the new name
alongside the old (a view or a column alias, events published under both subjects), move
readers and writers across over several deployments, then remove the old name. Values already
in immutable records (the movement journal's reference types, the outbox's published events)
are not rewritten: they are what happened, and readers accept both values. A stream is drained
before its consumers move to the new one.

## Consequences

- One vocabulary in the schema, the events and the code the Go implementation is built against.
- Provider-facing names differ from internal ones on purpose; the mapping is in ingestion.
