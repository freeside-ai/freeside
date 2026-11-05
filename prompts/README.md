# Prompts

Versioned stage prompt files. Agents appear where judgment is the work: specifier, implementer, remediator, diagnostic, finding classifier, later a briefer (see `docs/plan.md` §5.13). Traces record prompt/config digests per run (§8), so these files are the tuning dataset's independent variable.

Stage prompts are **control-plane** content under the same trust rules as `policy/`: loaded only from an approved default-branch commit, digest-snapshotted by running stages; workspace copies are data (see `docs/plan.md` §5.8).

- **Toolchain:** none (prompt text, versioned like code).
- **Scope boundary:** prompt content only; the engine that references it lives in `daemon/`, the policy that configures stages lives in `policy/`. Changes here are control-plane changes: gated, reviewed like code, never batched silently into feature PRs.
- **Phase 1A packages:** [`phase-1a/specifier.md`](phase-1a/specifier.md)
  is the research-and-specification contract,
  [`phase-1a/implementer.md`](phase-1a/implementer.md) is the implementation
  contract, and [`phase-1a/remediator.md`](phase-1a/remediator.md) is the
  remediation contract, which also drives the operator-feedback round. The
  daemon admits the stage-specific content digest and materializes those exact
  bytes from the artifact store; the workspace path is never prompt authority.
- **Summary shape:** each package's reserved summary mirrors a report the
  development workflow already uses, so an owner reads the same parts on a
  card as in a session handoff. The specifier's takes the Result, Your call,
  and Not verified parts of the plan-work-unit skill's report
  ([`skills/plan-work-unit/SKILL.md`](https://github.com/freeasinbird/free-skills/blob/main/skills/plan-work-unit/SKILL.md),
  Report the Handoff); the implementer's and remediator's take the parts of
  the handoff report in [`docs/agent-workflow.md`](../docs/agent-workflow.md)
  §handing-off step 7. Both leave out Status, which the card's daemon facts
  carry.
- **Judgment-role prompts:**
  [`publication-author.md`](publication-author.md) is the shared, refinable role
  prompt for the publication-author judgment sites (plan §5.13, §5.15). It is
  flat and named by role. Unlike the phase-1a stage packages, a judgment-role
  prompt reaches the Claude judgment driver directly through a daemon flag (the
  daemon reads the file at startup and folds its digest into the judgment
  configuration digest), not through the workspace artifact store.
