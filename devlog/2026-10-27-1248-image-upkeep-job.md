# Image-Upkeep Job

Work unit: #2333, Parts A and B of its implementation plan. Plan:
`docs/plan.md` §5.7, Project-image upkeep (revision 98). Contract: #2332
(`devlog/2026-10-26-2241-admitted-image-pointer.md`).

## Decisions

**The owner split the unit: this PR ships Parts A and B, and selection
waits.** Part C would have moved writer image selection from `-agent-image`
to the pointer. Ward's full conformance suite, its PreJob probe, and the
backend configuration digest every admission records are all bound to the
one configured `AgentImage` (`internal/ward/config.go`, `backend.go`,
`suite.go`). A writer running a pointer-selected image would run an image
conformance never proved, under an admission whose conformance identity
names the seed. The plan did not address this, and fixing it means changing
`internal/ward`, outside the unit's paths. Of three options (split, accept
the gap and record it, or widen to ward), the owner chose the split
(2026-10-27). Part C is a follow-up unit that first binds conformance to
the selected image. Follow-up: #2385.

**Chose to treat a stale preparation digest as not current.** An image
admissible at the tip whose `Environment.PreparationDigest` differs from
this daemon's `projectimage.PreparationDigest()` is rebuilt, even at its own
build commit. This is the reading of acceptance 1: a daemon upgrade that
changes the preparation command must end in a rebuilt image. An image with
no environment evidence is not current either.

**Chose an advisory host-fault item over a blocking one.** A host fault
(room, builder, or measurement failure, a stuck build, a missing build
commit, no `-base-build-ref`) converges the `image_upkeep_host_fault` doctor
item with the advisory posture. Unattended admission skips advisory items,
so a fault in upkeep never stops work the current image still serves. The
next pass that reaches a verdict resolves it.

**Chose host fault over a verdict for a build that exceeds its bound.** The
publication lane treats a stuck build as `build_or_proof_failed`, because
the candidate's own verification commands run in the proofs. Upkeep builds
the merged tip, and a repair verdict will submit a maintenance task (#2336).
A build that never finishes carries no evidence the repository caused it,
so it records a host fault instead.

**Chose to record an invalid build request as a host fault, unlike the
publication lane.** Every `ErrInvalidRequest` names the image record or
the host's build inputs, never the tip's tree. A `repair` would send a
maintenance task to fix a repository that did not cause the failure. The
publication lane still refuses the run, because there the refusal holds
only that run. An image record that disagrees with its own recipe, or a
rebuilt record that disagrees with the build, is a host fault for the
same reason; logging it alone would leave no item a person sees.

**Chose to let a scheduled pass reuse a settled refusal, and the start
pass measure again.** A repair refusal from a failed build would
otherwise rebuild the same tip, for up to an hour under the shared gate,
on every doctor fire, and a build error whose text varies would record a
new refusal each time. `UpkeepTipSHA` cannot stop it, because the tip
observation it lives on is only written once the tip watcher (#2334)
lands. The refusal record carries no preparation digest, so the start
pass, the trigger a daemon upgrade runs, measures again.

**Chose to record a failed measurement as a host fault.** When the gate
cannot compare the image with the tip (for example the image's build commit
is not in the fetched branch), the pass records `host_fault` rather than
failing silently or guessing a class. Reusing a recorded image is limited
to images whose build commit the checkout holds, for the same reason.

**Chose to write `UpkeepTipSHA` only for a settled pass.** A pass that ends
current, advanced, or refused for a non-host reason marks the tip handled.
A host fault leaves it unmarked, so the next trigger tries the same tip
again.

**Chose the manual-submission config as the registry-set source.** It is
the only project policy the daemon loads today. A production driver daemon
refuses to start when two projects declare different registry sets,
because one repository has one pointer. No set gives an empty policy, built
as a `ResolvedPolicy` value directly because `NewResolvedPolicy` rejects an
empty key set; the gate then refuses under `no_registry_set`.

**Chose a tolerant scheduler handler.** A non-transient handler error stops
the scheduler, which stops the daemon. A failed upkeep pass logs and
reports `observe_failed` instead, and the doctor's next cadence retries.

**Chose one FIFO build gate where publication never waits.** Upkeep queues
for the gate; a publication rebuild that finds it busy returns a retryable
error, so the lane's task stays queued under `publication_environment` and
its next attempt reuses the image upkeep built when it serves its commit.

**Chose one worker that keeps only the newest pending tip for the
tip-advance trigger.** The tip watcher (#2334, landed first) calls the
entry point inside its reconcile pass and marks the tip handled when the
call returns, so the call hands the tip off and returns. A tip still
waiting when a newer one arrives is dropped, because upkeep keeps the image
current at the tip, not at every commit on the way. The start pass runs on
the same worker, so it and a tip pass never fetch and measure side by side.
A handed-off tip the daemon stops before running is covered by the next
start pass.

## Refute-First Findings

A fresh-context reviewer tried to refute the diff (returned-object trust
boundary, scratch deletion). Outcomes:

- **Confirmed, fixed:** a scheduled pass rebuilt a tip that already failed
  as `repair` on every fire; an `ErrInvalidRequest` was classed `repair`;
  a recipe or rebuilt record that disagrees with the image only logged.
  The decisions above record each fix.
- **Disproved by a check:** a wrong image advanced onto the pointer (the
  rebuilt record is read back by ID and re-gated, and `advance` re-reads
  the pointer in its transaction); scratch cleanup reaching outside the
  pass (it removes only the directory `MkdirTemp` made); a lost wakeup or
  a gate left held on cancellation (a cancelled waiter removes itself, or
  passes on a gate handed to it as it ended; the gate test covers the
  first); a raw-string registry-set comparison rejecting an equal set
  (policy values must be canonical, `domain.RegistrySetFromPolicy`).
- **Allowed by decision:** the publication gate now reads the base before
  reporting `unsupported_npm_input` or `no_registry_set`, so an unreadable
  base returns a retryable error instead of that refusal. The run's base
  was already read at admission, and 40,000 generated lockfiles agreed on
  every first refusal. Scratch a crash leaves behind is not swept, as the
  publication rebuild's is not (Follow-up: #2387). The advisory item's
  run-doctor action does not rerun upkeep; the next scheduled pass
  resolves it.

## Revisit When

- Conformance can prove an image other than the configured one: Part C's
  selection then follows the pointer.
- Project policy moves out of the manual-submission config, or adopted
  registry hosts land (#2379): the registry-set source moves with it.
- The daemon serves more than one repository: `RunAll` needs a store read
  that lists pointers.
