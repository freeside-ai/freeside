# Resume a Run Interrupted Before Publication

Chose a fourth retained checkpoint state, `pre-publication`, over teaching
`recover` to relaunch a run or letting the harness skip the retained verifier
when a run has no ready item (#2040). The Wave 8 exit run for
freeasinbird/gh-imgup#117 stopped while its implementation run waited on a
`review_dispute` card; `--resume-session` then refused, because the retained
verifier anchors on the published ready item's PR binding and a run that
never published has none.

## The Trust Boundary

The checkpoint's `state` decides whether the harness and
`real-work-retained.py remote` check a remote PR, so a damaged or published
history must never read as `pre-publication`. The verifier reports it only
when all of these hold:

- Retained mode. Ordinary `verify` still refuses the run.
- The binding read fails with not-found for the run's original ready ID.
- The run's publication slot (`publish-production-<run>`) holds the run's
  own reservation: `publish.CheckInvocationAvailable` refuses a committed
  publisher intent or a foreign row, and a direct read then requires the
  reservation row, since availability also passes a free slot. The run
  reserves the slot in the transaction that creates it and holds it until
  the publisher commits, so this is positive proof that publication never
  began. A successor chain implies a committed original, so it needs no
  separate check.
- No `ready_for_final_review` item names the run, in any status. A ready
  card whose binding is missing stays a refusal, which the existing
  dismissed-history damage tests and a new open-card test pin.
- The run has no work-unit completion.

The live verifier still requires the submitted implementation invocation's
admission record and its repository and base before it reads the checkpoint,
and it reads the pre-publication admission through the current-policy gate
(`GetExecutionAdmission`), since the run is live work, not history.

The `remote` helper skips `gh` only for this state, and `check_remote` still
refuses it if handed one. The harness grants nothing on it: it supervises
the run, and acceptance still comes only from the ordinary final
verification, which requires an open ready item and its remote PR.

## Refute-First Findings

- **Confirmed, fixed: a published run could read as pre-publication.** The
  first rule trusted the absence of a ready item. A `publish_blocked` hold
  after publication records a held PR binding and no ready item
  (`RecordHeldItemPRBinding`), so it passed. The publication-slot check
  replaced that absence test; a fixture deleting the binding and the ready
  item pins it, and fails without the check.
- **Confirmed, fixed in review: a free slot passed as proof.** Codex
  (PR #2051) noted that `CheckInvocationAvailable` accepts an empty slot,
  so damage that removed the committed intent along with the binding and
  ready item would read as pre-publication and skip the remote check. The
  reservation row is now required; a fixture deleting it pins the refusal.
- **Confirmed, fixed: the admission skipped the current-policy re-gate.**
  `GetExecutionAdmissionRecord` is for terminal history only.
- **Disproved: the harness's decoded state can be steered.** Only the
  verifier writes the checkpoint, to a `mktemp` path under umask 077, and
  an empty, malformed, or unknown state refuses (fixture cases).
- **Disproved: ready, retained, and completed paths change.** The new branch
  runs only on a not-found binding for the original ready ID; the existing
  fixtures for all three pass.
- **Accepted gap: the live verifier's pre-publication branch has no
  hermetic test.** It needs a live run; the reader it depends on is tested.

## Rejected Options

- **Make `recover` relaunch the run.** `recover` is the release path for a
  dead holder; giving it a launch would mix release with execution and
  bypass the resume's composition and build checks.
- **Skip the retained verifier when there is no ready item.** That would
  trust absence without proving it, so a ready card that lost its binding
  would resume as if nothing had published.
- **Resume at `waiting_for_specification_approval` too.** No implementation
  admission exists there, and every retained check anchors on it. Set as a
  boundary during planning (owner may veto); it needs its own anchor.

## Assumption

The restarted daemon continues the run from its open attention item on its
own; the harness only observes. If a rehearsal shows otherwise, that is a
daemon defect for its own issue.

Revisit when an exit run is interrupted at the specification gate, or when
#1938 retires `run-real-work.sh`.
