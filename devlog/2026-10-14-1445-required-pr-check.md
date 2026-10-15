# One Required Check on Every Pull Request

Branch protection on `main` now names one required check, `required`,
reported on every pull request by `.github/workflows/pull-request.yml`.
That activates the up-to-date rule (`strict: true`) that
`2026-07-15-2222-public-repo-settings.md` left inert: a PR merges only
after its checks pass on its content combined with the tip of `main` it
lands on. This is rule 2 of the parallel-work proposal
(freeasinbird/free-skills#291, `docs/proposals/parallel-work-design.md`
§3.1 and §4 step 1), which the owner adopted on 2026-10-14. Issue #2017
carries the work contract.

## What Changed

The 2026-07-15 note kept base freshness a convention, enforced by the
merge-result audit and the handoff procedure, and left the forge rule off.
Its revisit condition was "forge-level freshness or check enforcement
becomes worth adding a required context". The owner's adoption of the
proposal meets that condition: the proposal moves the base-advance recheck
from the author to the forge, and the up-to-date rule does nothing until a
check is named.

## Decisions

- **Chose one always-reporting caller over the no-op twin workflows.** The
  2026-07-15 note rejected naming the path-filtered component jobs directly,
  because a skipped workflow never reports and a required context that never
  reports blocks the merge forever. The twins (a matching job name behind an
  inverse path filter) fix that per job, but double the workflow count and
  must track every job rename. The note's lighter variant, one always-run
  gate as the only required context, needs one name in branch protection
  however the component jobs change.
- **Chose to call the component workflows (`workflow_call`) over running
  `scripts/check.sh <component>` in the caller.** A component's steps span
  Linux and macOS runners (daemon, app, scripts), and each component
  workflow already carries its setup, caches, and tool pins, including
  daemon lint on golangci-lint-action. Running `check.sh` in the caller would
  duplicate that setup per runner and drift from it. The component workflows
  lose their `pull_request` trigger so they run once per PR event, through
  the caller, and keep their `push` trigger and filters unchanged.
- **Chose a copied path list in the caller over a changed-files action.**
  Every filter entry is a directory prefix or an exact file, so a short bash
  match covers it without a new third-party dependency. The cost is that the
  caller's lists copy the component workflows' `push` filters; AGENTS.md
  (Build, Test, Run) tells a new component to register its paths there. A
  change under `.github/workflows/`, or a PR too large for the files API to
  list in full, selects every component, because under-selection is the
  failure that lets a broken change merge.
- **Chose literal concurrency-group prefixes in the called workflows.** In a
  called workflow `github.workflow` is the caller's name, so the old
  `${{ github.workflow }}-...` groups would collide across components if
  GitHub honors a called workflow's concurrency. A literal prefix is correct
  whether it does or not.
- **The gate folds in the commit-message check.** `commit-messages.yml` was
  the one PR check without a path filter; the caller runs it on every PR, so
  the single required context covers it too.

Revisit when: the merge queue is adopted (#2025, which adds a
`merge_group` trigger to the caller), or the daemon race jobs join the
required check (#2019).
