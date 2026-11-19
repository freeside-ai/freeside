# Exclude Ignored Symlinks at Import

The owner chose, during the #2382 exit run, to fix this in-run (#3395). It
applies as plan revision 131.

## Decision

Chose to exclude a new symlink that the exact trusted base ignores and
doesn't track, on the same terms as an ignored regular file or nested
repository. Its `non_regular_change` finding drops with it. The other option
was to keep ignored symlinks blocking and expect build output to be cleaned
before handoff.

A symlink matches as a file by its own name, as Git judges one, and its
target is never read. A symlink the base doesn't ignore, or one over tracked
content, still blocks. So do special files and unusual modes under an ignored
directory.

## What Changed Since Revision 129

Revision 129 (`2026-11-17-1015-ignored-hydration-output.md`) decided that
every addition the base ignores leaves the candidate. Its rule named only
regular files and nested repositories, because only those had appeared. The
#1132 rule before it kept every non-regular change blocking, and the gate
test `TestIgnoredOutputRetainsNonRegularAndIntegrityChecks` pinned an ignored
symlink as blocking. Neither recorded a reason specific to symlinks. The
exclusion never reads an excluded path's content, so a symlink's target adds
no exposure that a regular file's bytes don't.

The #2968 implementation run disproved the premise. Building or testing the
app with SwiftPM always creates `app/.build/debug`, a link to the triple's
build directory. The writer's export was refused on that link after 35
minutes of work.

## Rejected

- **Asking the agent to delete build output before handoff:** it depends on
  agent behavior, and it leaves every other toolchain's links blocking.
- **Teaching the exporter to skip ignored paths:** the exporter doesn't hold
  the trusted base's rules, and revision 129 kept the ignore decision in the
  importer on purpose.
- **Excluding special files and unusual modes too:** none has appeared, and a
  FIFO or device node in a workspace is unusual enough to keep reporting.

## Tests

`TestImportExcludesIgnoredHydrationDebris` now carries SwiftPM's link under
both finding profiles. `TestImportIgnoredNonRegularAdditionStillBlocks`
generalizes the nested-repository test to symlinks: not ignored, and over
tracked content, both still block. The ignored-symlink assertion in
`TestIgnoredOutputRetainsNonRegularAndIntegrityChecks` moved to a special
file, which still blocks. Reverting the eligibility check fails the
hydration test.

## Refute-First Findings

From a fresh-context refute pass before commit.

- Confirmed: the symlink row of the over-tracked-content test used the rule
  `sub/`. That rule is directory-only, so it never matches a symlink, and the
  test passed without the tracked-content guard. The row now uses `sub`,
  which matches both kinds. Removing the guard fails both rows.
- Accepted: an excluded symlink carries no collision finding. Collision
  detection never queries a non-regular addition, which was already true for
  excluded nested repositories. The path never enters the tree, so nothing
  can collide. The comments that said collision findings always stay now say
  which additions they cover.
- Disproved: the target reaching a commit, the CAS, the commit plan, or a
  scan. The only read of a target compares it with the base symlink at the
  same path. A change with no object is never scanned, and construction
  reads only the filtered changes.
- Disproved: protected-path and Git-metadata findings dropping for an
  ignored symlink. Probes at `.github/workflows/`, a reviewer-instruction
  path, and a `.git` component all kept their findings or were refused.
- Disproved: a directory-only rule or a case-fold alias excluding a
  symlink. A symlink is queried as a file, and `/[A]pp/` over a tracked
  `app/` kept the gate on a symlink named `App`.
- Disproved: replay drift. The old build refused every handoff with an
  ignored symlink, so no recorded head disagrees with a replay.

Revisit when a toolchain's ignored output includes special files or unusual
modes, or when a project needs a symlink published on purpose.
