# Identity Limit Rollout Replaces the Overlap Experiment

Date: 2026-09-28. Tracking: #730. Overturns the evidence bar in
`devlog/2026-08-12-0657-identity-parallelism.md`. That note's admission gate,
active-execution count, and typed `identity_parallelism` hold stand.

## Decisions

**The owner dropped the provider-side overlap proof as the bar for raising
`max_parallel_executions` above 1.** The 2026-08-12 note allowed a raise
only after a live experiment proved overlap from provider or runtime
timestamps. The owner overturned that on 2026-09-28. Their routine
concurrent use of several Claude Code and Codex sessions on the same
subscriptions already shows that one subscription tolerates parallel
sessions. So the experiment would spend inference and credential risk to
show what daily use already shows. The changed assumption is where the
evidence comes from, not how strong it must be.

**A rollout check replaces the experiment.** Raise the Claude writer
identity's limit, use Freeside normally, and watch the daemon log and the
paired clients for auth, rate-limit, or credential failures. On any such
failure, record the last limit that ran cleanly, or 1. The owner's target is
8, possibly with 4 first. The owner records any production value above 1.
This change adds only the means to record it.

**Rejected building the provider-overlap probe first.** A probe was built on
this branch before the rescope. It read the Claude CLI's persisted
stream-json transcript, whose assistant frames carry an in-container
`timestamp` and the provider `request_id`. The reasoning held up, but the
evidence no longer gates anything, so shipping it would add an unused live
check. If the bar returns, start from that transcript source. The Claude
session JSONL is deleted with the run's continuity volume, and the container
timestamps are never recorded.

**Chose a `freesided set-identity-limit` command over a harness environment
variable.** A real run records the Claude writer identity in only one place:
the real-run harness behind `scripts/run-real-work.sh`
(`TestRealWorkItemCompletesProductionPipeline`), hardcoded at 1. No
`freesided` command recorded one, and Codex enrollment hardcodes its own
identity at 1. Carrying the limit as a harness input would force every
resume and final-verification pass to repeat the same value or fail the
harness's exact-match check. So the limit is operator state in the store
instead. The command changes only that field through the store's existing
transition rule and forward-revision ordering. It forwards to a running
daemon over the control socket, and the admission gate reads the current
revision on each admission. The harness now keeps a recorded limit on
relaunch rather than resetting it to 1, and a new identity still starts
at 1.

**Limited the command to Claude identities.** The Codex reviewer produces no
execution-admission records yet, so its limit gates nothing. Codex limits
are also outside this rescope. Widen the provider check when a Codex
execution driver consumes the gate.

**Found that the limit alone cannot give parallel writer runs.** Every
Claude writer handoff claims the identity's exclusive auth-store mutation
lease for the whole invocation, although its setup-token mount is read-only.
#383 kept that lease to hold the identity unavailable for the whole
invocation. Ward refuses a second live leased handoff for the identity, and
the engine does not treat that refusal as a hold. So above 1, a second
concurrent writer fails its stage rather than waiting. The owner's
interactive parallel use does not cover this, because the bar it replaces
was about the provider and this limit is Freeside's own serialization.
Changing the lease model is contract work, deferred to #1585. This change
ships only the operator path. Until #1585 lands, a raise yields failed
stages, not parallel runs, so keep every limit at 1. The rollout check
starts only after #1585 lands; before that, a failed stage at a raised limit
is this lease, not the subscription.

## Out of Scope, Observed

- `budgets.run_wip_cap` (`daemon/internal/intake/policy.go`) does not cause
  "Queued" tasks. A task submitted from a client or `freesided submit` starts
  through `RecordTaskStart` with no cap check
  (`daemon/internal/engine/production_workflow.go`). The cap is checked only
  on a label-intake `auto_start` and on re-admitting a task that is not in
  progress, and there it refuses the start (`wip_cap_exhausted`) rather than
  queuing it.

## Revisit When

- #1585 lands: the rollout check can start.
- The rollout check sees auth, rate-limit, lockout, or credential-state
  failures at a raised limit. Lower it first, then decide whether a measured
  bar is needed again.
- The subscription plan, the vendor CLI, the credential-delivery topology, or
  the execution driver changes in a way the owner's interactive use no longer
  reflects.
- A Codex execution driver starts producing admission records. Its limit
  needs its own decision.
