# Worker Environment Parity: Assessment and Deferral

## Decision

A worker-environment parity audit (containerized workers versus the
operator's locally installed agent) was assessed against the code and
plan on 2026-08-02. Its factual claims all verified. The owner
decisions that follow from it:

1. **The capability-profile, tool-profile, and skill-bundle program is
   deferred past 1B.0.** The audit proposed `WorkerCapabilityProfile`,
   `ToolProfile`, and `SkillBundle` contract concepts plus
   capability-aware onboarding and admission-time refusal. These are
   material plan changes on trust-critical surfaces, and the plan
   already places "more agents and skills" in Phase 4. Designing them
   before real-backlog use (which begins at the close of 1B.0 wave 4)
   would guess at gaps that the real runs will demonstrate directly.
   If executable skills prove missed in practice, a first minimal
   skill bundle (network-free, credential-free) is a candidate 1B.1
   amendment rather than a Phase 2 wait; either way it enters as its
   own gated plan change.
2. **Visual evidence needs no skill injection, but its capture half
   is not yet scheduled.** The design home exists: §5.15 rule 1
   assigns capture to the trusted recipe in clean rooms, and the
   provenance-gated EvidencePublisher is scheduled (1B.0 wave 5). The
   §5.18 wave-3 capture hooks are work-unit facts, not visual
   capture, and the `CaptureMode` registry holds only `CaptureNone`,
   so no scheduled unit produces a visual artifact for the publisher;
   that gap is escalated as #472. Injecting a capture skill into the
   implementation worker is rejected either way: §5.15 makes
   agent-captured images labeled claims, never publish-eligible
   evidence.
3. **No separate gh-imgup evidence stage.** The audit proposed a
   trusted evidence-publication stage running `gh-imgup`. That stage
   already exists in the plan as the EvidencePublisher (§5.15 rule 4,
   publish lane); the daemon holds the GitHub credential and can
   likely upload directly, so `gh-imgup` is at most an implementation
   detail inside it, not a new stage or an agent-visible tool.
4. **Near-term gaps became issues, not design work.**
   Follow-up: #469 (worker git author identity; a defect against
   §5.6's "the agent commits normally with git"), #470 (baseline CLI
   tools in the agent bases). #405 was flagged in place: its non-goal
   ("no builder changes beyond what a second base requires") likely
   cannot survive its own acceptance, because the generated project
   Containerfile runs `npm config`/`npm ci` from a base and the Codex
   base deliberately carries no Node; the expected rescope is a
   provider-independent toolchain layer.

## Skill Triage Model

Reusable taxonomy for future "give workers skill X" requests:

- **Daemon-owned function**: the skill automates workflow Freeside
  itself performs or has a planned stage for (visual-evidence via
  §5.15 capture plus the EvidencePublisher, await-pr-review via the
  review stage and watches). Never inject; the daemon's stage is the
  capability. self-merge and merge-cleanup sit outside this bucket:
  merge remains a human accountability checkpoint with automatic
  merge deliberately undecided, and post-merge branch cleanup and
  `main` resync stay with whoever approves the merge (forge
  auto-delete covers the remote branch), so both are operator-owned
  workflow, neither daemon-owned nor injectable.
- **Pure instructions**: conventions and checklists with no scripts or
  tools. Fold into the admitted stage-prompt/instruction bundle; the
  `prompts/` package is already open (`prompts/phase-1a/`, under
  control-plane trust rules), so this path exists today and needs no
  new mechanism.
- **Executable skills**: scripts, assets, and tool dependencies a
  worker would run. The only bucket needing a real SkillBundle
  mechanism (admission-snapshotted, digest-bound, read-only,
  stage-scoped); deferred per decision 1.

## Audit Corrections

Recorded so the audit is consumed with them attached:

- The Claude CLI vendors its own ripgrep for its Grep tool, so `rg`
  PATH-absence on the Claude base affects only Bash usage; the audit
  overstated that gap slightly.
- The git-identity failure is established statically (no `user.*` in
  the seed allowlist, no `GIT_AUTHOR_*` in the launcher environment,
  no baked gitconfig) but was not reproduced live; #469's acceptance
  starts with that reproduction.
- The audit's "operator personal defaults" configuration layer, if it
  ever lands, must be recast as reviewed control-plane configuration:
  §5.8 discipline, and the 1B.2 statement that client-side settings
  editing does not exist as a mechanism, both bind it.

## Revisit When

Real-backlog use in 1B.0 shows workers failing or thrashing for want
of an executable skill or tool the baseline (#470) does not cover;
that evidence reopens decision 1 at 1B.1 planning.
