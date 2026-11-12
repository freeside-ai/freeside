# Rig Names Leave the Manifest When Their Owner Proves Them Gone

Issue #833. Tier A: `internal/daemonlock/rig.go`, `internal/ward/`, and
`internal/daemoncompose/claude_driver.go` decide which runtime objects rig
cleanup, stale recovery, and the deputy's release proof can enumerate.

Chose owner-driven release over a smaller encoding or a bounded hold,
because only release lets one deputy hold serve any number of tasks. The
planner chose the approach in the issue; this note records the rule as built
and two findings that shaped it.

## The Corrected Premise

The issue was filed against a short acceptance campaign. Since #1583 the
deputy is the only real-work path, and it holds one rig lease for its whole
serving life. The manifest only grew (about 2.9 KB per task with one
review), so `encodeRigManifest` refused every bind after about 20 tasks and
the deputy stopped running work. That is a hard ceiling, not an edge.

## Decision

- **The rule.** A name leaves the rig only when its owner has finished with
  it after its final teardown, no later path creates an object under it
  without binding it again, and the runtime shows no object under it at
  release time. Releasing late only keeps a name; releasing early leaves an
  object the rig cannot enumerate.
- **The release re-lists the runtime.** `ward.ReleaseAbsentRuntimeResources`
  lists containers, volumes, and networks and releases only names with no
  object; a listing failure releases nothing. Chose this over trusting each
  owner's teardown because handoff teardown succeeds while a same-name object
  it judged foreign remains, so a closed journal record does not prove every
  object under a name absent. This is not the rejected bind-time prune: that
  pruned on absence alone, and absent is not finished.
- **Release points in this unit.** The pre-job container after
  `Backend.PreJob` succeeds; a writer handoff namespace after its journal
  record closes (a closed record is never reopened, since `Begin` refuses an
  existing run ID, and `Recover` returns early on it); a review namespace
  after `cleanupCodexReview` closes its intent (the stored outcome fences any
  relaunch of that run ID, and every cleanup path rebinds before acting).
- **Pre-launch review recovery is not a release point.** `codexReview`
  recovers a prior open intent and then continues the launch under the same
  names, so a release there would unbind names the launch is about to create
  under. Found while tracing the plan's review step.
- **A failed release changes no outcome.** It is logged, and the names stay
  bound.

## The Authority Log

- **Grow-only or shrink-only per record.** `readRigLockMetadataLog` accepted
  only supersets. It now accepts a record that only adds names (bind) or
  only drops names (unbind), and refuses one that does both, since no
  operation the holder performs produces it. Signature, sequence, identity,
  canonical form, and the mirror-subset checks are unchanged.
- **Unbind writes mirrors before authority,** the reverse of bind. Every
  reader requires each mirror to be a subset of the authority, so an unbind
  interrupted between the two leaves smaller mirrors under the larger
  authority, and `ReadRigManifest` and stale recovery return the larger,
  safe set. Authority first would leave mirrors naming dropped names, which
  every reader refuses.
- **A no-op amendment writes nothing.** The pre-job hook rebinds the same
  names on every start retry, and the log is read whole on each amendment.

## Rejected

- **Recording run-ID namespaces instead of expanded names.** About 13
  times smaller, but the list still only grows, so the ceiling moves.
- **A documented per-hold cycle bound.** A deputy that serves indefinitely
  still fails after a fixed number of tasks.
- **Pruning at bind time any name whose object is absent.** A name bound
  but not yet created would be pruned, and its owner would then create an
  object the rig cannot enumerate.

## Split

The per-task terms ship here. The one-shot helpers (credential observer,
lineup probe, Claude auth enrollment seeder) and conformance's non-handoff
names add about 70 bytes per credential read and about 1.5 KB per
conformance rerun, so they still accumulate, now over hundreds of tasks
rather than about 20. They moved to a follow-up unit to keep this one within
the size budget. Follow-up: #3094.

## Refute-First Record

One fresh-context reviewer got only the diff and the intended outcome and
tried to break "no released name can later have an object the rig cannot
enumerate." It found no early release.

- **Confirmed, deferred: the authority log now grows without bound.** Before
  this change the 64 KiB manifest capped the log below about 100 records.
  Now a task appends about four, and each amendment and each reader verifies
  the whole log. Measured on a loaded machine: 87 ms per amendment at 101
  records, 204 ms at 801, about 0.15 ms per record. Only each record is
  capped, so this is a slowdown, not a ceiling. Compaction is this unit's
  non-goal. Follow-up: #3096.
- **Confirmed, deferred: failure and crash paths leak names.** The pre-job
  hook binds the handoff namespace before the stage driver's input load and
  regate, so a refusal there never opens a record. A handoff refused before
  `Journal.Begin`, a crash or failed release after close, and a review
  failure with no stored outcome also never release. Each leak is safe
  (late) but costs about 2 KB of manifest. Follow-up: #3095.
- **Confirmed, fixed: a null network list read as empty.** The container and
  volume decoders refuse `null`; the network decoder read it as an empty
  list, so release would have unbound every network name asked about. It now
  refuses `null` too.
- **Confirmed, fixed: missing keep-bound tests.** Recover and review cleanup
  now have tests that a failed close releases nothing, and a mutation that
  releases unconditionally fails them.
- **Allowed: a timed-out create that finishes after release.** If a runtime
  create times out and completes after teardown's absence proof and the
  release listing, its object lands under a released name. Teardown's loss
  verdict already accepts this window; this change drops only the final rig
  cleanup's second chance at the object.
- **Disproved by tracing:** an interrupted unbind leaves every mirror a
  subset of the authority, so no reader wedges or trusts the smaller set; a
  splice of signed records needs two at one sequence, which the gated append
  never writes; bind and unbind serialize under the gate's `flock`; the
  handoff release fires only after a successful close and a closed run ID
  is never reopened; the review release fires only after an outcome is
  stored, which fences any relaunch; the pre-job release runs under the
  driver's mutex after the duplicate-start check; and runtime names match
  exactly.

Revisit when a new binder appears (each needs its own release point), when
two owners could share a name, or when the deputy stops holding one rig for
its serving life.
