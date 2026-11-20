# Recipe Steps Skip By Path And Record NotApplicable

Issue #3408 lets a verification recipe skip a command that no changed path
can affect, so a docs-only change stops paying for the daemon tests and the
macOS executor. The note is mandatory: the change is a contract change on
the verification-verdict path (Tier A).

## Decisions

- **Rules are path lists in the recipe, in CI's grammar.** A command object
  takes `paths` and the recipe takes `run_all_on`. A rule ending in `/` is
  a directory prefix; any other rule is one exact file, as `changed_under`
  in `pull-request.yml` reads its selectors. Chose this over globs because
  Freeside's recipe can then copy CI's selectors verbatim, and
  `scripts/check-recipe-selectors.py` in the `required` check proves they
  match. Globs, `.`, `..`, empty segments, and a leading `/` are refused so
  a rule can only mean one thing. A recipe where every command has `paths`
  is refused, so some command always runs.
- **Rules come only from the approved recipe bytes.** They are part of the
  recipe digest, so a candidate that edits them changes the digest and
  needs a fresh approval before they apply. The change set is the caller's
  `Options.Changes`, which must be the full set from the verification's
  base to the verified tree. Both callers pass one: the import's change
  account (a remediation round imports against the admitted base, so its
  account is the whole pull request's, not the round's) and the base-to-
  merge diff on re-entry and handoff.
- **Rules match folded paths too.** A path matches a rule exactly, or when
  its case-folded, alias-normalized key (the importer's collision model)
  matches the folded rule. A room on a case-insensitive filesystem checks
  `DAEMON/x_test.go` out into `daemon/`, and the importer's collision check
  doesn't flag a new file under a case-variant directory, so exact matching
  alone would skip the daemon tests for a file that joins a daemon package.
  Folding only adds matches. Found by the pre-push review.
- **Two change sets run everything.** An empty set has nothing to rule a
  command out with, and a path the daemon can't read as UTF-8 (the importer
  carries it in `PathHex`) can't be matched against a rule. Both run every
  command rather than guess.
- **A skip is recorded per step in the report, not as a readiness
  requirement.** The step keeps its slot with exit code 0 and a
  `not_applicable` proof: the digest of its rule and of the sorted changed
  paths. Anyone holding the approved recipe and the change set can
  re-derive it. Chose this over a separate readiness requirement because
  the report is already the verdict readiness and publication read, the
  field is omitted when nothing is skipped (old reports keep their bytes
  and digests), and `ParseReport` refuses a skip that also claims output,
  history, or a nonzero exit. The published section shows `Not run:`, so a
  skip never reads as a pass.
- **A skip assumes the step passes on the base.** That holds while CI runs
  every step on `main`, which the `required` check and the up-to-date rule
  give. A step CI never runs on the base could hide a failure the change
  didn't cause; copying CI's selectors keeps the recipe within what CI
  already relies on, and inherits CI's gaps. The pre-push review found one:
  daemon tests read `api/openapi.yaml`, `prompts/`, and three scripts that
  the `daemon` selector didn't name, so a spec-only change skipped
  `TestOpenAPIEnumsMatchDomain`. Before this change every candidate ran the
  daemon tests, so copying the gap would have been a regression. Chose to
  widen the selector and the recipe together over widening the recipe alone,
  which the agreement check refuses and which would leave CI's gap open.
  The agreement check proves the recipe matches CI, not that either covers
  what a step reads.
- **The proposed split was not applied.** The plan proposed two units
  (format and evaluation; Freeside's recipe and the agreement check). No
  second issue was filed, the issue body kept the whole contract, and the
  owner handed over the whole issue, so it lands as one PR.

Revisit when a recipe needs a rule CI's selectors can't express, or when
verification runs a step CI doesn't run on `main`.
