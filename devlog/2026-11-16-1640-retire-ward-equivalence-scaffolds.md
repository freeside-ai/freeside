# Retire the Ward Extraction-Proof Scaffolds

The owner decided on 2026-11-16, on the daemon test audit's recommendation,
to retire the `internal/ward` tests that compared the shared review and
credential-refresh code with reconstructed copies of the code it replaced
(#3257). They proved the #872 extraction; afterwards they only held the code
to the old behavior, and each protocol change since (#1212, #1291, #2110,
#2197) had to annotate them.

## Decisions

- **Retire them over keeping them as regression guards.** A scaffold pins
  equality with deleted code, not a behavior Freeside commits to. Every
  regression a scaffold could detect is still caught by a direct test,
  mapped below.
- **Tier A, though only tests change.** `internal/ward/` is a Tier A path.
  A test-only change there is Tier B unless it adds or removes a test of a
  Tier A gate. Deleting `FuzzReviewNormalizeCollectionEquivalence` removed
  the only check of two review-output refusals, and the unit added them back
  as direct rows, so it took the refute-first pass and a fresh-context
  review.
- **Three behaviors got new direct tests first.** Review output with bytes
  after the result object, review output with an unknown top-level key, and
  a credential refresh that returns no ID token (the stored one stays). Each
  assertion was shown to fail when the branch it guards is disabled.
- **Two Codex tests became plain tests.** The six Codex provider strings are
  pinned against literals (`TestCodexReviewProviderConstants`), since
  nothing else names three of them. The intent-state near-member strings
  stay as a plain invalidity test.
- **Dropped on purpose:** the byte-for-byte comparison of the committed auth
  file with the old rotation, the side-by-side launch-spec assembly, and the
  three fuzz targets. The old launch-spec builder called the same
  `validateCodexReviewRequest` and `validateCodexReviewAgentSpec` as the
  current one, so it asserted no refusal of its own. Its seeds ran some
  refusals that no direct test feeds (a Codex review on a setup token or an
  unknown auth mode, an unknown boundary, a proxy URL with a path or an
  `https` scheme, an empty prompt), but a regression in the validator would
  have changed both sides alike, so the scaffold never guarded them. Direct
  tests for them are follow-up work outside this unit's contract
  (Follow-up: #3284).

## Finding: Trailing Review Output Is Refused One Layer Earlier

The contract expected trailing bytes to be refused as "trailing structured
output". They aren't: `RejectDuplicateJSONKeys` runs first, refuses anything
after the top-level value, and reports it as "malformed structured output".
The strict decode's trailing branch in `normalizeCollection` is a second
layer this input never reaches; with the first layer disabled it fires. The
new row expects the message the code gives.

## Refute-First Findings

- **Confirmed and fixed here:** this note first claimed every refusal the
  scaffolds exercised had a direct test; the launch-spec seeds above show
  otherwise, and the wording now says what the scaffolds could detect.
- **Disproved by a check:** that the new tests pass vacuously. Allowing
  unknown members, skipping the duplicate-key pass's trailing check, and
  overwriting the ID token unconditionally each fail a new assertion.
- **Disproved by a check:** that the deletion strands a helper or a
  reference. `go vet` is clean and no name from the deleted files remains
  outside `devlog/`.

## Coverage Map

| Scaffold | What it exercised | Test that still catches it |
|---|---|---|
| `FuzzReviewNormalizeCollectionEquivalence` | Out-of-domain severity, missing location, unmarked (0,0), inverted range, duplicate key, missing findings array | `TestCodexReviewSourceRejectsInvalidFindingsEnvelope` |
| | Output that isn't JSON, or is empty | The duplicate-key pass's refusal branch, which that test's "duplicate" and "trailing bytes" rows and the truncated "malformed json" row of `TestClaudeReviewCollectionEnvelope` (same `normalizeCollection`) reach. No surviving row feeds an empty result. |
| | Trailing bytes, unknown top-level key | New rows in `TestCodexReviewSourceRejectsInvalidFindingsEnvelope` |
| | Nonzero exit, exit 78, a terminal failure at exit 0 | `TestCodexReviewTerminalFailureOverridesEmptyFindings` |
| | A credential refresh attempt inside the container | `TestCodexReviewSourceNamesInContainerRefreshAttempt` |
| `TestReviewBuildAgentSpecEquivalence`, `FuzzReviewBuildAgentSpecEquivalence` | Refused boundary, proxy URL, workspace path, endpoints, and auth file | `TestCodexReviewRefusesResumeAndContinuity`, `TestCodexReviewRejectsCredentialBearingProxyURL`, `TestCodexReviewRejectsWorkspaceControlPathOverlap`, `TestCodexReviewProviderOnlyIsExact`, `TestCodexReviewAuthSnapshotFailsClosed` |
| | The built spec and binding | `TestBuildCodexReviewAgentSpecConforms` (golden `codex-review-spec`), `TestBuildCodexReviewAgentSpecOmitsWorkspaceShadowWhenAgentsAbsent`, `TestCodexReviewAPIKeyStaysOutOfEnvironment` |
| `FuzzReviewCommandEquivalence` | Nothing that can differ: `codexReviewProvider.reviewCommand` only returns `codexReviewCommand` | None needed |
| `TestCodexAuthRefreshExtractionMatchesLegacyDecisionCorpus` | Refresh token not rotated, empty, or equal to an old token; access token malformed, empty, or too short-lived | `TestPrepareCodexReviewAuthRejectsInvalidReturnedTokensBeforeMutation` (an empty access token fails the same expiry-parse branch as its malformed row) |
| | A successful rotation | `TestPrepareCodexReviewAuthRotatesUnderLease` |
| | No returned ID token | `TestPrepareCodexReviewAuthKeepsStoredIDTokenWhenProviderOmitsIt` |
| `TestReviewProviderConstantsMatchBase` | The six Codex provider strings | `TestCodexReviewProviderConstants` |
| `TestCodexReviewIntentStateValidEquivalence` | Registered states and the zero value; near-member strings | `TestCodexReviewEnums`; `TestCodexReviewIntentStateRejectsNearMembers` |

The devlog notes that describe the scaffolds
(`2026-08-20-2332-review-runtime-core.md`,
`2026-08-27-1106-review-launch-spec-fuzz.md`,
`2026-08-27-1545-deleted-file-review-location.md`) stay as they were; they
record what was true then.

Revisit when the review or credential-refresh code is extracted or replaced
again: that change needs its own comparison test while it lands, per
`docs/agent-workflow.md` §refute-first, not a permanent one.
