# The Workflow Audit Treats xcode-27 As GitHub-Hosted

Chose to add `xcode-27` to `standardGitHubHostedRunnerLabel` over leaving it
to fail high, because GitHub's hosted runner reference lists it as a standard
arm64 macOS label for public and private repositories. #2461 moves this
repository's `app-ci.yml` and `convergence-ci.yml` onto that image. Without
the label, the audit reports `SelfHostedRunners`, so an existing trust profile
with `AllowSelfHostedCI` false fails publishing on `self_hosted_runners`
drift, and fresh onboarding records `AllowSelfHostedCI: true` for a repository
with no self-hosted runners.

This follows the rule in `devlog/2026-07-21-1923-live-automation-trust-audit.md`:
only documented standard hosted labels clear the axis without an inventory
match. It adds no new class of trust.

## What Stays Out

- **`xcode-27-xlarge`:** A larger runner, not a standard label. It still fails
  high.
- **Unknown labels:** They still report self-hosted. So does a future
  `xcode-28` until GitHub documents it and someone adds it here.
- **Existing trust profiles:** A profile onboarding already recorded with
  `AllowSelfHostedCI: true` from the wrong reading stays as recorded. It
  approves more than the repository uses, and it doesn't cause drift.

## Refute-First Findings

An independent reviewer tried to prove the change wrong, with probes in a
temporary copy of the code.

- **A hosted-looking `runs-on` reaching a self-hosted runner: disproved by a
  check.** Only `xcode-27`, its case variants, its quoted form, and
  `[xcode-27]` clear the axis, and all of those still report self-hosted when a
  repository runner matches. Other label sets, `group:` mappings, expressions,
  and matrix values fail high. A `workflow_call` job with a matching repository
  runner reports self-hosted.
- **A repository runner carrying the label going unseen: disproved by a
  check.** The inventory read runs before analysis and fails closed on a
  non-200 response, a short page, a malformed row, or the 100-page limit. Nil
  and empty inventories behave the same. The code applies no status filter to
  the runners GitHub returns. GitHub's docs don't say whether offline runners
  are returned.
- **Label not really a standard hosted label: disproved by a check.** Both the
  public and private tables in the hosted runner reference list it. No other
  allowlist of these labels exists in the repository.
- **New test rows not proving the behaviour: disproved by a check.** Dropping
  the label fails the hosted row, and checking the allowlist before the
  inventory fails the runner row. Case handling and `[xcode-27]` have no test
  row; the case gap predates this change.
- **Organization and enterprise runners: allowed by an explicit decision.**
  The audit lists `/repos/{repo}/actions/runners`, which doesn't return them,
  and GitHub's docs don't reserve hosted label names. A runner registered at
  those levels with the label `xcode-27` would be missed. The gap is the same
  for every label on the allowlist, and the 2026-07-21 note accepted it. The
  reviewer's inference is that `xcode-27` is somewhat likelier than
  `ubuntu-latest` as a custom Mac fleet label, which raises the odds of a
  collision but doesn't change the kind of gap. Whether `freeside-ai` has such
  a runner is unverified, because listing them needs `admin:org`. The
  repository has 0 runners, and #2461's `convergence` job ran in runner group
  `GitHub Actions`.
- **Scale-to-zero ephemeral runners: allowed as a known limit.** The inventory
  is a snapshot, so a repository runner that exists only while busy can be
  absent when the audit reads it. This is the reviewer's inference, not an
  observed case, and it applies to every label.

Revisit when the audit gains a view of organization or enterprise runners, or
when GitHub drops `xcode-27` from its standard labels.
