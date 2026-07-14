# Restore api CI vacuum binary after a merge clobber

Scope: `api/` (its CI workflow + README). Re-applies PR #48, which merged
clean at `d03b809` and then was silently reverted on `main` by the PR #47
merge (`606b2b4`).

## Fixed

- **Restored the pinned-binary api CI change.** `git checkout d03b809 --`
  brought back `.github/workflows/api-ci.yml`, `api/README.md`, and this
  session's sibling entry `2026-07-14-1240-api-ci-vacuum-binary.md` exactly
  as they merged. `main` had regressed to the old `go run` (~7min) validator.

## Gotcha

- **A later merge from a stale branch can revert an already-merged sibling
  PR, silently.** #47 (`fix/freeze-runner-capabilities`, tip `93ab618`)
  carried pre-#48 copies of the three api/ files; its merge commit
  `606b2b4` reverted them while adding its own legitimate
  `daemon/internal/exec` work. GitHub reported a clean merge and CI stayed
  green (the reverted workflow still lints fine), so nothing flagged it.
  Verified the clobber was scoped to exactly PR #48's three files by
  diffing the #47 merge against its first parent (`54f365c..606b2b4`); no
  other unrelated file was touched. Lesson: after a burst of parallel PRs
  merges, confirm each merged PR's files still reflect it on `main`
  (`git show main:<path>`), don't trust the merge-commit title alone.

## Verification

- Restored files are byte-identical to the `d03b809` merge state (checked
  out from it, not re-typed); the binary/checksum guard and its local
  refute checks already held at #48 and are unchanged.
- CI on the restore PR must show `api CI` green in seconds again (the
  original #48 run was 7s vs the ~7m17s `go run` baseline).
