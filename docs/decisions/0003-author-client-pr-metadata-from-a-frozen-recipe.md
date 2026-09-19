# 0003: Author Client PR Metadata from a Frozen Recipe

- Status: Accepted
- Date: 2026-09-19
- Decider: Ben Nelson-Weiss
- History: revision 63, "Candidate-Bound Public PR Metadata"
  (`docs/history/decisions.md`), revised by revision 64, "Author PR Metadata
  with a Judgment Role" (`docs/plan.md` §13 while current, then the same
  history file)
- Source notes: `devlog/2026-09-17-1430-client-publication-metadata.md`
  (revision 63, #1382) and `devlog/2026-09-19-0823-publication-author-site.md`
  (revision 64, #1414)

## Context

A client submission has no operator-written PR title or body, so the daemon
must produce public PR metadata for it. Revision 63 chose a deterministic
path: freeze a versioned recipe at submission and render the candidate
producer's own public-intended account as escaped text. It also decided that
client work carries no generated closing directive, that no inference call
feeds publication, and that agent prose renders only as escaped text.

The first real client run (gh-imgup #82) merged PR #110 without the `Closes`
keyword its target repository's template requires. That outcome followed
revision 63's design and was not a bug, but it showed the gap. Revision 64
revised those three decisions two days later. That first re-litigation is what
promotes the decision to this ADR (plan §13).

## Decision

Client PR metadata comes from a recipe frozen at submission. A judgment role
authors the public prose after review, and the trusted publisher alone writes
any close directive.

**Retained from revision 63:**

- New client submissions bind a versioned metadata recipe, commit attribution
  and an optional source-issue reference before reserving implementation
  identity. Only a complete canonical GitHub issue URL supplied as the source
  may become a descriptive Source issue link. Unknown recipes and mixed
  literal and derived inputs fail closed.
- Task names and the private `freeside.summary` channel never supply PR prose.
  Explicit CLI and label-intake title and body bytes stay literal.
- The same candidate's rendered prose reproduces byte for byte across retry,
  restart, lost-response recovery and drift repair. A remediation or feedback
  successor is a new candidate with its own account of the whole current
  change, never its predecessor's.
- Agent-reported results stay claims. A public label or sensitivity value
  grants no publication authority, and the publisher still owns Verification,
  review history, advisories, scope decisions and the identity marker.
- Recipe v1's rendering is fixed; a rendering change needs another recipe
  version. The implementer still writes the v1 claim
  (`.freeside-evidence/publication.md`), now as the deterministic fallback.

**Revised by revision 64:**

- **New client submissions freeze recipe v2.** The frozen version is
  per-submission and never rewritten, so records that froze v1 keep it with
  its recovery and replay semantics. v2 layers an authoring pass over the v1
  claim and adds no client-supplied trust bit.
- **An inference call now feeds publication.** The publication author is a
  first-class judgment role, like the specifier, implementer, remediator and
  reviewer: a refinable prompt and per-role agent and effort selection through
  the admitted-agent lineup, with no workspace or tools and advisory authority
  only. It comprises two sites sharing one prompt and lineup entry: an
  `explain` prose site and a `propose` closure site. It runs once, after the
  final clean review, so verification and review outcomes are inputs. Its
  output is stored once, bound by digest and re-rendered from storage, never
  regenerated, which keeps revision 63's reproducibility guarantee for the
  authored prose. Label-initiated publication uses the same role under its
  own intake contract.
- **Agent prose may render as live Markdown.** Recipe v2 renders the authored
  prose through a screen at least as strict as v1's. The screen rejects close
  and automation directives at the body source and keeps cross-references,
  mentions, raw HTML and invented links and images inert. An unavailable role
  or a screening failure falls back to the path's deterministic text (the v1
  claim rendering for client work) and adds no publication block.
- **A PR may carry a close directive, written only by the publisher.** The
  closure recommendation is an effect-registry proposal approved through the
  `effect_proposal` action, which binds the proposal digest, the publication
  identity and the candidate head; a successor with a new head supersedes any
  prior approval. A daemon-bound `issue_subject` source, reached only through
  label intake, is a verified close. A same-repository client-supplied source
  URL is an unverified recommendation the human confirms. A cross-repository
  URL, or any failure to admit, approve or resolve the proposal, yields `Refs`
  or the descriptive link and no close. Approval is the one step that changes
  the body's issue reference from `Refs` to `Closes`.
- **The close and the prose fail independently.** A prose-screening failure
  falls the body back to v1 rendering but still carries an approved `Closes`.
  A closable source always has a durable closure proposal, including a
  fallback that defaults to no close when the site or its admission fails.
  Publication stays ungated, but a closable source holds the PR un-mergeable
  on the forge until the closure question resolves.
- **No author input is more restrictive than the target's visibility.**
  Evidence inputs and link resolution are limited to `publish_eligible`
  evidence.
  Every repository input takes its sensitivity tier from repository
  visibility, and a source issue less visible than the target contributes
  only its reference, never its body.
- **Control-plane inputs resolve from the trusted base.** The target
  repository's template and composed instruction snapshot resolve from the
  trusted base and are digest-bound, never the candidate head.

## Consequences

- A PR can satisfy a target repository's template, including a required
  `Closes` line, without an agent ever writing a directive.
- The safety of live Markdown rests on the v2 screen. If the implementing unit
  cannot state a screen that holds links, images, mentions, raw HTML and
  directives inert, v2 ships the prose inert as v1's escaped block, and rich
  formatting waits for a later recipe version.
- A wardless, tool-less role needs its own admission class. Its no-tools proof
  stays interim (hand-audited for the single Claude driver) until #900 defines
  a per-driver capability proof. The admission class and the role-name lineup
  keys are one separate contract unit (#1421) that `starts-after` #900.
- A close directive from a source that is neither same-repository nor
  `issue_subject`-bound needs a new trusted binding, not a relaxed screen.
- This ADR records the decision as of revision 64 and decides nothing new. The
  rejected options, including relaxing the v1 screener, one dual-mode site,
  the merge as the closure approval, regenerating authored text and same-task
  retry authority for a metadata failure, live in the source notes with the
  refutation findings.
