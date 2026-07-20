---
run: manual
stage: intro-doc-layering
date: 2026-07-19
branch: docs/intro-doc
---

# Layer a non-normative intro over the plan instead of simplifying it

Owner decision, made in chat and recorded here. Declared paths:
`docs/intro.md`, `README.md`, plus this note.

## Context

The documentation surface had two points with nothing between them: a
27-line README whose pitch is dense with insider vocabulary, and
`docs/plan.md`, a ~1,200-line normative spec. A readability pass on the
plan had already run into its natural limit: the plan's §-references
are load-bearing across AGENTS.md, issues, code comments, and decision
notes, so structural simplification is expensive by construction, and
fidelity-losing simplification of a normative document is the wrong
trade.

## Decision

Add `docs/intro.md` as an explicitly **non-normative** narrative
companion: goals and core ideas in plain language, subsystem names
introduced inline where their concepts land, a "plan wins on conflict"
disclaimer at the top. The intro cites the plan; nothing cites the
intro. README links it as the first step of a reading ladder (intro →
plan → AGENTS.md).

Rejected alternative: continue simplifying `docs/plan.md` itself.
Rejected because the readability need and the precision need pull one
document in two directions, while a layered companion lets each
document hold one job; and because §-renumbering would invalidate
references the repo depends on.

Drift containment: the intro stays at the goals-and-concepts altitude
(which changes rarely) and carries no feature lists or status detail
(which churn per wave). Corrected in the epistemics round: the disclaimer
protects authority, not readers. As the recommended first read, a
stale intro misleads nearly everyone regardless of its formal status,
so a material change to identity, the safety model, or the success
criteria explicitly triggers an intro review (also a revisit
condition below).

## Review reframing (owner review on PR #192)

The owner's first-round review rejected the draft's frame, which had
been adapted from the plan's defensive enumeration, in favor of first
principles: the intro now leads with maximizing agent autonomy and
optimizing the human's remaining involvement (taste, judgment, long
context, available from anywhere), with safety framed as the cost side
of an agent cohabiting your computer, not as the purpose. Consequent
changes: isolation presented as what buys autonomy (not
credential-withholding for its own sake, and the escalation path for a
blocked agent stated); the workspace described as a working copy that
is untrusted, not "not a working copy"; the ready card carries the
agent's labeled assessment alongside mechanical evidence; the guarded
control plane explicitly allows agent learnings to flow back through
the gated path (self-improving, never self-modifying); "framework for
growing autonomy" added beside the workflow-controller identity.

One deliberate softening: the intro says Freeside "does not merge for
you" rather than "never auto-merges". The plan's §2 non-goal 1 still
says "never auto-merges"; the owner does not want that door closed out
of hand, so aligning the plan is a pending material plan edit, decided
by the owner, not this docs unit.

Round two added an owner priority the plan barely carries:
**information presentation as a first-class human-optimization**.
Agents produce more text than anyone reads, and unread text pushes the
human out of the loop; decision cards should lead with the ask and a
summary, push detail lower, and digest changes, plans, and feedback.
The intro gained a core-idea section for it ("Information is shaped
for the decision"). The plan's §9 Comprehension is two lines
("evidence packets first; altitude summaries later"), so specifying
this properly is a candidate material plan addition, owner's call.
Follow-up: #194.
Also per owner in round two: no inline plan references in the intro's
body (the disclaimer and reading map remain the only pointers), the
names recap section removed (subsystem names stay inline only), and
"recipe" dropped from the intro's vocabulary in favor of plain
descriptions of trusted verification.

An owner epistemics round recalibrated the intro, all
charter-level candidates for the alignment harvest: leverage (useful,
correct work per unit of human attention, maintenance, cost, and
risk) stated as the objective with autonomy strictly a means, and
oversight valued as sampling and accountability rather than pure
overhead; promotion to standing grants requires low risk, stable
preconditions, and bounded downside, never repetition alone;
verification described as reproducible evidence that raises
confidence and gates readiness, not proof of correctness;
containment described as publication authority outside the boundary
plus explicit bounds, with provider-credential and egress exposure
named residual risk (secret scanning best-effort, unable to prove
absence); the success claims' measurement acknowledged as designed
work (sampled decision audits, normalization by volume and risk,
maintenance accounting), not a byproduct of raw telemetry; and human
merge framed as current accountability policy whose risk-bounded
relaxation stays an open question, resolving the earlier internal
contradiction with this note's auto-merge flag.

An owner editorial sweep was applied nearly wholesale and is binding
for future intro edits: one sober register with color
used sparingly (the tagline and "the boundary does the watching" are
the deliberate survivors); second person for the owner, "the agent"
for execution, "human" only in generic claims; the walkthrough as a
numbered sequence; four consolidated core ideas (attention +
information design; bounded autonomy; untrusted output + independent
evidence, with CI and control-plane guarding as subpoints; durable
decisions); absolutes kept only for enforced invariants, softened for
outcomes; and an explicit intended-product marker pointing at the
README for current status. Target length ~1,400 words nominal; the
owner later accepted landing somewhat above it after the epistemics
and harness-routing additions.

A measure-alignment round aligned the success model and narrowed
guarantees:
"leverage" restated as net leverage (a balance, not a pseudo-ratio);
success claim 1 restated as useful, correct work per unit of
attention rising against a normalized baseline (a charter divergence
to harvest: plan §1 claim 1 says "attention falls", which omits the
numerator); safety invariants verified by conformance and adversarial
tests, not read off telemetry; the review-surface non-goal narrowed
(code review on GitHub, workflow decisions in Freeside) with
"automatic merge" named; guarantees narrowed to their real scope
(push/publish on GitHub; provenance-to-rerun rather than reproducible
evidence; unreviewed candidate code out of secret-bearing CI;
supported, reconcilable external actions); and summaries made a named
trust surface (producer identified, uncertainty preserved, linked to
evidence), which #194's specification should carry forward. A closing
correction framed the four claims as necessary gates, with cost and
maintenance still deciding whether passing them yields net leverage.

An owner round named harness-agnosticism as an under-attended
theme: routing different agents, harnesses, and reasoning depths to
appropriate task types, assessing them from recorded outcomes, and
absorbing the manual balancing (including usage-limit-driven
switching) the owner does by hand today. The plan holds the hooks
(StageDriver, §7 provider diversity and routing comparisons, §8
per-run driver/cost/outcome telemetry) but §2 non-goal 5 defers
automatic provider fallback "until explicitly earned"; the owner
direction confirms that door should open on evidence, and that the
manual balancing burden belongs in the attention accounting. The
intro states it staged (routable choice, policy informed by
outcomes, automatic switching deferred until earned).

## Pre-merge owner revision round

A final owner rewrite (folded into the single commit) revised the
intro's voice and made deliberate divergences that extend the
plan-alignment harvest:

- The thesis sentence now leads with granting autonomy ("grants
  agents the autonomy to turn work items into evidence-backed pull
  requests"), diverging from the plan §1 / AGENTS.md canonical
  one-liner; the harvest decides which formulation is canonical.
- The objective vocabulary moved from "net leverage" to "positive
  return"; plan §1 still says "net personal leverage after
  maintenance".
- Oversight reframed from deliberately spent sampling and
  accountability to non-optional and deliberately frictionless
  ("oversight that's a chore is oversight that gets skipped"); the
  plan carries no oversight statement at all.
- The standing-grants promotion criteria added in the epistemics
  round (low risk, stable preconditions, bounded downside, never
  repetition alone) were cut from the intro; whether they belong in
  the plan instead is a harvest question.
- Durability now states "anything that cannot be safely retried
  waits for you" and drops the "supported" qualifier; the plan
  specifies neither the fallback nor the qualifier's scope.
- The routing staging sentence ("automatic switching waits until the
  evidence earns it") was cut, and "usage" joined the routing
  inputs.
- Register: title-case headings (repo-convention question filed as
  #207); subsystem names now land at the end of their sections,
  still inline; the mobile/anywhere mentions trimmed to the
  ready-card step.

Revisit when the intro is declared ready to merge: a "plan alignment"
harvest of this PR's review threads (the first-principles charter
reframe of §1–§2 including the leverage objective, the auto-merge
non-goal wording, the measurement-design direction, the
harness-routing theme against non-goal 5, register additions the
review settled) becomes actionable at that point and gets its
tracker issue then, per the ordinary deferral escalation. The owner
deliberately declined an earlier filing so the harvest sees the
finished conversation rather than a mid-review snapshot; this note
records that rationale, not the work item. Follow-up: #208 (filed at
ready-to-merge).

Revisit when: a concrete need forces restructuring the plan's section
numbering (then a section-mapping table ships with it), or a material
plan change contradicts the intro's narrative (then the intro updates
in that unit or the next docs pass), or the auto-merge non-goal is
revisited in the plan (then the intro's phrasing is re-checked), or
the comprehension/presentation theme is specified in the plan (then
the intro's section is re-checked against it), or any material plan
change touches identity, the safety model, or the success criteria
(then an intro review is mandatory, not discretionary).
