# Docs

- [`slices/`](slices/): one spec per slice: what it builds, its numbered decisions with the
  reasoning, and its known gaps. A later decision can revise an earlier one, and says so.
- [`decisions/`](decisions/): cross-cutting decisions that aren't tied to one slice (currently
  [money representation](decisions/0001-money-representation.md), and
  [deposits and withdrawals](decisions/0002-deposits-and-withdrawals.md) as our names for the
  two money flows).
- [`development.md`](development.md): the reference for running and testing the system, and the
  tooling.
- [`guarantees.md`](guarantees.md): each guarantee, how it is kept, and the Node and Go tests
  that show it.
- [`next-steps.md`](next-steps.md): where the project could go next, grouped by what each step would
  show.
