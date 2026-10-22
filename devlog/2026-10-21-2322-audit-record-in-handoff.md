# Name The Audit Base In The Handoff, Not The PR Body

Mandatory note: explicit owner choice. The agent-setup update to the latest
canonical PR-body and handoff rules conflicted with Freeside's own rule,
from `devlog/2026-07-14-1738-merge-result-audit.md`, that PR Verification
records the merge-result audit's base SHA, command, and verdict.

## Decisions

- **The owner chose to adopt the canonical rule (2026-10-21).** The
  merge-result audit still runs against freshly fetched `origin/main` before
  handoff and after a base advance. Its base SHA now goes in the handoff
  report's Status line (`docs/agent-workflow.md` §handing-off step 7), and
  the PR body carries no audit line, base-commit bullet, or SHA.
- **A passing audit writes no verdict.** The canonical handoff report also
  omits audit lines, so "open and green, ready to merge" is the pass record,
  and a failing audit is reported as a PR that is not ready. Rejected keeping
  the Verification bullet as a project override: the synced
  `docs/agent-workflow.md` says to leave it out, so AGENTS.md would have
  contradicted the procedure it points at.
- **Why the body line can go.** The line recorded that the author ran the
  audit; it never enforced anything. CI does not run
  `scripts/merge-result-audit.sh`, so the `required` check does not replace
  it. What covers integration is the lander, which reruns the audit per PR
  against the tip it selects (`devlog/2026-10-19-1500-landing-record.md`).
  No script or skill reads the body line, and a recorded SHA went stale at
  the next merge to `main` anyway. A PR merged by hand, outside the lander,
  keeps only the author's pre-handoff run, reported in the handoff.

Revisit when PRs regularly merge outside the lander, or a consumer needs
the audit verdict from the PR itself.
