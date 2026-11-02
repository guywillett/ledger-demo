# Slice 3: Node's test cases mapped to Go's

**A snapshot at `v0.3-go`, not kept up to date.** Made in slice 3's review as the evidence for the *Done when* item "Go's integration tests cover the cases of the Node suites for the ledger, the API, the relay, the deposit-worker and the submitter (cases, not code)" ([slice-3-go.md](slice-3-go.md)). Later changes to either suite don't update it.

**Scope.** Node files under `node/test/` for the ledger, the API (client endpoints and webhooks, including `ingestion/`), the outbox and relay, the consumer layer, the deposit-worker and the submitter. Not mapped, as the brief says: provider-sim, reconciliation, the e2e suites; also out of scope here, as foundations: `shared/` (config, db, events, iban, ids, logger, backoff) and `vectors.test.ts`, whose Go counterparts live in `config`, `db`, `events`, `ids`, `money`, `sepa`, `logging`, `backoff` and the vector tests.

**Marks.** `=` the same case; `~` covered differently (how is said); `-` not covered in Go; `n/a` Node-specific. One row per Node `it` (an `it.each` is one row, its rows noted). Paths: Go files are under `go/internal/` unless they start with `cmd/`; Node files under `node/test/`.

**How sure.** Every table-driven case was compared row by row, and every pair whose names don't say it all was read in full (marked in the notes). For the rest, a `=` rests on matching names and doc comments (most Go tests are named after their Node case, word for word) and on the step reviews that ported them; not every assertion of every `=` pair was re-read.

## Summary

| Mark | Count |
|---|---|
| `=` | 156 |
| `~` | 15 |
| `-` | 1 (accepted) |
| `n/a` | 0 |
| **Node cases** | **172** |

No `n/a`: where Node's case is a Fastify or Zod behaviour, Go matches it (measured) rather than skipping it, and Node's compile-time type tests have run-time or by-construction counterparts (marked `~`).

**Every `-`:**

| Node case | Go | Accepted because |
|---|---|---|
| `ingestion/incoming-payments.test.ts`: "reports an inconsistency, not a conflict, when no compared field differs" | The two lines of `ingestion.RecordNotification` that turn a refused insert with no differing field into `Inconsistent` have no test. Both sides of them do: `DifferingFields` is pinned by the shared vectors (`api/webhook_internal_test.go` `TestDepositSameDetailsVectors`), and the API's 500 for `Inconsistent` by `TestInconsistentIs500` (with a stubbed record function). | Accepted in the review. The branch is defensive: unreachable against the real database (Node needs a stub pool for it too), and a bug there would show as a 500 instead of a 204 for an already-impossible state. A Go test would need a fake `db.Tx`, which Go doesn't have. The branch says all this in a comment. |

**Closed in the review** (each was `~` in the first draft): the books after the transfer-update tests (`TestLeavesTheBooksConsistentForEveryAccountWhoseUpdatesApplied`) and "404 before the body is parsed" (a case in `TestAnswers404AsNodeDoes`); both are now `=`. The skipped `deposits_unmatched_check` test is still `~`, now with its reason written in `api/webhook_test.go`.

## `api/accounts.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| balance: starts at zero | `api/accounts_test.go` `TestBalanceStartsAtZero` | = | |
| balance: pending_in once booked, then available once credited | `TestBalanceShowsPendingThenAvailable` | = | |
| balance: keeps amounts beyond 2^53 exact | `TestBalanceKeepsAmountsBeyond2To53Exact` | = | |
| balance: 404 for an unknown account | `TestBalanceUnknownAccountIs404` | = | |
| balance: 400 for account_id %s (`acct_garbage`, a `dep_` ID, `alice`) | `TestBalanceInvalidAccountIDIs400` | = | Read: the same three; Go compares the exact body, which covers Node's "doesn't repeat the input". |
| transactions: only this account's deposits, newest first, current status | `TestTransactionsListsThisAccountsDepositsNewestFirst` | = | |
| transactions: empty page; 404 for an unknown account | `TestTransactionsEmptyPageAndUnknownAccount` | = | |
| transactions: pages with limit and before | `TestTransactionsPagesWithLimitAndBefore` | = | |
| transactions: include=history | `TestTransactionsIncludesHistory` | = | |
| transactions: 400 for %s (5 rows) | `TestTransactionsInvalidQueryIs400` | = | Read: Node's five rows, plus nine measured against Node's API (`limit=`, `2.5`, `-0`, `Infinity`, repeated parameters, `;`, a bad escape, `before=`). |
| transactions: ignores unknown query parameters | `TestTransactionsIgnoresUnknownQueryParameters` | = | |
| an unexpected error on a GET: 500 with a problem | `api/http_test.go` `TestAnswersAnUnexpectedErrorWith500` | = | Both contract-checked since `9bb547d`. |

## `api/body-errors.test.ts`

One `it.each` (malformed JSON, a non-JSON content type, an oversized body) per route; one row per route here.

| Node case | Go | Mark | Note |
|---|---|---|---|
| POST /provider/incoming-payments: 400, 400, 400 | `api/webhook_test.go` `TestAnswersBodyProblemsAsNodeDoes` | = | Read: rows "malformed", "text/plain", "over 1 MiB", among Node-measured others. |
| POST /provider/transfer-updates: 400, 400, 400 | `api/webhook_transfer_updates_test.go` `TestAnswersBodyFramingErrorsWith400` | = | Its comment names this Node row. |
| POST /accounts/:id/withdrawals: 400, 415, 413 | `api/withdrawals_test.go` `TestAnswersBodyFramingErrorsWithTheirExactCodes` | = | Its comment names this Node row. |

## `api/deposit-events.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| deposit.created: stored once, in the transaction that created it | `api/webhook_test.go` `TestStoresDepositCreatedInTheDepositsTransaction` | = | |
| not stored again for duplicates | `TestStoresNoEventForDuplicatesConflictsUnmatchedOrInvalid` | = | |
| not stored for conflicts, unmatched deposits or invalid bodies | `TestStoresNoEventForDuplicatesConflictsUnmatchedOrInvalid` | = | |
| through the relay: reaches the DEPOSITS stream once | `relay/relay_test.go` `TestPublishesEachEventWithItsMsgIDAndMarksItPublished`, `TestPublishesAgainAfterARelayCrashedBetweenPublishingAndMarkingAndTheStreamDropsTheRepeat` | ~ | Read: Go's relay tests create deposits through `ingestion.RecordNotification` (the webhook's own function) rather than the HTTP handler, publish to the test stream and check subject, `Nats-Msg-Id`, payload and a single message. The same chain minus the HTTP layer, which the outbox tests above cover. |
| DEPOSITS stream matches `nats/streams/DEPOSITS.json` | `relay/relay_test.go` `TestStreamsMatchTheirDefinitions` | = | Both streams. |

## `api/incoming-payments.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| records a deposit as created, mapped by IBAN, with its event | `api/webhook_test.go` `TestRecordsADepositAsCreatedWithItsEvent` | = | |
| accepts the contract's request example | `TestAcceptsTheContractsRequestExample` | = | |
| normalises the IBAN before matching | `TestNormalisesTheIBANBeforeMatching` | = | |
| works without a provider account ID | `TestWorksWithoutAProviderAccountID` | = | |
| the SCT maximum exactly, one minor unit more refused | `TestAcceptsTheSEPACreditTransferMaximumExactlyAndRefusesOneMinorUnitMore` | = | |
| ignores unknown fields | `TestIgnoresUnknownFields` | = | |
| the same notification 5 times: once, counted | `TestRecordsTheSameNotificationOnceCountingRepeats` | = | |
| 10 concurrent identical notifications: once | `TestRecordsConcurrentIdenticalNotificationsOnce` | = | |
| a retry with or without provider_account_id is a duplicate, keeping the first | `TestADuplicateKeepsTheFirstProviderAccountID` | = | Read: with, without, and another value. |
| formatting differences are the same details | `TestTreatsFormattingDifferencesAsTheSameDetails` | = | |
| records %s as a conflict (amount, IBAN of ours, foreign IBAN) | `TestRecordsAConflictChangingNothing` | = | Read: the same three rows, differing fields, the alert line, no value logged. |
| exactly one of concurrent conflicting notifications creates | `TestExactlyOneOfConcurrentConflictingNotificationsCreates` | = | |
| a valid IBAN not ours: unmatched, no account, event or movement | `TestRecordsAnUnknownIBANInSuspense` | = | |
| refuses %s (the database CHECK `deposits_unmatched_check`, 2 rows) | none | ~ | Tests the shared schema, not Node's code; it runs in Node's suite against the same migrations. Not ported on purpose, documented at the top of `api/webhook_test.go`. |
| stays unmatched after an account with that IBAN is created | `TestStaysUnmatchedAfterAnAccountWithThatIBANIsCreated` | = | |
| 400 for %s, recording nothing (11 rows) | `TestAnswers400ForAnInvalidNotificationRecordingNothing` | = | Read: the same eleven rows. |
| 400 for malformed JSON | `TestAnswersBodyProblemsAsNodeDoes` | = | Row "malformed". |
| 400 (not 415) for text/plain, application/xml | `TestAnswersBodyProblemsAsNodeDoes` | = | Rows "text/plain", "application/xml". |
| 404 for an unknown route without echoing the URL | `api/http_test.go` `TestAnswers404AsNodeDoes` | = | Row `/nope?iban=…`; checks the body echoes neither. |
| reuses a well-formed x-request-id and echoes it | `api/http_test.go` `TestReusesAWellFormedRequestIDAndEchoesIt` | ~ | The same middleware, tested on `GET /nope` rather than on the webhook. |
| replaces an x-request-id too long or with other characters | `TestReplacesABadRequestID` | ~ | As above. |
| logs each outcome at its level, masks IBANs, never logs the body | `api/webhook_test.go` `TestLogsEachOutcomeAndNeverTheBody` | = | |
| logs the path without the query string | `api/http_test.go` `TestLogsOneAccessLinePerRequest` | ~ | Read: on `GET …/transactions?iban=…&limit=1`, not on the webhook; checks the path has no query and no `FR76` is logged. |
| internal inconsistency: 500 and an error line | `api/webhook_internal_test.go` `TestInconsistentIs500` | = | Go stubs the record function, Node the pool; both check the 500 and the alert line. (Ingestion's half: see `ingestion/`.) |

## `api/not-found.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| %s answers 404, as an unknown route (`/withdrawals/`, `/accounts//balance`, `/accounts//transactions`) | `api/http_test.go` `TestAnswers404AsNodeDoes` | ~ | Two of the three paths are rows (added in `aa68bce`); `/accounts//transactions` isn't, but goes through the same check (`onlyCleanPaths`). |
| answers 404 before the body is parsed | `api/http_test.go` `TestAnswers404AsNodeDoes` | = | Its last case: `POST /accounts//withdrawals` with a malformed JSON body. |
| leaves a non-empty invalid parameter to the route (400) | `api/withdrawals_test.go` `TestGetAnswers404ForAnUnknownWithdrawalAnd400ForAnInvalidID` | = | |

## `api/transfer-updates.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| executes a submitted withdrawal | `api/webhook_transfer_updates_test.go` `TestExecutesASubmittedWithdrawal` | = | |
| releases a failed one | `TestReleasesAFailedOneFundsBackInAvailableTheReasonStored` | = | |
| applies an update that overtakes the submitter | `TestAppliesAnUpdateThatOvertakesTheSubmitterTheLateMarkSubmittedSkips` | = | |
| counts the same event resent | `TestCountsTheSameEventResentSettlingNothingTwice` | = | |
| a second event with the same result: duplicate | `TestRecordsASecondEventWithTheSameResultAsADuplicate` | = | |
| a reused event ID with other details: nothing, alert | `TestChangesNothingWhenTheProviderReusesAnEventIDWithOtherDetailsAndAlerts` | = | |
| failed after executed: conflict, alert | `TestRecordsFailedAfterExecutedAsAConflictMovingNothingAndAlerts` | = | Node also checks reconciliation lists it; Go checks the conflict row reconciliation reads (Node only, decision 50). |
| a transfer-ID mismatch: conflict | `TestRecordsATransferIDMismatchAsAConflictMovingNothing` | = | |
| an update for %s: unmatched, alert (2 rows) | `TestRecordsAnUpdateForAnUnknownWithdrawalOrAForeignReferenceAsUnmatchedAndAlerts` | = | Read: Node's two rows plus a `dep_` TypeID; Node's reconciliation check as above. |
| 400 for %s, recording nothing (5 rows) | `TestAnswers400RecordingNothing` | = | Read: Node's five rows plus an offset `occurred_at`. |
| 400 (not 415) for a non-JSON content type | `TestAnswersBodyFramingErrorsWith400` | = | |
| 500 for an invariant violation, rolled back, alert | `TestAnswers500ForAnInvariantViolationRollingEverythingBackAndAlerts` | = | |
| leaves the books consistent for every account whose updates applied | `TestLeavesTheBooksConsistentForEveryAccountWhoseUpdatesApplied` | = | Reconciliation is Node only (decision 50): Go checks each account as it would (`expectConsistent`), after executed, failed, overtaking, resent and contradicted updates. |

## `api/withdrawals.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| creates and holds: 201 with Location | `api/withdrawals_test.go` `TestCreatesTheWithdrawalAndHoldsItsFunds201WithALocationHeader` | = | |
| trims the text fields; empty or blank remittance is none | `TestTrimsTheTextFieldsAndTreatsAnEmptyOrBlankRemittanceAsNone` | = | |
| a repeated request: one 201, then 200s, one hold | `TestAnswersARepeatedRequestWithTheSameWithdrawalOne201Then200sOneHold` | = | |
| a repeat differing only in trimmed whitespace or blank remittance is the same | `TestTreatsARepeatThatDiffersOnlyInTrimmedWhitespaceOrBlankRemittanceAsTheSame` | = | |
| one withdrawal for 10 concurrent requests with one key | `TestCreatesOneWithdrawalFor10ConcurrentRequestsWithOneKey` | = | |
| the same key with a different %s: 409 | `TestRefusesTheSameKeyWithADifferentDetail409NothingChanged` | = | The rule itself also by `withdrawal-same-details.json` (`TestWithdrawalSameDetailsVectors`). |
| insufficient funds: 422, nothing stored, key stays free | `TestRefusesInsufficientFundsWith422StoringNothingAndTheKeyStaysFree` | = | |
| exactly one of two concurrent withdrawals exceeding available | `TestLetsExactlyOneOfTwoConcurrentWithdrawalsThatTogetherExceedAvailableThrough` | = | |
| 404 for an unknown account; the key then works | `TestAnswers404ForAnUnknownAccountStoringNothingTheKeyThenWorksOnARealOne` | = | |
| 400 for %s, storing nothing (15 rows) | `TestAnswers400StoringNothing` | = | Read: the same fifteen rows. |
| reports every problem at once: account, key and body | `TestReportsEveryProblemAtOnceAccountKeyAndBody` | = | |
| withdrawal.held in the outbox, same transaction; nothing for a repeat | `TestStoresWithdrawalHeldInTheOutboxInTheSameTransactionAndNothingForARepeat` | = | |
| logs without the name, the remittance or the full IBAN | `TestLogsOutcomesWithoutTheNameTheRemittanceTextOrTheFullIBAN` | = | |
| GET: the withdrawal, and its history with include=history | `TestGetReturnsTheWithdrawalAndItsHistoryWithIncludeHistory` | = | |
| GET: 404 unknown, 400 invalid ID | `TestGetAnswers404ForAnUnknownWithdrawalAnd400ForAnInvalidID` | = | |
| reconciliation finds the account consistent | `TestTheAccountStaysConsistentAfterWithdrawalsThroughTheAPI` | ~ | Reconciliation is Node only (decision 50), so Go checks what it would in SQL (`expectConsistent`): every bucket against its movements, held against the held and submitted withdrawals, and each withdrawal's movements against its status. |

## `ingestion/incoming-payments.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| after a refused notification: a conflict naming the differing fields | `api/webhook_test.go` `TestRecordsAConflictChangingNothing`; `api/webhook_internal_test.go` `TestDepositSameDetailsVectors` | ~ | Node calls `recordNotification` with a stub pool; Go reaches the same branch end to end against the real database (a stored deposit, then a differing notification) and checks the differing fields; the comparison is pinned by the vectors. |
| reports an inconsistency, not a conflict, when no compared field differs | none | - | See the summary. |

## `consumer/delivery-log.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| types: a path that never settles, and a forged Settled, don't compile | `consumer/lifecycle_test.go` `TestTerminatesABugWithAFatalAlert` (subtest "the zero Settled") | ~ | A path that returns nothing doesn't compile in Go either; but Go can write the zero `Settled{}`, so the lifecycle catches it at run time as a bug (documented on `Settled`). |
| settles a message once: a second settle throws, nothing signalled twice | `TestSettlesOnceASecondSettleIsRefusedAndNothingIsSignalledTwice` | = | Its comment names this Node case. |

## `deposit-worker/handler.test.ts`

All in `depositworker/handler_test.go`, named after Node's cases.

| Node case | Go | Mark |
|---|---|---|
| books, reviews, decides and credits an approved deposit, then acks | `TestBooksReviewsDecidesAndCreditsAnApprovedDepositThenAcks` | = |
| returns a rejected deposit to the sender | `TestReturnsARejectedDepositToTheSender` | = |
| the same event once when delivered twice with different message IDs | `TestAppliesTheSameEventOnceWhenDeliveredTwiceWithDifferentMessageIDs` | = |
| the same event once when two consumers process it concurrently | `TestAppliesTheSameEventOnceWhenTwoConsumersProcessItConcurrently` | = |
| completes after a crash between booking and the decision | `TestCompletesAfterACrashBetweenBookingAndTheDecisionCreditingPendingInOnce` | = |
| posts the recorded decision after a crash between deciding and posting | `TestPostsTheRecordedDecisionAfterACrashBetweenDecidingAndPostingNeverReDeciding` | = |
| terminates an event that is not valid JSON, no event_id logged | `TestTerminatesAnEventThatIsNotValidJSONLoggingTheDeliveryWithoutAnEventID` | = |
| terminates an event that fails validation, event_id kept | `TestTerminatesAnEventThatFailsValidationKeepingItsEventIDInTheLog` | = |
| terminates an event for a deposit that does not exist | `TestTerminatesAnEventForADepositThatDoesNotExist` | = |
| naks after an unexpected failure; the redelivery completes | `TestNaksAfterAnUnexpectedFailureAndTheRedeliveryCompletesFromWhereItStopped` | = |
| naks without throwing when the delivery can't be recorded | `TestNaksWithoutFailingWhenTheDeliveryCannotEvenBeRecorded` | = |
| signals working() during the review | `TestSignalsWorkingDuringTheReviewSoTheMessageIsNotRedeliveredMeanwhile` | = |
| skips the review delay for a duplicate and an unknown deposit | `TestSkipsTheReviewDelayForADuplicateAndForAnUnknownDeposit` | = |
| terminates on an invariant violation, logged as fatal | `TestTerminatesInsteadOfRetryingWhenAnInvariantIsViolatedLoggingItAsFatal` | = |
| terminates on an OverdraftError | `TestTerminatesOnAnOverdraftErrorRatherThanRetryingIt` | = |
| signal vs record: terminates an invalid message even when the outcome can't be recorded | `TestTerminatesAnInvalidMessageEvenWhenItsOutcomeCannotBeRecorded` | = |
| decideRandomly: approves below the rate, rejects at or above, with a reason | `TestDecideRandomlyApprovesBelowTheRateAndRejectsAtOrAboveItWithAReason` | = |

## `ledger/lifecycle.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| approved: booked into pending_in, then credited | `ledger/postings_test.go` `TestApprovedIsBookedThenCredited` | = | |
| rejected: booked, then returned, reason recorded | `TestRejectedIsBookedThenReturnedWithTheReason` | = | |
| skips every transition when repeated | `TestSkipsEveryTransitionWhenRepeated` | = | |
| skips transitions out of order | `TestSkipsTransitionsOutOfOrder` | = | |
| rejects illegal transitions at compile time | `deposits/deposits_test.go` `TestTransitions`, `TestRejectsInvalidTransitions` | ~ | Go's transitions are a closed set of constants (only those exist); one made by conversion changes nothing, checked at run time. |
| never re-makes a recorded decision | `TestNeverRemakesARecordedDecision` | = | |
| books exactly once under concurrency | `TestBooksExactlyOnceUnderConcurrency` | = | |
| rolls back a transition whose movement already exists | `TestRollsBackATransitionWhoseMovementAlreadyExists` | = | |
| deposit_status_history is append-only | `deposits/deposits_test.go` `TestKeepsHistoryAppendOnly` | = | |

## `ledger/move.test.ts`

All in `ledger/move_test.go`.

| Node case | Go | Mark |
|---|---|---|
| moves money between the right buckets | `TestMovesBetweenTheRightBuckets` | = |
| moves amounts beyond 2^53 exactly | `TestMovesAmountsBeyond2To53Exactly` | = |
| a no-op when called twice with the same reference | `TestIsANoOpWhenCalledTwiceWithTheSameReference` | = |
| refuses a reused reference with different details | `TestRefusesAReusedReferenceWithDifferentDetails` | = |
| exactly once when called concurrently | `TestAppliesExactlyOnceWhenCalledConcurrently` | = |
| can't overdraw a bucket: OverdraftError, rolled back | `TestCannotOverdrawABucket` | = |
| a balance beyond bigint: BalanceOverflowError, rolled back | `TestABalanceBeyondTheBigintRangeIsABalanceOverflowErrorAnInvariantViolation` | = |
| through the held bucket | `TestMovesThroughTheHeldBucket` | = |
| overdrawing held: OverdraftError on held | `TestReportsOverdrawingHeldAsAnOverdraftOnHeld` | = |
| moveOnce: LedgerInconsistencyError on a duplicate | `TestMoveOnceReportsADuplicateAsLedgerInconsistency` | = |
| throws without a balance row | `TestFailsWithoutABalanceRow` | = |
| rejects the same bucket or a non-positive amount | `TestRejectsInvalidMovements` | = |
| balance_movements is append-only | `TestKeepsBalanceMovementsAppendOnly` | = |

## `ledger/withdrawals.test.ts`

| Node case | Go | Mark | Note |
|---|---|---|---|
| hold: available to held, with its first history row | `ledger/withdrawals_test.go` `TestHoldMovesAvailableToHeld` | = | |
| hold: InsufficientFundsError, nothing stored | `TestInsufficientFundsStoresNothing` | = | |
| hold: exactly one of two concurrent withdrawals commits | `TestOnlyOneOfTwoConcurrentWithdrawalsThatExceedAvailableCommits` | = | |
| hold: nothing for a key already used | `TestAReusedKeyHoldsNothingTwice` | = | |
| table: no remittance only as NULL, '' refused | none | ~ | Schema only; not ported on purpose, documented at the top of `ledger/withdrawals_test.go`. |
| table: refuses %s (4 rows: the SEPA character set) | none | ~ | As above. |
| table: accepts every character of the set | none | ~ | As above. |
| markSubmitted: held to submitted, transfer ID stored, no money moved | `TestMarkSubmittedStoresTheTransferIDWithoutMovingMoney` | = | |
| execute from held, from submitted | `TestExecuteFromHeldAndFromSubmitted` | = | |
| release from held, from submitted | `TestReleaseFromHeldAndFromSubmitted` | = | |
| final statuses never change | `TestFinalStatusesNeverChange` | = | |
| the provider's update wins the race | `TestTheProvidersUpdateWinsTheRace` | = | |
| a %s reporting another ID throws, changing nothing (execute, release) | `TestASettlementReportingAnotherTransferIDChangesNothing` | = | Subtests for both. |
| a settlement with the same ID, or none, applies | `TestASettlementWithTheSameTransferIDOrNoneApplies` | = | |
| a repeated update for a final withdrawal | `TestARepeatedUpdateForAFinalWithdrawal` | = | |
| a submission after the update | `TestASubmissionAfterTheUpdate` | = | |
| types: illegal transitions and mismatched references don't compile | `withdrawals/withdrawals_test.go` `TestRejectsTransitionsMissingTheirValues` | ~ | Documented at the top of `ledger/withdrawals_test.go`: a transition's required values are checked at run time; a mismatched movement reference can't be written (`ledger.Reference` has unexported fields and per-entity constructors). |

## `outbox/outbox.test.ts`

All in `outbox/outbox_test.go`.

| Node case | Go | Mark |
|---|---|---|
| enqueue: stores the event with its payload as JSON | `TestEnqueueStoresTheEventWithItsPayloadAsJSON` | = |
| enqueue: rolls back with its transaction | `TestEnqueueRollsBackWithItsTransaction` | = |
| enqueue: throws on a second event with the same msg_id | `TestEnqueueFailsOnASecondEventWithTheSameMsgID` | = |
| claim: id order, leasing, counting the attempt | `TestClaimsInIDOrderLeasingEachEventAndCountingTheAttempt` | = |
| claim: respects the limit, oldest first | `TestClaimRespectsTheLimitOldestFirst` | = |
| claim: skips leased and published, reclaims after the lease | `TestClaimSkipsLeasedAndPublishedEventsAndReclaimsOnceTheLeaseExpires` | = |
| claim: two concurrent claims are disjoint (SKIP LOCKED) | `TestTwoConcurrentClaimsGetDisjointEvents` | = |
| a failure with a retry delay, then publication, each once | `TestRecordsAFailureWithARetryDelayThenPublicationEachOnce` | = |

## `relay/relay.test.ts`

All in `relay/relay_test.go`.

| Node case | Go | Mark |
|---|---|---|
| publishes each event with its msg_id as Nats-Msg-Id, marks it published | `TestPublishesEachEventWithItsMsgIDAndMarksItPublished` | = |
| records a failure, publishes after the retry delay | `TestRecordsAFailureAndPublishesAfterTheRetryDelay` | = |
| publishes again after a crash between publishing and marking; the stream drops the repeat | `TestPublishesAgainAfterARelayCrashedBetweenPublishingAndMarkingAndTheStreamDropsTheRepeat` | = |
| each event to its own stream | `TestPublishesEachEventToItsOwnStream` | = |
| an unknown subject is a publish failure | `TestRecordsAnEventWithAnUnknownSubjectAsAPublishFailure` | = |
| streams match their definitions | `TestStreamsMatchTheirDefinitions` | = |

## `submitter/handler.test.ts`

All in `submitter/handler_test.go`.

| Node case | Go | Mark | Note |
|---|---|---|---|
| submits a held withdrawal, built from the stored withdrawal | `TestSubmitsAHeldWithdrawalBuiltFromTheStoredWithdrawal` | = | |
| records nothing on a redelivery after the submission was recorded | `TestRecordsNothingOnARedeliveryAfterTheSubmissionWasRecorded` | = | |
| an acceptance for a withdrawal settled meanwhile: duplicate | `TestTreatsAnAcceptanceForAWithdrawalSettledMeanwhileAsADuplicate` | = | |
| no call for a withdrawal no longer held | `TestMakesNoCallForAWithdrawalNoLongerHeld` | = | |
| releases the funds on a refusal | `TestReleasesTheFundsOnARefusal` | = | |
| naks an unknown outcome with a growing delay; the redelivery submits | `TestNaksAnUnknownOutcomeWithAGrowingDelayAndTheRedeliverySubmitsIt` | = | |
| treats %s as our bug (invalid 400, key_conflict 409) | `TestTreatsInvalidAndKeyConflictAsOurBug` | = | Both rows, and a Go-specific one: an outcome of no kind (an adapter bug). |
| terminates a bug outcome even when it can't be recorded | `TestTerminatesABugOutcomeEvenWhenItsOutcomeCannotBeRecorded` | = | |
| a transfer-ID mismatch from markSubmitted is an invariant violation | `TestTreatsATransferIDMismatchFromMarkSubmittedAsAnInvariantViolation` | = | |
| terminates a ledger invariant violation with an alert | `TestTerminatesALedgerInvariantViolationWithAnAlert` | = | |
| naks a transient failure after an accepted call; the redelivery records it | `TestNaksATransientFailureAfterAnAcceptedCallAndTheRedeliveryRecordsIt` | = | |
| terminates an invalid event (%s: not JSON, not withdrawal.held) | `TestTerminatesAnInvalidEvent` | = | Read: the same two rows. |
| terminates an event for an unknown withdrawal | `TestTerminatesAnEventForAnUnknownWithdrawal` | = | |
| logs no name, IBAN or remittance text | `TestLogsNoNameIBANOrRemittanceText` | = | |

## `submitter/provider.test.ts`

Node runs these against provider-sim; Go's `provider/provider_test.go` against `httptest` servers that answer as provider-sim does (a real HTTP server on a local port). The cases are the same; the fake differs.

| Node case | Go | Mark | Note |
|---|---|---|---|
| 201, then 200 for a repeat of the key: accepted, same transfer ID | `TestMaps201Then200ForARepeatOfTheSameKeyToAcceptedWithTheSameTransferID` | = | |
| 422: refused, with the reason | `TestMapsA422ToRefusedWithTheReason` | = | |
| 400: invalid, with only a summary (field paths and size) | `TestKeepsRequestValuesAProviderEchoesInA400OutOfTheOutcome` | ~ | Read: Node provokes provider-sim's own validation (a name with "Zoë"); Go's fake answers a 400 with field paths, and an odd-shaped one: fields and size kept, nothing else. The mapping is the same. |
| keeps request values a provider echoes in a 400 out of the outcome | `TestKeepsRequestValuesAProviderEchoesInA400OutOfTheOutcome` | = | |
| 409: key_conflict | `TestMapsA409OurKeyWithAnotherBodyToKeyConflict` | = | |
| 503: unknown; the retry with the same key is accepted | `TestMapsA503ToUnknownAndTheRetryWithTheSameKeyIsAccepted` | = | |
| a connection failure: unknown | `TestMapsAConnectionFailureToUnknown` | = | |
| a timeout: unknown; the next call with the same key gets the same, single transfer | `TestMapsATimeoutToUnknownAndTheNextCallWithTheSameKeyGetsTheSameSingleTransfer` | = | |

## Go tests with no Node counterpart

In the same scope (the foundations' own tests aren't listed). Grouped by why they exist.

**Parity with Node's behaviour, measured against Node's real server (Node doesn't test it; Go has to, because Go doesn't get it from Fastify or Zod):**
- `api/accounts_test.go`: `TestTransactionsLimitMessagesAreZods`, `TestTransactionsReportsEveryQueryProblem`, `TestTransactionsLimitIsReadAsNodeReadsIt`.
- `api/http_test.go`: `TestDecodesEscapesInThePath`, `TestEscapedSlashesAndDotsArePartOfThePathValue`, `TestHeadAnswersTheHeadersOnly`.
- `api/middleware_test.go`: `TestRecoversAPanicWith500`, `TestReportsAPanicAfterTheResponseStartedAndAborts`, `TestJSNumber`.
- `api/webhook_test.go`: `TestReportsFieldProblemsAsZodDoes`, `TestANullProviderAccountIDIsInvalid`, `TestKeysMatchExactlyAndTheLastDuplicateWins`, `TestCountsLengthsInCodePointsAsZodDoes`, `TestAnswersAFramingErrorWithRequestRejected` (the "request rejected" log line; Node has no test of it), `TestAnswers500WhenTheBodyCantBeRead`.
- `api/webhook_internal_test.go`: `TestRejectUnknown` (the strict-body helper).
- `api/withdrawals_test.go`: `TestAnIdempotencyKeySentTwiceIsInvalid` (Node's HTTP layer joins a repeated header; Go reproduces it).
- `outbox/outbox_test.go`: `TestClaimGivesThePayloadAsCompactJSON` (the bytes Node publishes).
- `provider/provider_test.go`: `TestSendsNodesExactRequest`, `TestTimesOutABodyThatStallsAsUnknown`, `TestMapsAnswersItCantReadToUnknown`, `TestABodyOverTheLimitIsUnknownUnlessTheStatusDecides`.

**Go-only behaviour (decisions 58 and 59, and Go's shutdown):**
- `consumer/consume_test.go` `TestStopWaitsForInFlightMessagesWithinTheGracePeriod` also checks the handlers' context is released on a clean stop.
- `api/middleware_test.go` `TestEveryRequestHasADeadline` (58).
- `depositworker/handler_test.go` `TestACancelledReviewIsNakedAsTransientWithNothingPosted` (59).
- `consumer/consume_test.go` `TestAfterTheGracePeriodHandlersAreCancelledAndTheirMessagesRedelivered` (59).
- `consumer/lifecycle_test.go` `TestDoneAfterTheHandlersContextIsCancelledStillRecordsAndAcks` (59).
- `relay/relay_test.go` `TestShutdownFinishesTheBatchInProgress` (Node checks this by hand).
- `cmd/api/main_test.go` `TestASecondSignalEndsTheShutdownAtOnce`.

**The consumer layer tested on its own** (Node tests these through the deposit-worker's handler tests, or in its e2e suites):
- `consumer/consume_test.go`: `TestFetchesOnlyAsManyMessagesAsThereAreFreeSlots` and `TestALongHandlerNeverCausesARedelivery` (Node: e2e), `TestStopEndsAWaitingFetchAtOnce`, `TestStopWaitsForInFlightMessagesWithinTheGracePeriod`, `TestAMissingConsumerFailsAtStartAndEndsTheLoopAfterRepeatedAttempts`, `TestAPanickingHandlerIsLoggedAndFreesItsSlot`, `TestStartConsumingRefusesUnusableOptions`.
- `consumer/lifecycle_test.go`: `TestDoneRecordsTheOutcomeThenAcks`, `TestStopAndRetryLaterSignalFirstThenRecord`, `TestRetryLaterUsesTheBackoffForTheAttemptAndRetryAfterItsOwnDelay`, `TestAnErrorAfterSettlingIsLoggedNotSettledAgain`, `TestASignalThatCantBeSentStillSettles`, `TestLogLinesCarryTheDeliveryFieldsAndBind`, `TestRetriesATransientFailure`, `TestDoneThatCantRecordRetriesInsteadOfAcking`, `TestADeliveryFinishesOnce`, `TestAPanicInTheHandlerIsClassifiedLikeAnError`; and, duplicating the deposit-worker's cases at this level, `TestTerminatesAMessageThatIsNotJSONLoggingTheDeliveryWithoutAnEventID`, `TestTerminatesAnInvalidEventKeepingItsEventIDInTheLog`, `TestNaksWhenTheDeliveryCantEvenBeRecorded`, `TestTerminatesAnInvalidMessageEvenWhenItsOutcomeCantBeRecorded`.
- `consumer/process_internal_test.go`: `TestRunFailsAtStartupWithAStartupFailedLine`, `TestRunHandlesMessagesAndShutsDownOnASignal`.
- `depositworker/handler_test.go`: `TestAWorkingSignalThatCantBeSentDoesntStopTheReview`, `TestAZeroWorkingIntervalReviewsInOneStretch`.

**Run-time checks where Node relies on types:**
- `compliance/compliance_internal_test.go` `TestDecideRefusesWhatIsntADecision`.
- `deposits/deposits_test.go` `TestParseStatus`, `TestTransitionStatus`.
- `withdrawals/withdrawals_test.go` `TestParseStatus`, `TestTransitionStatusSkipsAnUnknownWithdrawal`, `TestGetMissing`, `TestAsAccountNotFound`.
- `ledger/move_test.go` `TestIsInvariantViolation`; `ledger/withdrawals_test.go` `TestUnknownAccountIsAccountNotFound` (Node covers an unknown account only through the API's 404).

**Configuration** (Node: `shared/config.test.ts`, out of scope here): `cmd/relay/config_test.go` (the lease rules), `cmd/deposit-worker/config_test.go`, `cmd/submitter/config_test.go`.
