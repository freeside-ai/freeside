# The Section 5.16 Durable Scheduler (#442)

Work unit: issue #442, the head of Wave 3's serialized contract track.
Spec: plan §5.16 verbatim (the issue's Acceptance quotes it). This note
records the design decisions inside that spec's degrees of freedom and
the alternatives they rejected.

## Decisions

- **Synced state is the schedule definition, not the firing clock.** The
  `schedules` table is a synced aggregate (entity_version /
  as_of_revision, Put on WriteTx), carrying kind, subject binding,
  generation, status, one-shot fire instant, recurring cadence, expiry,
  and terminal resolution. The rolling next-nominal-fire instant for
  recurring kinds and the occurrence ledger are daemon-internal rows
  (the 0014 rule), like the outbox. Rejected: syncing the whole firing
  state, because every Write bumps the §5.14 revision, and a 30-second
  janitor cadence would invalidate every client cache twice a minute to
  report that nothing meaningful changed. "Durable, queryable, and
  synced" reads as: the schedule's existence, binding, and outcome are
  client-visible; its tick bookkeeping is the daemon's.
- **Occurrences are claim-then-consume, outbox-style.** A due schedule
  first commits a pending occurrence row (identity `(schedule_id,
  generation, nominal_fire_at)`, insert-or-ignore) and, for recurring
  kinds, advances the internal timer in the same internal transaction;
  the handler's durable outcome, the occurrence's consumed mark, and any
  schedule state change then commit in one transaction (Write when the
  outcome touches synced state, WriteInternal when it does not). A crash
  between the two leaves the pending occurrence durably redeliverable,
  which is the §5.16 redelivery bullet; the kill test drives exactly
  that window.
- **Handlers observe outside the transaction, commit inside it.** A
  handler returns a consumption (outcome code plus an optional commit
  callback); long work (a janitor cycle, a doctor pass, a GitHub base
  read) runs before the consuming transaction opens, because SQLite
  holds one write lock and the doctor's own converge writes items in
  nested transactions. Handlers whose products are independently
  idempotent (doctor converge, janitor coverage) rely on at-least-once
  redelivery plus idempotence; handlers with synced products (the
  base-freshness fact) commit them in the consuming transaction.
- **One-shot deadline outcomes live on the schedule, not the item.** A
  fired `pr_checks_deadline` / `review_wait_threshold` records its
  terminal resolution (`deadline_elapsed` or `subject_concluded`) on the
  synced schedule row. Rejected: bumping the attention item's priority
  or version on fire, because a version bump invalidates prepared
  commands (an operator mid-approval would be reset by a nudge), and
  §5.16's acceptance is about the timer mechanics, not a notification
  product surface. Revisit when the §7 review stage (#427) consumes the
  review-wait threshold for real.
- **The base-advance watch writes the item fact only on material
  change.** First observation writes the fact; later fires update it
  only when the observed base tip differs (item version bumps then,
  which correctly invalidates commands prepared against a stale base
  claim). An unchanged observation is recorded on the occurrence, not
  the item, so a 15-minute watch does not churn item versions.
- **The janitor migration keeps the coverage lifecycle inside the
  janitor.** The scheduler owns cadence only: a scheduled pass runs
  withdraw → cycle → publish under the janitor's own cycle mutex
  (`RunScheduledPass`), so coverage semantics (`ActiveFor`,
  `AwaitAllowsRepository`, `WithStableCoverage`) are untouched. The
  daemon's startup gate becomes a synchronous first pass during
  composition (replacing the start-loop-and-poll-ActiveFor dance);
  scheduler-loop failure remains daemon-fatal, preserving "a stopped
  janitor stops the daemon". The doctor keeps its synchronous startup
  pass (§10); the schedule owns only the 24-hour repetition.
- **The onboarding poll arms at BeginInstallation and survives the CLI.**
  `freesided onboard` arms a durable `installation_poll` schedule bound
  to the pending envelope (registration, active epoch, durable intent
  revision; expiry = the envelope's ExpiresAt) in the same session that
  records the intent, and `--resume` drives the scheduler over that
  schedule instead of an ad-hoc 100ms ticker. Fire-time validation's
  activation-state check is the §5.9 epoch/revision currency check.
  The handler only observes readiness and resolves the schedule; it
  never promotes: promotion extends trust, and §5.16 fixes that firing
  never extends or preserves authority. The daemon also registers the
  kind (closed-union coverage) with the same observe-only handler, so
  an abandoned intent's expiry is recorded durably even if the operator
  never resumes. The trusted-repository wait (repo already trusted,
  waiting for first janitor coverage) stays a process-local in-memory
  poll: it is a startup-coverage wait with no durable intent, i.e. a
  stateless heartbeat by §5.16's own carve-out.
- **Mode eligibility is a per-kind dispatch, currently all-modes.**
  Trusted-config jobs run in every operating mode per §5.16. The four
  workload kinds also declare both modes for 1B: the watches are
  read-only observation over durable subjects that can exist in either
  mode, and onboarding is operator-attended by nature. The golden
  eligibility matrix pins the mechanism and today's values; a consumer
  that demands a narrower mode changes the dispatch, the matrix, and
  this note.
- **Schedule IDs are deterministic, not random.** `schedule-doctor`,
  `schedule-janitor`, `schedule-installation-poll-<registration>`,
  `schedule-<kind>-<item>`: the domain package generates no IDs
  (doc.go), and a deterministic identity makes re-arming a current-state
  aggregate transition (generation bump) rather than a row leak.

- **Crash-window coverage is restart-equivalent, not process-SIGKILL.**
  The transactional bullet's kill/redelivery acceptance is pinned by two
  tests that together cover the whole window: a store-level rollback test
  (consumption + outcome in one Write; a failed transaction leaves the
  occurrence pending and the schedule armed) and a scheduler-level
  redelivery test that drives the same durable store with a second
  scheduler instance, which is exactly what a killed-and-restarted
  process is over SQLite state. Rejected: extending the permanent
  SIGKILL harness with scheduler checkpoints, because the fake daemon's
  watch deadlines are minutes-scale and staging a due schedule across a
  process kill buys no atomicity evidence the two tests don't already
  give. Revisit when a scheduler consumer commits external effects at
  fire time (the harness's checkpoint pattern applies then).

## Verification Findings

- `publish.Reconciler.ReconcileRef` / `ReconcilePull` have no production
  callers today: the §5.11 continuous reconciler is not yet wired, so
  the base-advance watch takes a `BaseTipObserver` seam (real: a
  conditional ref read via the publication transport; fake lane: the
  fake publication state) rather than reading a reconciler-maintained
  fact that does not exist.
- There is no checks/review observation anywhere in `daemon/` yet; the
  two deadline kinds therefore terminate on elapsed-versus-concluded
  evidence from the item subject only. Their richer consumers arrive
  with #427.

## Revisit When

- #427 lands: the review-wait threshold's real consumer may want the
  deadline outcome surfaced as attention, and the checks deadline may
  bind to observed check runs instead of item-open state.
- Proposed watches land (post-1B): expiry becomes mandatory for
  proposable kinds and the §5.13 caps/coalescing attach.
- A consumer demands a mode-restricted kind: update the eligibility
  dispatch and matrix together.
