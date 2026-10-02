## Why

One pre-#245 keystore record whose metadata carries null owner, owner id,
visibility, and key id failed `Keystore.ListApps`, and because every consumer
reads the complete registration set, that single record fail-closed
`InstallationResolver.Resolve` for every owner and every `InstallationJanitor`
cycle, with an error that named nothing an operator could act on. It blocks the
rest of the Wave 2 chain (#276, #277, #236), which all need a live janitor pass.
Rationale, rejected alternatives, and the refute-first findings are in
`devlog/2026-07-24-1647-keystore-unreadable-record.md`. Refs #271.

## Scope

Scope: `daemon/` (`daemon/internal/publish` only), `devlog/`

## What

- Enumeration keeps refusing to proceed, and now names its subject: the
  per-record failures that make a record unusable return
  `*UnreadableRegistrationError` carrying the record's numeric owner key and
  wrapping both a new `ErrUnreadableRegistration` sentinel and the underlying
  cause. Skipping the record was rejected: owner resolution selects among all
  registrations and refuses more than one match, and the janitor cannot claim
  complete coverage while one registration is invisible, so a partial view is
  the fail-open direction.
- The multi-error unwrap keeps existing cause handling intact: a
  widened-permissions record still matches `ErrCredentialPermissions` and still
  produces the doctor's `bad_keystore_permissions` finding, while an
  identity-only failure produces the new `unreadable_registration` finding. The
  owner ID rides every one of those finding codes, since the specific code tells
  the operator more than collapsing them would; a keystore-wide failure names no
  owner.
- `Keystore.QuarantineApp` is the operator's remedy: it withdraws exactly that
  record from the enumerated set, moving key, metadata, and any `.staging`/`.old`
  swap journals intact to a sibling directory inside the same credentials root
  (preserved, not deleted, and still inside the containment boundary that keeps
  App keys out of checkpoints and workspaces). It declines a readable
  registration, since withdrawing a working one would drop it from janitor
  coverage while its installations stay live, and declines to overwrite a
  distinct earlier withdrawal.
- **Attribution and the remedy are one decision, not two.** Naming an owner
  promises that withdrawal is the remedy, so attribution runs the withdrawal's
  own preflight rather than re-deriving its preconditions. Readability is decided
  in one place for both, the withdrawal's two directory syncs are one helper, and
  a precondition added later is inherited by both sides by construction.
- Crash ordering on the withdrawal: the destination entry is persisted before the
  source removal, so a crash cannot lose the credential, and each journal removal
  is persisted before the next rename, so recovery can never find a promotable
  journal left behind and resurrect a withdrawn record. Journals move before the
  active directory, so every error partial state keeps the active record in
  place and a retry resumes.

## Review Notes

- Seventeen Codex rounds. Fifteen findings accepted and folded into the single
  commit rather than appended; two declined with reasons, one of those filed as
  #280. Dispositions and fixing SHAs are on the resolved threads.
- Seven of those rounds were one class: attribution promising a remedy the
  withdrawal would then refuse. I widened the derivation four times (permissive
  modes, owner access, the directory set, the predicate) before taking the
  structural fix, which is why the final shape has attribution calling
  `withdrawalUnavailable` instead of mirroring its conditions. That history is
  the most useful thing to check the design against.
- Two findings were declined with reasons rather than fixed. One asked withdrawal
  to retry a sync that `recoverSwap` failed to complete; that boundary is shared
  by four callers and the retry already happens on every enumeration, so a branch
  here would imply coverage it does not add. The other asked withdrawal to refuse
  whenever any owner suffix exists in quarantine; that state is indistinguishable
  from the resumed partial withdrawal this PR deliberately supports, so it is
  filed as #280 with both resumption properties written into its acceptance.
- Repair-in-place was rejected rather than overlooked: repairing returns the
  registration to janitor coverage, and the janitor deletes installations absent
  from its authority snapshot, so repairing a registration the operator no longer
  manages is the destructive direction.
- The `.DS_Store` observation from the issue is deliberately unchanged and
  recorded under "Accepted By Decision" in the note.
- The issue's acceptance 2 (the maintainer's local record ending in a valid
  state) is **not** done in this PR, which is why it says `Refs #271` rather than
  a close keyword; see the `Not run:` bullet.

## Verification

- Passed: `go build ./...`, `go test ./...`, `go vet ./...`, `golangci-lint run`
  (0 issues) from `daemon/`, re-run after every fold including the final one.
- Passed: refute-first harness for the destructive path. The enumeration-blocking
  record states (incomplete metadata, identity not binding to the directory key,
  absent key, widened key permissions, invalid recovery journal with no active
  record) are each required to be attributed to their owner and then withdrawable.
  The availability states (both roots and the quarantine destination, each with
  no owner write, no owner search, neither, and group/other bits; a malformed
  target as file and as symlink; an occupied destination) are each required to be
  *not* attributed and refused by the withdrawal — then, with the obstruction
  cleared, attributed and withdrawn, so the gate cannot be satisfied by refusing
  everything. Findings, including what the pass rejected, are in the decision note.
- Passed: withdrawal-state fixtures — resumption of a partial withdrawal,
  completion of a withdrawal whose renames landed but whose sync did not, journals
  and active record withdrawn together with nothing left for recovery to promote,
  refusal to overwrite, and the discarded-stage case that leaves nothing to
  withdraw.
- Checked: reproduced against the maintainer's real keystore through a throwaway
  env-gated test (deleted after the run, not in this diff). Before the change the
  failure was a bare `registration owner is empty`; with it, `ListApps` reports
  `registration for owner 219324892 is unreadable: …` and `errors.As` yields owner
  `219324892` — the org-owned `freeside-ai` record (App 4365457) described on the
  issue.
- Passed: `scripts/merge-result-audit.sh origin/main fix/keystore-incomplete-record daemon/ devlog/`
  against freshly fetched `origin/main` = `7e8bb0074d2bb67dc11d65e7493822ec323d6223`;
  verdict `PASS: 6 changed path(s), all within the allowed paths`. The branch was
  rebased onto that tip after #278 merged, and build/test/vet/lint were re-run
  against it.
- Not run: the crash states the durability barriers protect. The daemon has no
  crash or fsync-failure injection at this layer, and adding an injectable sync
  purely to observe ordering would put test scaffolding in the credential path.
  The barriers are verified by inspection of the call order and recorded as a
  Verification Gap in the decision note, along with the one residual window
  per-rename persistence cannot close.
- Not run: the quarantine of the maintainer's local record (issue acceptance 2).
  The command mutates the real credentials directory and was declined by this
  session's permission policy; it needs the maintainer to run it or to grant the
  permission. It is a move within the credentials root, reversible by moving
  `credentials/github-app.quarantine/219324892` back to
  `credentials/github-app/219324892`.
