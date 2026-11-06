# Reviewers Read The Task's Attachments From A Proved Volume (#2589)

## Decision

- **Chose to deliver only the task's attachments to a reviewer, not the
  discussion's.** A task attachment is part of what the operator asked for, so
  a reviewer judging the change needs it. A discussion attachment is the
  writer's conversation input, and the reviewer runs with fresh context by
  design (plan §7). The engine reads them through the same `taskInputFiles`
  helper writers use, so a reviewer sees each file at the writer's path
  (`task/<name>`), in submitted order.
- **Chose one lifecycle for both review providers.** The input volume lives in
  `CodexReviewLifecycle`, which the Codex and Claude providers share. It runs
  the writer handoff's input seeder and observer scripts on the review's
  pinned observer image, so both writer and reviewer accept the same
  nonce-bound listing proof (`inputListingDigest`). The volume is proved once
  after seeding and again before the reviewer starts, with a distinct observer
  each time, like the credential snapshot.
- **Chose to bind the files into the request's authority digest, with no
  version bump.** `ReviewRequest.Inputs` is `omitempty` in
  `AuthorityDigest`, so a request without attachments hashes exactly as
  before, and a persisted request still re-gates. A request with attachments
  binds their names, digests, media types, and sizes, so a re-gate fails if
  the list changes. Task attachments are immutable once stored
  (`PutTaskAttachments` refuses a rewrite), so the digest stays stable across
  rebuilds.
- **Chose to rerun an in-flight review persisted before this change, and let
  a finished one stand.** Such a request carries no inputs, so a re-gate
  against a request that has them would fail on every pass and wedge the
  task. The source's supersession check now treats "no inputs persisted,
  inputs expected" like a superseded instruction binding. A review still in
  flight is torn down and rerun with the files, since it has no verdict to
  lose. A finished clean review is re-gated under the authority it was
  persisted with, the current request without inputs, and stands. That
  matches the findings path, where adjudication already lets such a record
  stand. The rejected option, rerunning a finished review, was tried first.
  A second refute-first pass showed it leaves a round between a
  remediation's request and its review, which `reconcileRemediationReview`
  refuses on every pass, and that it pushes a clean final round past the
  hard limit into escalation. A task without attachments never consults the
  check, so its path is unchanged. The cost: a task in flight across the
  upgrade may pass on a review that never saw its files, which is what it
  would have done before this change.
- **Kept the prompt identity unchanged.** Both providers still build the
  prompt from the request alone. A request without inputs gets byte-identical
  prompt text, and with inputs a section is appended that lists each file.
  When the listing would push the prompt past its cap, the section instead
  states the count, total size, and directory, and tells the reviewer to list
  it. Every file is still delivered, and the cap never truncates the files.
- **Kept the review topology version unchanged.** Every new binding and intent
  field is `omitempty`, so a review without inputs writes the shape older code
  wrote. Older code never delivered inputs, so no stored row carries them
  under an older version. Recovery adds the input names only when the intent
  journals an input volume, and never on a legacy (pre-snapshot) name set.
- **Rejected reusing the writer handoff's `prepareStagedVolume` directly.** It
  is bound to the handoff's `runState` claims and journal. The review path
  journals each resource in its own intent before creating it, and recovery
  reaps those resources by that record, so it needs its own create, seed, and
  observe steps. Only the scripts and the proof are shared.

## Failure Classes

A blob that doesn't match its admitted digest or size is a contradiction. A
missing blob or a host staging error is transient. A launch with inputs but
no blob source is a configuration error. If the volume is replaced before
start, cleanup refuses to delete it, because it is no longer provably
ward-owned.

## Checks

Live read checks: not run. Proving that each reviewer CLI can open a
root-owned PNG under `/var/lib/freeside/inputs` needs a held production rig
(`FREESIDE_RIG_ACQUISITION`), and this session had none to use. The writer's
check (`TestPinnedClaudeInputReadLive`, #715) proves the Read path for the
Claude CLI at the same mount. The review command's narrower tool list is still
unproven.

Refute-first: a fresh-context reviewer tried to refute the change before
push. It found one blocking defect, the upgrade wedge above, now fixed with
tests at both layers. A second reviewer then tried to refute the first fix
(rerun the finished review) and found the remediation wedge and hard-limit
escalation that led to the current rule. One gap remains: an in-flight
pre-change request at the hard-limit round still escalates once it is
superseded, as one superseded by new instructions already does. It also found a misplaced doc comment (fixed), missing
teardown coverage with inputs (added), and that the over-cap summary is not
itself held to the cap. That last one is declined: the summary is about 400
bytes against a 120 KiB cap, and a prompt with no room for it fails the
launch's cap check as configuration rather than leaving the reviewer unaware
of the files. Claims it tried and could not make hold:

- A rewritten intent cannot redirect a delete: every input name is
  re-derived from the run id, and the input volume and seeder must carry the
  intent owner.
- Recovery reaps the seeder, observer, volume, and host stage, each under
  its ownership proof.
- The input mount is the reviewer's last mount, read-only, and checked
  before start and again at reconstruction.
- Both review providers receive the blob source in production composition.
- A review without attachments keeps its request digest, intent, binding,
  and prompt byte-identical.
- The rig name patterns admit the new resource names.
- Superseding a finished request keeps its outcome and evidence: the
  teardown returns early once the outcome is ready.
- Supersession never fires for a task without attachments or for a request
  that already carried inputs, so it cannot loop.

A stage-wipe failure can mask a blob mismatch's class (the joined error then
reads as operational). The masking needs a host `RemoveAll` failure in the
same launch, and the retry meets the mismatch again, so it is left as is.

Revisit when a reviewer should see discussion attachments, when review inputs
need a different mount than writers, or when the review topology version next
changes for another reason, which would let the input fields become required.

Scope: `daemon/internal/exec`, `daemon/internal/engine`, `daemon/internal/ward`,
`daemon/internal/daemonlock`, `docs/plan.md`, `devlog/`.
