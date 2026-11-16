# No Shared Lock or Engine Pass Across a Network Call

Unit #3215 (tracker #3201) makes the daemon hold no lock and no engine pass
across a GitHub, model, research, or ward call, so one project's hung call
never stops another project's work. One supervised daemon now runs every
project (plan revision 125), so a wait that once stalled one deputy run
would stall them all. The issue body lists the call sites; this note keeps
the decisions that rejected a reasonable option, the older records it
revises, and the tests it changed.

## Records This Revises

Each older note stays frozen; this note carries the correction.

- **`devlog/2026-09-28-1800-capacity-scheduling.md`, one call per site.**
  Its "Each call site runs one call at a time" now reads as a per-pool
  limit, `max_calls_per_site`, starting at one. A second call to a busy
  site waits its turn instead of getting the site's fail-safe
  (`call_in_flight`). Plan revision 125 already chose this; the note's
  bound changes from "at most one request per site" to "at most the
  pool's limit per site". Migration 0143 writes one into every stored
  pool, so no deployment's concurrency rises on upgrade. The owner
  accepted waiting over the fallback with the issue's plan.
- **`devlog/2026-07-25-1233-janitor-gate-match-scope.md`, "No token
  escapes an in-flight pass".** That held because a pass withdrew all
  coverage before it ran, so every mint and every admission base fetch
  waited for the whole pass, GitHub calls included. Chose to serve the
  last complete pass's coverage while a pass runs, over withdrawing for
  the pass. A token can now be minted on coverage one pass old. That
  staleness is bounded: a pass ends within the GitHub client's 30-second
  request timeout times the janitor's page and removal caps, a clean pass
  replaces the whole view, and a failed pass still withdraws it. The
  post-match re-gate in `mint.go` still runs against the served view. The
  rule reaches one Tier A transition beyond mints and base fetches:
  `freesided onboard` activates a pending installation through the same
  gates (`AwaitPendingReady`, `AwaitAllowsRepository`), so it can now
  activate on a view one pass old instead of waiting out the pass in
  progress. The owner accepted the rule with the issue's plan.

## Decisions

- **Chose one hung request over a silent registration for the token stall
  tests.** Every installation resolution lists every registration's
  installations, because the ambiguity rule must see them all. A
  registration GitHub never answers therefore still stalls each
  resolution until the request timeout. What this unit removes is the
  lock around it: a cached token, and another repository's resolution,
  no longer wait behind it. Dropping the full listing would weaken the
  ambiguity rule.
- **Chose to key publish passes by project, and filings by repository.**
  Replies, issue comments, and hand-back label removal start from a run,
  which names a project, and a project names one repository. Filings key
  by repository because the rate cap and the one-outstanding rule count
  per repository and must not race. Label intake keys by repository. A
  key whose last run has not returned is skipped, not queued, so a
  stalled repository never piles up runs (`internal/keyedrun`).
- **Chose a notice held by either drive over a notice held by the last
  finished drive.** Passes now overlap, so a pass that skipped a stalled
  repository would otherwise resolve a notice the stalled drive is about
  to hold again.
- **Chose engine workers that accept their own discussion reply over a
  next pass that collects it.** A reply that won its claim must still
  land when its workflow closes meanwhile, and a closed workflow's intent
  is no longer pending, so a collecting pass never finds it. The worker
  accepts through the same pre-commit gate. What it accepted, and any
  error that would have stopped the pass, reach the next pass, which
  counts the one and returns the other, so the loop still stops on a
  correctness error. A failed triage reaches the next pass the same way.
  Each worker re-reads its state before its call, because no lock spans
  the pass's check and the worker's start: a run for the same key that
  finished in between may already have answered.
- **Chose a triage hold that records no reason.** A specifier whose
  triage call runs is held with `errSpecifierTriagePending`, an
  invocation hold with no hold reason, because nothing waits on an
  operator. `TestDispatchHoldReasonCoversEveryHoldClass` gains it in the
  reasonless set and the hold-classification golden gains its row.
- **Chose research results kept in memory for the next pass over a new
  stored record.** A restart forgets them, and the next fetch recovers
  each registered artifact, so a record would add a table for no
  correctness gain.
- **Chose to move a failed run's advisory diagnosis off the pass too.**
  The issue's list missed it. It ran inline when the pass recorded a
  failed terminal, and with a waiting site the pass could also wait out
  an abandoned diagnosis's provider teardown. The claim is advisory and
  no later step of the pass reads it.
- **Chose asynchronous Inspect recovery in the stage driver.** Inspect of
  an orphaned invocation starts ward recovery in a goroutine and reports
  the invocation running; a later Inspect reads the phase recovery left,
  and a non-retryable recovery error comes back from the next Inspect.
  Close waits for recoveries. The shared stage contract case for a crash
  before the result (`internal/exec/contract/stage.go`) now polls Inspect
  until the answer is no longer a non-live running one, instead of
  reading the recovered phase from the first Inspect. Every backend still
  has to reach the same terminal answer.
- **Chose to check start authority before the driver's lock and leave the
  first commit-author check inside the engine pass.** `StartWithInputs`
  authenticates the start before taking the driver's mutex, which warms
  the per-invocation author cache, so the re-check under the lock makes
  no network call. Moving that first check off the engine pass needs a
  new pending-start result across about ten packages' tests and isn't in
  the issue's acceptance criteria. Follow-up: #3224.

## Not Done Here

- **Admission base fetch (part 7) waits for #3203,** which changes the
  same path and was open when this unit started.
- **The composed two-project test (part 8) is its own unit.** It needs a
  daemon fixture that runs two projects at once, which no test helper
  builds yet.

## Janitor Tests Changed

The issue allows a janitor "denied while a pass runs" test to change only
with the coverage rule. Each change:

- **Deleted, replaced by `TestCoverageGatesServeTheLastPassWhileOneRuns`:**
  `TestStableCoverageExcludesReconciliationWithdrawal` and
  `TestCoordinatedCoverageDoesNotRelockStableSection`. Both tested
  `WithStableCoverage`, which existed only to hide the in-pass withdrawal
  and is removed.
- **Renamed:** `TestResolverWaitsForScheduledPassPublication` to
  `TestResolverServesCoverageDuringScheduledPass`. It reproduces the same
  run-482 outage shape and now asserts the resolver answers from the
  served view during the pass.
- **Added:** `TestScheduledPassFailureWithdrawsCoverage`, the other half
  of the rule.
- **Bodies changed:**
  - `TestInstallationJanitorFaultsOutliveTheGateTheyExplain` now expects
    the last pass's coverage to hold while the next pass runs, and the
    coordinated gate to wait only for the faulted registration.
  - `TestInstallationJanitorRunActivatesOnlyAfterCleanPass` now expects
    the last pass's coverage and grant to stay allowed while the next
    pass is blocked, where it expected both withdrawn. Shutdown still
    withdraws them.
  - `TestJanitorSessionPrimesCoverageSynchronously` drops its
    `WithStableCoverage` step.
  - `TestRunClaudeConformanceBindsExactNamespaceBeforeSetup` drops the
    removed wrapper argument.

The named gate tests in the issue pass unchanged.

## Refute-First Findings

A fresh-context review tried to break the change.

- **Confirmed and fixed:** a failed triage only logged and retried each
  pass, where it used to stop the loop. A worker could repeat a call that
  a run for the same key had just finished. The failed-run diagnosis still
  ran inside the pass. Onboarding's use of the coverage gates was missing
  from this note.
- **Allowed:** the start-authority cache can keep an entry an import just
  forgot, when an authentication already in flight writes it back. No
  later start reads that entry, and the cache's size cap clears it.
- **Disproved by reading the paths:**
  - A token cache hit never skips the current-authority check.
  - Registrations never share an entry, and one key mints once.
  - The janitor gate never allows without a complete pass behind it.
  - A call slot is never leaked on cancel or timeout.
  - A skipped key's work is picked up by the next pass.
  - A stage recovery never outlives the driver's `Close`.

Revisit when a janitor pass can run long enough that one pass of
staleness matters, such as a cap raised past what the request timeout
bounds.
