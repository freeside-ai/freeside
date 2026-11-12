# Freeside Protects No Repository-Specific Path

Issue #3123, under plan revision 118 (#3124,
[note](2026-11-12-0840-advisory-automation-control.md)). A change to the
protected paths Freeside's trust profile binds and to a Tier A gate test,
so a mandatory note.

Chose to drop all 18 `-protected-path` flags from Freeside's onboarding
over keeping the check scripts, the recipe, and `docs/plan.md` protected,
because every kind of development done by hand today now goes through
Freeside, and a protected edit is refused at import and loses the writer's
run.

## Decision

- **Onboarding passes no `-protected-path` flag.** Freeside's profile
  widens no class, so its `protected_paths` is empty.
  `TestImportFreesideProtectedPaths` pins that, and `daemon/README.md`
  (Onboard Freeside) carries the same statement.
- **The importer's built-in patterns still apply.** Automation-control
  (`.github/workflows/**`, `.github/actions/**`, and the rest of
  `DefaultAutomationControlPatterns`) holds publication until #3125 makes
  it advisory. Reviewer-instruction stays advisory. Git-metadata stays a
  waivable finding. The flag itself stays available to other repositories.
- **Freeside's profile needs a fresh review if it was approved with the
  flags.** The approval digest binds the patterns, so onboarding after
  this change avoids a second review.

## Direction

- **Owner, 2026-11-11 (tracker #3132):** every kind of development done by
  hand today goes through Freeside, with no pause for approval, so paths
  are un-protected where protection blocks that work.
- **Owner, 2026-11-11, during `Plan #3123`:** confirmed the full list. Drop
  every flag, matching #2873's exit ("Freeside's profile protects no
  repository-specific path"). The owner widened #3124 to remove the plan's
  operator-authored sentence first.

## Reverses

- [`2026-10-27-1204-freeside-protected-paths.md`](2026-10-27-1204-freeside-protected-paths.md):
  the operator-authored list, `docs/plan.md` protected whole, and the
  `api/` and `scripts/` additions that followed it.
- [`2026-11-10-1040-app-steps-executor-switch.md`](2026-11-10-1040-app-steps-executor-switch.md):
  #2883's choice to protect `.github/**` whole. Files such as the pull
  request template and `CODEOWNERS` now publish; workflows and actions
  still hold under the defaults.

## Rejected

- **Keep the verification scripts and `docs/plan.md` protected.** Every
  such edit stays hand-run, and a writer that touches one loses its run at
  import. The writer is never told the list, so it can't avoid them.

## What the Owner Accepts

- **A candidate that edits a check script is graded by its own edit.**
  Freeside's verifier runs the approved recipe from `main`, but the
  recipe's commands call the candidate's copies of the scripts, and CI's
  `required` check runs them too. The owner's pull-request review is the
  only check on such an edit. The verify stage still raises a non-blocking
  risk flag for the recipe file, the scripts the recipe names directly
  (`scripts/check.sh`), and its built-in patterns such as
  `**/.golangci.yml` (`daemon/internal/verify/flag.go`). It no longer flags
  the other formerly listed files, such as `scripts/check-vocabulary.sh`,
  `scripts/daemon-test-shards.json`, or `api/vacuum.ruleset.yaml`.
- **A pull-request workflow run executes the candidate's scripts** with a
  read-only token, no secrets, and no elevated triggers (verified
  2026-11-11, re-read 2026-11-12 in the revision 118 note). That holds
  while automation-control blocks; #3125's advisory option changes it, and
  the revision 118 note records that exposure.
- **A `.githooks/**` edit runs on a maintainer's machine** at commit once
  it is merged and checked out.
- **An edit to `.freeside/verify.json` doesn't change its own candidate's
  verification**, because §5.8 loads the recipe only from an approved
  default-branch commit. After merge, a changed recipe still needs
  approval.
- **Agent edits to `docs/plan.md` publish.** The plan's materiality gate
  then rests on the owner's review of the pull request.

## Revisit When

- A candidate edit to a check script passes verification and shouldn't
  have.
- A repository secret, an environment, or a self-hosted runner is added to
  Freeside, which would change what a candidate's scripts can reach in CI.
