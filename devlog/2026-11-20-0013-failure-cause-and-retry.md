# A Failure Card Names Its Cause and Offers Retry Only When a Retry Could Work

#3440 makes an execution failure record why it failed and lets the card
offer a plain Retry when a retry could succeed. A lost or failed run now
carries the ward check that refused it, with screened evidence, or the
gauntlet's first findings. Every new card carries a retry outlook
(`would_repeat` or `may_succeed`) that the daemon derives from the
recorded failure. Retry is offered only for a `may_succeed` failure that a
repeat can clear, and accepting it starts exactly one new attempt bound to
the operator's command. The owner kept the unit whole: the typed cause,
the outlook, the Retry path, and the app's rows land in one PR.

## A Lost Outcome May Name the Check That Lost It

Chose to let a `lost` outcome carry an optional handoff check over
inventing a failed outcome for F42. A handoff check that refuses a running
invocation commits nothing; recovery later proves the loss. The live path
stashes the check on the stage intent, and the loss commit falls back to
it, so the live and recovery paths name the same check. A failed outcome
with cause `handoff_check` must carry one; no other outcome may.

`sameOutcome` ignores the check when it compares lost outcomes, so the
first diagnosis wins. A crash between the outcome write and the intent
advance replays recovery against a journal ward already closed, which
names no check; without that rule the replay would conflict with the
stored outcome and wedge the invocation.

## Evidence Is Screened Where It Is Made and Refused Unscreened

Ward formats a failure's underlying error into its reason, and that error
can name host paths or quote content. `ScreenFailureEvidence` removes the
wrapped error's text, replaces every credential-rule match with `[redacted]`, and
bounds the result to 512 bytes, keeping head and tail because a wrapped
reason ends with its specific cause. If replacement pushes the result back
over the cap or a token completes another rule, the evidence falls back to
the bare token, because `Validate` refuses unscreened evidence and a
refusal there would hold the outcome write. Export findings show at most
five kinds and paths plus the total; a credential-shaped path shows only
as its `path:<digest>` token, which an operator holding the path can
recompute.

Chose to project export rejection to clients now, which #768 had kept
daemon-side, because F60's card is useless without the findings. The
rejection record is written best-effort after the outcome, so a card
raised in that window has no findings; the API says so.

Some ward reasons build runtime error text into the reason itself with no
wrapped error (teardown's problem list, the recovery audit), so that text
stays, still screened by the credential rules. Chose to accept it over
re-plumbing ward's error construction: it is the operator's own host
diagnostics, shown on the operator's own clients, and the credential
rules are what keep secrets out.

Chose to write the rejection detail before the outcome. The engine can
raise the card as soon as the outcome commits and reads the findings once,
so the old order could leave a card without them for good. The detail
binds only to the admission, so it needs no outcome first. The write
stays best-effort, so a failed detail write still leaves a card without
findings.

## The Outlook Is Derived, Never Stored as a Property of a Cause

`DecideRetryOutlook` reads the outcome, cause, and check: content evidence
or a cause in the run's own output or configuration (`export_rejected`,
`invalid_evidence`, `invalid_configuration`, `contract_contradiction`,
`memory_limit`) gives `would_repeat`; anything else gives `may_succeed`.
Card facts `Validate` re-derives it, so a stored outlook can't disagree
with its failure. A `would_repeat` card offers no Retry and says so.

## Retry Is Narrower Than `may_succeed`

`OffersSameAgentRetry` also requires a loss, a cancellation, or a failure
the provider caused or nobody typed. Quota, credential, and capacity
causes keep their own remedies (the quota park, re-enrollment, an
alternate agent). Implementation Retry restarts the run, so it is offered
only for the implementer: a lost remediation round would discard reviewed
work. Review and verification stages are not offered it. A specification
card offers Retry only while the iteration budget allows one, so the card
never offers what acceptance would refuse.

## The Command Binds the Attempt, and Replays Start Nothing

- **Implementation:** a plain retry attempt binds the command and the
  failed invocation, two bindings instead of the capability and alternate
  retries' three. The store re-authenticates it against the accepted
  command, the resolved card that still offers Retry, and the parent run;
  a forged command or another invocation allocates nothing. A replay finds
  the existing attempt.
- **Specification:** the iteration's retry reason embeds the command id,
  and a scan of every iteration runs before the task-open gate, so a
  replay after the task moved on still converges instead of failing.

## Why AttentionItem.Validate Does Not Enforce the Offer

Chose to enforce the Retry offer at acceptance (signet), at the attempt
gate (store), and at reconciliation (`retryCommandResolved`) over adding
an `AttentionItem.Validate` rule. Existing rows and fixtures offer Retry on
failure cards without an outlook; a Validate rule would make them
unreadable. Every path that acts on Retry calls `OffersSameAgentRetry` on
the stored card, which is false for any card without an outlook, so an old
offer authorizes nothing.

## Tier A Paths

`domain/handoff_check_failure.go` (the evidence and path screens),
`engine/failure_retry.go`, and `engine/specification_retry.go` join the
Tier A list. `OffersSameAgentRetry` moved into `domain/alternate_agent_retry.go`,
already listed, beside the other failure-card offer rules.

## Refute-First Findings

Fresh-context reviewers tried to prove the change wrong in three rounds
before push.

- **Confirmed and fixed:** rebuilding a stored failed `handoff_check`
  outcome after a crash (`restoreDurableOutcome`, `adoptRecordedOutcome`)
  dropped its check, so the rebuilt result failed validation and wedged
  recovery. Both now copy it, and a test covers each.
- **Confirmed and fixed:** the rejection detail was written after the
  outcome (above).
- **Confirmed and fixed (round 2):** an implementation run outside a
  campaign offered Retry, and every reconcile pass then failed to find its
  attempt and stopped before dispatch. Retry now needs a campaign.
- **Fixed (round 2):** a specification card read its chain for the budget
  even when its facts could never offer Retry; it now reads it only when
  they could.
- **Confirmed and fixed (round 3):** a paired device could submit a
  retry whose command id had surrounding whitespace. Each retry reconciler
  writes the id into a reason that must be trimmed, so the accepted
  command failed every pass and stopped dispatch. Signet now refuses an
  untrimmed id for Retry, Retry with capability, and Retry with another
  agent; the last two had the gap before this unit.
- **Allowed by decision:** runtime error text built into a ward reason
  without a wrapped error stays in the evidence (above).
- **Declined (round 2):** each pass replays an accepted Retry through
  `ReattemptProductionRun`, which finds the bound attempt and starts
  nothing. The capability and alternate-agent retries replay the same way.
- **Declined (round 3):** the seed fixture's failure card now offers a
  working Retry beside a live later attempt. Real flows supersede an
  earlier attempt's card when the next attempt is submitted, so only the
  seeded state reaches it.
- **Confirmed, deferred:** an untyped failure records cause `unknown`,
  which offers Retry even where the failure is deterministic, such as a
  specification whose base left its branch. The specification budget
  bounds the repeats, and dropping `unknown` would withdraw Retry from the
  commonest retry-worthy case, a writer that exited nonzero. The fix is to
  type that cause. Follow-up: #3515.
- **Confirmed, deferred:** `freesided reattempt` doesn't check an accepted
  but not yet reconciled Retry, so an operator racing their own command
  could start two attempts. Capability and alternate-agent retries share
  the gap, which predates this unit. Follow-up: #3516.

Revisit when a failure cause gains a remedy a plain repeat could reach
(for example a credential that refreshes itself), or when review or
verification stages get a retry path of their own.
