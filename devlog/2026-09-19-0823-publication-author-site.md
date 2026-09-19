# Author PR Metadata with a Judgment Role

Work unit: #1414. Plan revision: 64.

A model writes the public pull-request text after verification, and the trusted
publisher writes any issue-closing reference from daemon-held metadata. This
revises revision 63 (#1382), which decided that client work carries no generated
closing directive, that no inference call feeds publication, and that the agent's
description renders only as escaped text. The condition that changed: the first
real client run (gh-imgup #82) merged PR #110 without the `Closes` keyword its
target repository's template requires. That followed revision 63's design and
was not a bug, but it showed the gap.

This unit edits `docs/plan.md` only and touches no shared package, so its
`kind:contract` label does not apply (owner decision, 2026-09-19).

## Decisions

Eight points where the original proposal conflicted with code at `323133a9` or
with revision 63. Each is settled in plan text (§5.12, §5.13, §5.15, §9, §13).

1. **Revises revision 63, named explicitly.** §13 revision 64 names the three
   revision-63 decisions it changes and the condition that changed. AGENTS.md
   forbids silently overturning an owner decision.
2. **`Closes` trust depends on the source.** The daemon-bound `issue_subject`
   source is a resolution-verified binding the publisher may close. A
   client-supplied source URL is not daemon-verified: `canonicalSourceIssue`
   accepts any repository's issue and disclaims closing authority, and the
   client chooses the number. So a same-repository client-supplied URL closes
   only as a human-confirmed recommendation, marked unverified; a
   cross-repository URL gets `Refs` or the descriptive link. The human merge
   gate, not the daemon, is the authority for a client-chosen same-repo number.
3. **A first-class role, comprising two sites.** The publication author is a
   judgment role like the specifier, implementer, remediator and reviewer: a
   refinable prompt and per-role agent and effort selection through the
   admitted-agent lineup, not a deployment-pinned binding. It has no workspace or
   tools, and its authority stays advisory. It comprises two sites, an `explain`
   prose site and a `propose` closure site, keeping §5.13's one-authority-per-site
   rule while sharing one prompt and lineup entry. Chose the role framing over
   the pinned daemon-judgment-site pattern (owner directive) so its prompt, agent
   and effort can be refined and compared like the other roles.
4. **The closure directive is a registry effect.** A new closure proposal kind
   carries one bounded `resolves` flag, targets the daemon-selected source
   issue, has a trusted constructor and a gate that fails safe to no close.
   Publishing stays ungated; only the close directive depends on an approved
   proposal. This reconciles §5.13's "publication is not proposal-gated."
5. **v2 renders live Markdown behind a stricter screen.** Closing and automation
   directives are stopped by rejection at the body source, because GitHub reads
   close keywords from source, not rendered HTML. Because live Markdown drops
   v1's escaping, the screen also neutralizes issue and PR cross-references, bare
   URLs, commit references and `@`-mentions and renders raw HTML inert, so
   rendered prose cannot create a spurious cross-reference, a mention or
   backreference ping, or an invented link or image. Links and images resolve
   only to existing publishable evidence artifacts. Chose this over relaxing the
   screener. If the implementing unit cannot state a screen that holds those
   constructs inert, v2 keeps the prose inert as v1's escaped block, recorded as
   a fallback.
6. **Authored bytes reproduce.** The authored artifact is stored once, bound by
   digest, and re-rendered from storage on retry, restart and drift repair.
   Chose stored-and-replayed over regenerating text, which would break v1's
   guarantee that the same candidate yields identical metadata bytes.
7. **Runs once, after the final clean review.** §7 orders implement, verify,
   review, then clean publish, so verification and review outcomes both exist
   when the author runs; the output is stored once and digest-bound. Chose one
   post-review run over running at first publication (which would miss review
   outcomes and mis-order against §7) and over re-running at convergence (which
   would break reproducibility).
8. **The text carrier already exists.** `domain.ClaimText` landed 2026-07-20 and
   recipe v1 already reads it, so §9's claim that the claim path lacks an inline
   carrier is corrected. The publication author's prose rides its own advisory
   publication-authoring artifact; the briefer reuses `ClaimText`.

## Trust and Reachability Decisions

The trust model has several further decisions, each settled in plan text:

- **Closure approval through the `effect_proposal` action.** The closure
  recommendation is an effect-registry proposal honored only through the standard
  `effect_proposal` approval, which binds the proposal digest; admission alone
  never authorizes a close. The merge cannot serve as the approval: a merge binds
  the commit SHA, not the mutable proposal or PR body, so it could not prove the
  human approved those bytes. On approval the publisher writes `Closes` and
  merging then closes the issue.
- **Closure provenance.** The proposal carries provenance: `verified` for a
  daemon-bound `issue_subject` source, `recommended` and unverified for a
  same-repository client-supplied source URL. The constructor sets the flag with
  provenance rather than only for trusted sources, so the unverified same-repo
  source is admissible without being treated as daemon-trusted, and the approval
  card shows which it is.
- **Approval bound to the candidate head.** The proposal artifact is
  head-independent, so the approval also binds the exact publication identity and
  candidate head; a feedback or remediation successor supersedes a prior
  approval, so an approval never closes a changed candidate that may no longer
  resolve the issue.
- **A closable source always resolves.** A closable source always has a durable
  closure proposal, including a fallback defaulting to no close when the site or
  admission fails. Resolving writes `Closes` only when the `resolves` flag is
  set; approving a fallback or otherwise unset-flag proposal releases the gate
  with `Refs` and no close, so approval never closes despite a failure.
- **Forge-enforced merge gate.** Publication stays ungated, but a closable source
  holds the PR un-mergeable on the forge (opened as a draft or gated by a required
  check) until the closure question resolves. An internal readiness signal alone
  would not stop a maintainer merging directly on GitHub, so the gate lives on the
  forge; the mechanism is the publisher's.
- **Prose screening decoupled from the close.** A prose-screening failure falls
  the body back to v1 but still carries an approved publisher-written `Closes`;
  only a closure-proposal or metadata failure downgrades the reference. Coupling
  them would let a formatting fault in the explain site suppress a close the human
  approved, recreating the missing-close outcome.
- **No input more restrictive than the target.** No author input that can flow
  into public prose may be more restrictive than the publication target's
  visibility. Evidence inputs and reference resolution are restricted to
  policy-approved publishable evidence (`publish_eligible`); a source issue whose
  repository is less visible than the target (a private issue under a public
  target) contributes only its reference, never its body. Sensitivity tiers govern
  handling, not disclosure, so they alone do not close this path.
- **Sensitivity derived from repository visibility.** Every repo input takes a
  tier from repository visibility, not a fixed one: the diff and control files
  take the target's tier (normal public, sensitive private) and the source issue
  takes its own repository's tier. A `public` class is not used, since the
  `SensitivityClass` contract does not define it, and no repo input is left on a
  fixed `normal` tier, which would under-protect private-repo data in redaction
  and retention.
- **Scoped instruction snapshot.** The author reads the trusted-base composed
  instruction snapshot for the changed paths (nested AGENTS.md and overrides at
  every depth, §5.8), not the root AGENTS.md alone, so authored prose respects
  rules scoped to the diff.
- **Control-plane inputs from the trusted base.** The target repository's template
  and instruction snapshot resolve from the trusted base and are digest-bound,
  never the candidate head, so a candidate edit cannot influence the author or
  closure call (§5.8, §12).

## Decomposition Decisions

- **Lineup participation, this role only.** The revision decides lineup
  participation for the publication author. #900 covers extending it to the other
  daemon judgment sites (the task namer, finding classifier, finding adjudicator,
  drift auditor, diagnostic, attention-discussion and briefer sites).
- **Wardless admission class.** A lineup-selected role normally proves runner and
  ward conformance, which a wardless, tool-less role cannot. A daemon-side
  judgment admission class admits it: its launch proof is the admitted inference
  driver and prompt-package digest. The deterministic fake and budget cover output
  handling but do not prove the harness runs with no tools, the whole safety
  argument for a wardless role; that no-tools proof stays interim (hand-audited
  for the single Claude driver), with a per-driver capability proof left to #900
  before other call drivers join.
- **Agent vocabulary is its own contract unit.** Lineup participation needs lineup
  keys that accept role names, not only stage names
  (`daemon/internal/domain/agent.go`). That change plus the wardless admission
  class is a separate `kind:contract` unit (#1421) that `starts-after` #900 (owner
  decision on #900), not folded into #1417.
- **`issue_subject` reachability.** `issue_subject` is label-initiated, so label
  publication uses the same author role under its own intake contract; otherwise
  the verified-close arm would have no recipe path.
- **Recipe v2 selection.** New client submissions freeze the current recipe, now
  v2, so the author role is reachable; records that froze v1 keep it and its
  recovery and replay semantics. Otherwise the frozen-v1 rule would leave v2
  unselectable.
- **Scheduling left to the spine.** The decomposed units are unscheduled
  deferrals; the spine places them in a wave (expected 1B.1). This revision does
  not schedule them or edit the §11 table (#1414 non-goal).

## Rejected Options

- **Relaxing the v1 screener** to let agent text carry live directives or
  autolinks. The trust model keeps the ban: the agent recommends, the publisher
  writes any directive from daemon metadata.
- **One dual-mode site** producing prose and the closure flag together. It would
  need a second authority mode on one site and couple the flag's fail-safe to the
  prose's.
- **The merge as the closure approval.** A merge binds the commit SHA, not the
  proposal bytes, so it cannot prove the human approved the exact closure.
- **Regenerating the authored text** on retry or restart. It breaks byte
  reproducibility that recovery, drift repair and successor rules depend on.

## Revisit When

- The implementing unit finds no screen that holds links, images, mentions, raw
  HTML and directives inert under live Markdown. Then v2 ships with inert prose,
  and rich formatting waits for a later recipe version.
- A publication target needs a close directive from a source that is neither
  same-repository nor `issue_subject`-bound. That needs a new trusted binding,
  not a relaxed screen.
