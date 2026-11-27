# A Base That Left Its Branch Is a Typed Cause, and the Stage Joins the Outlook

#3515 types the deterministic failure that #3440's review found behind
cause `unknown`: a specification whose recorded base left its branch fails
the same way on every Retry, because a specification Retry adds an
iteration to the same run and keeps its base. The new cause
`base_left_branch` withdraws Retry there and keeps it for an implementer,
whose Retry starts a new run at the branch tip. The audit of the other
`unknown` sites types one more refusal as `invalid_configuration` and
leaves the rest untyped, for the reasons below.

## The Outlook Depends on the Stage

Chose to derive the outlook from the stage as well as the outcome, cause,
and check, revising #3440's note
(`devlog/2026-11-20-0013-failure-cause-and-retry.md`, "The Outlook Is
Derived"). Whether a base that left its branch repeats depends on what a
Retry keeps: `retrySpecificationRun` keeps the run and `runBase` returns
its recorded base, while `ReattemptProductionRun` builds a run with no
base, which takes the current tip. Rejected: a second cause per stage,
which would encode the remedy in the cause, the thing #3440's note ruled
out. Every other cause keeps one outlook for every stage, so stored cards
still validate.

`repeatCanClear` accepts the cause, and the outlook check in
`OffersSameAgentRetry` withholds it from a specification card, so signet,
the store's attempt gate, and `retryCommandResolved` follow without change.
No alternate agent qualifies: another agent would seed the same base.

## Where the Cause Is Recorded

- **The driver's seed.** `classifyTransportSeedError` wraps
  `publish.ErrRemoteMissingBase` in both `ErrSeedRefused` and a new
  `ErrSeedBaseLeftBranch`, so every existing `ErrSeedRefused` check holds,
  and `failureCause` maps the new sentinel. A verdict returned after the
  seed's context was canceled is not trusted (Refute-First Findings).
- **The engine's stop of an admitted invocation.** Only an attempt that
  `readmitStaleAdmission` admitted again reaches the stop with an
  admission. The specification lane records the cause; the production
  delivery refusal records it on the outcome and facts but still offers
  only Acknowledge and no outlook.
- **No cause without an outcome record.** A specification stopped before
  admission keeps its fact-less card. `gateExecutionFailureItem`
  authenticates facts against the outcome record, and giving that card a
  cause would need an outcome with no admission to bind to.

Each terminal write retires its intent or outbox entry in the same step,
and recovery adopts the stored cause (`restoreDurableOutcome`,
`adoptRecordedOutcome`), so no replay rebuilds an outcome stored as
`unknown` before this change and compares it with a typed one.

## Audit of the Other `unknown` Sites

The rule: a failure whose remedy is an operator change to state that a
Retry reads again stays `unknown`, because Retry works once the change is
made; a failure fixed by inputs the Retry keeps gets a typed cause.

- **Typed `invalid_configuration`:** a start refused for its registry set
  (`startRegistryHosts`). A reattempt copies the parent's policy artifact,
  adoptions are validated on write and only accumulate, and a
  specification never runs under `provider_registry`.
- **Stays `unknown`, with a follow-up:** a seed whose base tree fails the
  materialization rules (`publish.ErrMaterializationRefused`). It repeats
  on a specification Retry, but an implementation Retry seeds the
  branch's current tip, where a pushed fix clears it, so
  `invalid_configuration` (would_repeat for every stage) would withdraw a
  Retry that can work. It needs a stage-dependent cause of its own, which
  this unit's contract doesn't name. Follow-up: #3749.
- **Stays `unknown`:** a prompt refusal in `StartWithInputs`, because the
  rendered prompt includes protected paths from the repository's latest
  trust profile, which an operator can narrow before retrying; a seed
  refused for credentials or access; a writer that exits nonzero; a
  cordoned launch that fails before claiming its directory, ends nonzero,
  or has its export refused (tied to this run's worktree or transient);
  recovery failures tied to this invocation's files.
- **Already typed:** the handoff-spec refusals in `exec/stage/cordon.go`
  and `pipeline.go` wrap `ErrUnsupportedStart` in `handoffSpecRefusal`,
  which records `invalid_configuration`.
- **No card to change:** an undeliverable production input offers only
  Acknowledge; an undeliverable specification input never has an
  admission; review cards offer no Retry.
- **Unreachable here:** a cordoned seed runs after its launch is claimed,
  so a base that left its branch would end `lost`. Specifications never
  run cordoned (`admitAttemptIn` resolves a writer class only for
  implementers and remediators), and an implementer's loss offers Retry,
  which is right for this cause.

## Refute-First Findings

The parent's review of the implementation and a fresh-context reviewer
from another provider tried to prove the change wrong before push.

- **Confirmed and fixed:** typing the materialization refusal
  `invalid_configuration` withdrew an implementer's working Retry (above).
- **Confirmed and fixed:** the first cut also typed the prompt refusal in
  `StartWithInputs` `invalid_configuration`; tracing what a Retry reads
  showed the protected paths come from the latest trust profile, so it
  stays `unknown`.
- **Confirmed and fixed (round 2):** `fetchBase` wraps any failure of its
  ancestry checks as `ErrRemoteMissingBase`, including a git process
  killed by a canceled context, so a specification seed canceled at
  shutdown would have recorded `base_left_branch` and lost its Retry.
  `seedBase` now drops the typing when the seed's context was canceled,
  which keeps the outcome this case had before (`failed`, `unknown`).
  The transport's conflation predates this unit. Follow-up: #3750.
- **Confirmed, deferred (round 3):** a git failure with the context still
  live (a killed `merge-base`, an exit 128) is wrapped the same way and
  still records `base_left_branch`. Only the transport can tell a real
  ancestry miss from a git error, and `publish` is outside this unit.
  It needs git to fail inside a freshly cleared seed checkout, and it
  costs one specification card its Retry offer, with Discuss and Stop
  left. Follow-up: #3750.
- **Disproved by a trace:** a recovery replay rebuilding an outcome stored
  as `unknown` and comparing it with `base_left_branch`. The driver adopts
  the stored cause, and both engine recorders retire the outbox entry in
  the transaction that writes the outcome.
- **Declined:** an implementer re-admitted by `readmitStaleAdmission` whose
  base left its branch gets the delivery-refusal card, which offers only
  Acknowledge. It offered only Acknowledge before this unit, and adding
  Retry there changes that card's decision surface, which the plan kept.
- **Declined:** a specification discussion whose base left its branch
  posts an unavailable reply with no execution outcome. It raises no
  failure card, so there is no Retry to withdraw.
- **Declined (round 2, raised again):** the re-admitted implementer's
  Acknowledge-only card above.

Revisit when a specification Retry starts from a fresh base, when a
retry attempt's run inherits its parent's base, or when specifications
gain a cordoned runner class.
