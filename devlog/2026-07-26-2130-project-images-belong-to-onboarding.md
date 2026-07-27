# Project Images Belong to Onboarding, Not to `images/` (#304 Correction)

Owner decision, 2026-07-26, after #328 merged: remove `images/project-gh-imgup/`
and its two scripts. The agent base (`images/agent-claude/`) stays. This
corrects a decision recorded in
[`2026-07-26-2011-agent-claude-and-project-images.md`](2026-07-26-2011-agent-claude-and-project-images.md),
which is frozen; this note supersedes its project-image half only.

## What Changed

Nothing about the constraints. What changed is that the earlier unit never
weighed the checked-in project image against the plan's own division of labour:

- `docs/plan.md` §10 gives `freesided onboard <repo>` the job of detecting a
  managed repository's verification recipe and **building its project image**. A
  project image is therefore a runtime artifact of onboarding a repository, not
  source in the control plane. A hand-maintained directory for one repository is
  a stand-in for a component the plan already assigns elsewhere.
- The stand-in had a running cost the earlier note recorded as a limit rather
  than as an objection: `images/project-gh-imgup/package-lock.json` was a copy of
  another repository's dependency manifest, pinned at one commit. That
  repository has Dependabot enabled
  ([`2026-07-24-1419-audit-dynamic-workflows.md`](2026-07-24-1419-audit-dynamic-workflows.md)),
  so its lockfile moves on its own schedule and the vendored copy goes stale,
  putting another project's dependency churn into this repository's history. That
  is the pollution `images/README.md` already anticipates for vendor-CLI churn,
  arriving from a different direction.
- It also did not generalize: a second managed repository would have meant a
  second directory, a second vendored lockfile, and a second build script.

Rejected: keeping the directory marked "provisional" until onboarding subsumes
it. A provisional stand-in for a component nobody has started still has to be
refreshed every time the upstream lockfile moves, and the marker does not stop
the drift.

Rejected: moving the project image into the managed repository or a sibling
repo. It would preserve the artifact but not the contract; onboarding still has
to build project images for repositories that carry no Freeside-specific files.

## What Is Worth Keeping From the Removed Work

The build mechanics were sound and the replacement should reuse them, so they
are recorded here rather than only in the deleted files' history:

- **Bake a warmed npm cache plus a global npmrc, not `node_modules`.** The
  project's verification recipe then runs verbatim with no network, instead of a
  rewritten offline variant that would prove a different recipe.
- **Prove the offline property in both directions.** The positive run under
  `--network none` means nothing without a negative probe that masks the baked
  cache with an empty tmpfs and must fail *by reaching for the registry*; any
  other failure proves nothing about where the dependencies came from.
- **A project image is an agent image**, so it has to pass the ward allowlist
  check (`scripts/check-agent-image.sh`) as well as any dependency proof.
- The base is consumed by tag with its digest recorded in a label, because
  Apple `container` 1.1.0 resolves no locally built `name@digest` and the builder
  runs in its own VM.

Revisit when: onboarding's project-image build lands (#334), which is where
these mechanics belong.

Follow-up: #334.
