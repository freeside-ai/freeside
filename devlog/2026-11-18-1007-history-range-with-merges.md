# Pack the History Between a Merged-In Commit and the Base

Work unit: #3181. Found reviewing #3121. Amends
`devlog/2026-11-12-1730-verify-command-history.md`.

A history command's shallow `.git` cut the parents of every boundary of
base..head. When a candidate merged history older than its base, those
older boundaries were parentless commits the base couldn't reach, so
`base..head` listed them as the candidate's own and `merge-base
--is-ancestor` couldn't place them in the base. `scripts/check-commit-messages.sh`
then checked mainline's title-only `Land ... (#N)` subjects and failed a
candidate `required` passes, and missed a `base-merge` violation CI reports.
The work is Tier A by path (`internal/verify/`).

## Decisions

- **Chose walking down to the octopus merge base over replace refs,
  grafts, or a full copy.** When base..head has boundaries other than the
  base, the verifier walks from the head down to `git merge-base --octopus
  --all` of the base and those boundaries, and packs every commit it meets.
  Each boundary is then reachable from the base inside the written
  repository, so `base..head` and `merge-base --is-ancestor` answer as in a
  full clone. Replace refs or grafts that hang the boundaries off the base
  were rejected: they rewrite the base's history as `git log` shows it and
  add files to the `.git` set. A full-history copy stays rejected for cost,
  as the earlier note decided. A candidate with no boundary but the base
  gets byte-for-byte the `.git` it got before.
- **Refuse when the merged-in commits share no ancestor.** A candidate that
  merges two unrelated histories the base already holds has no octopus
  merge base, and the only walk that keeps it exact is the whole history.
  The verifier refuses it with `ErrHistoryUnavailable` before any command
  runs, like the existing no-merge-base refusal, rather than fall back to a
  full walk.

## Consequences

- **Resolves the earlier note's `base-merge` risk.** A merge parent older
  than the merge base is now reachable from the base, so the check reports
  it.
- **The pack grows with how far back the merged branch forked.** It holds
  every commit between the oldest merged-in commit and the base, each with
  its whole tree, and every history command copies it. Only candidates
  with such merges pay, and those come only from a head someone pushed (a
  `head_changed` re-entry); daemon-built heads are linear.

## Refute-First Findings

A fresh-context reviewer tried to break the change.

- **Disproved by a check: the written range differs from a full clone.**
  The reviewer rebuilt the `.git` with plumbing over random commit graphs
  (1,074 base and head pairs, 281 with older boundaries, two- and
  three-parent merges, two roots, a criss-cross case where the octopus
  merge base has two results). `rev-list base..head` (plain, `--merges`,
  `--no-merges`), `merge-base --all`, `merge-base --is-ancestor` for every
  merge parent, and `fsck --connectivity-only` all matched the full clone,
  and `merge-base --octopus` never exited 0 with no output. The base is
  always walked, never a bottom: bottoms are strict ancestors of it.
- **Disproved by a check: the tests pass vacuously.** Restoring the old
  cut fails the full-clone comparison, and replacing the refusal with a
  full walk fails the refusal case.
- **Confirmed, fixed: the `ErrHistoryUnavailable` doc named only the old
  causes.** It now names the new refusal.
- **Confirmed, deferred to #3369: the engine retries a refusal that
  depends only on the commit graph.** It already did so for the no-merge-
  base refusals; the engine is outside this unit.
- **Allowed by decision: pack size is set by how far back the candidate
  merged** (Consequences), and any `merge-base --octopus` failure reads
  as `ErrHistoryUnavailable`, matching the existing merge-base call.

Revisit when the pack size of merge candidates shows up in verification
time, or when the importer starts writing merge commits.
