---
run: manual
stage: reconcile
date: 2026-07-13
branch: docs/plan-v4-reconcile
---

# Plan v4 reconciliation

Plan v4 replaces the committed v1 verbatim, not merged; v4 §13 is the
full decisions log. Deciders: three external reviews plus user
decisions (named per item in §13). Major reversals, one line each; no
code should ever be written against the committed v1:

- **Credential model:** v1's per-stage tokens inside workspaces → no
  GitHub credential ever enters a workspace; a daemon-side git/publish
  service owns all mutations after gauntlet validation (§5.4–5.6).
- **Execution layer:** ACP sessions → StageDriver batch jobs with a
  permanent fake driver; ACP returns only in Phase 3 for interactive
  attachment (§5.3).
- **Integration:** webhook-first → per-resource reconciliation plus
  intake scanners; webhooks are Phase 2 at most (§5.11).
- **Pipelines:** YAML pipeline DSL → code-defined Go state machines
  with YAML as policy only (§5.12); `pipelines/` renamed `policy/`,
  explicitly control-plane (§5.8).
- **Roadmap:** Phase 0 validation study deleted into the 1A exit
  gates; app/ ships minimal clients in 1A, not Phase 2 (§11).

## Decisions

- **Document gating is now materiality-based** (AGENTS.md, per plan
  §9): material changes stay their-own-PR; wording changes are
  recorded in the carrying PR. Chose to update the convention with the
  plan in one PR because the plan change is this PR's direct subject.
- **Coherence sweep scoped to touched surfaces** (user choice):
  AGENTS.md intro/table/cross-refs and the daemon/, app/, prompts/
  READMEs were de-v1'd; `api/` and `images/` left byte-identical per
  the reconciliation instructions, so their §4.x references and
  v1 phrasing are knowingly stale until those directories' first real
  PRs. Scope-discipline convention unchanged; only its dangling
  §4.6/§4.7 ref moved to §5.6/§5.8 (stated deviation, wording-level).
- **"Free as in bird" returns to the root README** (user, via the
  reconciliation prompt), reversing the bootstrap session's removal;
  the register line "the harness runs the agent; the reins are yours"
  lands with it (plan §15 naming stack).
- **Repo description** updated to the category line (user-confirmed
  wording): "An agent control plane: turns software work items into
  evidence-backed pull requests and interrupts you only when judgment
  is required."

Drains `2026-07-08-1051-scaffold-phase0.md`: the devlog-contract
queue item resolved via plan v4 §9's cadence split — this repo
protocol is unchanged for human sessions, autonomous runs write
summaries to the artifact store, and the previously expected
run-front-matter extension is not needed. The license ADR-candidate
item is re-deferred here: open-sourcing remains a Phase 4 packaging
decision (plan §11); nothing in this scope touches it.

## Verification

- `docs/plan.md` confirmed byte-identical to the source v4 file
  (`diff -q`), then corrected for one erratum the source carries
  (Codex P2): §4's run_proposal item pointed at a nonexistent
  §4.11; initiators are §5.12. A sweep of every Section/§ and bare
  parenthetical cross-reference against the header list found no
  other dangling target. Wording-level fix, recorded here and in
  the PR, no revision increment (plan §9); the user's source copy
  still carries the typo.
- Managed AGENTS.md blocks extracted before/after and diffed:
  byte-identical (the canonical compare script is not in this repo;
  the awk extraction stands in).
- Coherence grep over touched surfaces for `§4.x`, `Phase 0`, and
  `pipelines/` residue; remaining hits are only in `api/`, `images/`
  (deliberately untouched) and devlog archaeology (frozen).
