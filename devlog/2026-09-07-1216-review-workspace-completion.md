# Review Workspace Access and Completion

Chose review-only ownership alignment and a deterministic check inside the
Codex sandbox over broader directory permissions. This repairs #1212 without
changing the shared findings schema, ReviewSource interface, or review policy.

## Reproduced Cause

The incident's approved project image is
`127.0.0.1:5518/freeside-project-freeasinbird-gh-imgup@sha256:961254b6e143f6bcc48afa528c6b76883af602021e27e61262d31fdaa62bf70b`.
It contains Codex 0.146.0, despite the repository's later 0.147.0 image recipe.
Tests use the incident image rather than assuming the recipe identifies the
deployed binary. Apple container and its server both report 1.1.0.

`PrepareCodexReviewWorkspace` stages a private host checkout and copies it with
`cp -a`. The resulting volume root is mode 0700, uid 501, gid 20. An outer
container process running as root can inspect it. Codex's read-only sandbox
creates a user namespace mapping only uid/gid 0. The host owner becomes
unmapped uid/gid 65534, and sandbox root cannot traverse the directory. The
CLI falls back to another command cwd; relative Git commands then report
that they are not in a repository. A two-commit fixture reproduces this
through the production seeder and the pinned CLI's sandbox launcher.

The authenticated incident transcript ends in `turn.completed`, not
`turn.failed`. Rejecting only terminal failure events cannot repair this
incident. Its retained digest establishes content identity, not completion.

## Chosen Repair

The Codex review seeder changes ownership to container uid/gid 0 before the
separate observer runs. Directory modes, regular-file permissions and bytes
stay unchanged. Claude and ordinary stage seeders retain their existing
commands. No sandbox is disabled and no permissions are broadened.

The review wrapper uses `codex sandbox` with an explicit root-read-only,
network-restricted profile before invoking the model. The pinned CLI source
accepts an explicit `SandboxState`, which avoids named profiles inheriting
permissions from ambient configuration. The source
routes that entry point through the same Linux sandbox launcher used by
`exec -s read-only`; the incident transcript exposes the matching bwrap
profile and uid map. The preflight checks the actual cwd, repository root,
HEAD, both bound commit objects, and the exact base-to-head diff. A failed
preflight exits with EX_CONFIG and uses the existing configuration failure
and recovery path. Its status and diagnostics remain in the collected bytes.

The source also rejects an unfinished or failed terminal turn even when the
CLI reports exit 0 and an empty findings array. A later completed turn may
supersede a recovered failure. Nested tool-command failures are not terminal
review failures. The replay gate rejects known failure even if stored content
hashes are internally consistent.

Authenticated bounded collection bytes are retained for both successful and
failed normalization. A present structured result is collected on nonzero
exit too. Missing or oversized output remains a failure; available bounded
bytes may be retained, but missing transcripts are never invented and
oversized bytes never enter the outcome.

Codex configuration and result-evidence versions advance. Old clean results
cannot satisfy the new evidence gate, and the existing configuration
approval/adoption path governs the new command. Claude keeps its previous
configuration and intent identities.

The approved production-path rerun confirmed both collection dispositions.
The missing-base case retained its failed preflight and stayed failed after
restart. The successful zero-finding case retained a completed turn with a
successful Git command inspecting the exact bound README diff and both
revisions. Independent digest recomputation authenticated both collections.
This establishes actual inspection of this fixture, with the evidence limits
below; it does not establish universal review thoroughness.

## Evidence Limits and Rejected Options

- Rejected mode 0755, recursive permission broadening, added privileges and
  sandbox disabling. Ownership alignment restores the intended private
  reader without those changes.
- Rejected outer-container preflight alone: it succeeds in the reproduced
  broken setup. The sandbox preflight is the relevant access check.
- Rejected an incomplete-review boolean or parsing explanatory prose. Neither
  proves inspection, and this repair needs no new shared review vocabulary.
- Empty findings remain legitimate. Successful Git commands establish access
  to the bound repository and diff; neither those commands nor authenticated
  transcript bytes establish that an LLM reviewed every line.

## Refute-First Findings

- Confirmed and fixed during review: blanket archive-error classification
  would turn transient read failures into durable contradictions and clean
  up the retryable container. Explicit bounds, type, duplicate and path
  violations now carry a private marker; tar and filesystem read failures
  stay operational. A truncated-export regression proves that inspection
  retains the stopped container and succeeds after a healthy re-export.
- Confirmed and fixed: populating the new base binding for Claude would change
  existing pre-start intent digests. The source supplies it only for Codex;
  the Claude recovery regression checks the unchanged digest.
- Removed an ambient-config assumption: the preflight supplies its complete
  effective sandbox state instead of a named TOML profile. Candidate-only
  configuration cannot grant trust, but a derived approved image could carry
  trusted user or system configuration that merges extra profile fields.
- Disproved by the live sandbox matrix: the repaired private root remains
  mode 0700, tracked file mode 0600 remains intact, and wrong cwd, missing
  workspace, absent commits and a wrong bound HEAD fail the production check.
- Disproved by collection/replay fixtures: exit-zero terminal failure cannot
  become a clean result, retained failure bytes survive cleanup and restart,
  and altered bytes invalidate their digest. A later completed turn after a
  recovered tool failure still admits a clean result.

Revisit when the approved image changes its process identity, the CLI changes
its sandbox launcher or terminal-event protocol, or reviewers gain a stronger
provider-owned completion contract. Reprove the actual approved image instead
of substituting its build recipe or an outer-shell observation.
