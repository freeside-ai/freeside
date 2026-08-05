# The Review Anchor Resolves Pre-Publication

**Decision.** Plan revision 28 resolves the §7 review-anchor fork,
deliberately carried unresolved since revision 25, as PRE-PUBLICATION
(decider: user, 2026-08-05, in the operator session for the first
production backlog run): implement → verify → review → clean: publish,
the PR opening already reviewed, forge checks still gating merge. Two
companions: the EvidencePublisher's first slice is pinned as the
disposition history on the PR at publication (#525, the owner's
condition on the resolution), and the external review response
capability is named in the roadmap and filed as #524.

## Final Rationale

The framing sentence: the internal loop is the agent's pre-push work;
the PR is the collaboration surface.

- **The PR list stays a decision queue, not a work queue.** A PR that
  exists is a candidate for human judgment; publishing before review
  turns the forge into a progress tracker for unfinished work.
- **Post-publication state is the expensive place to be correct.** The
  #496/#514 ready-identity class showed what correctness costs once
  state is bound to a live PR: identity divergence, staleness
  invalidation, atomic supersession, schedule conclusion. Every stage
  moved before publication avoids that class instead of hardening
  against it.
- **PR comments are mutable, so the authoritative ReviewRecord lives
  in the store under either anchor.** PR-anchoring would therefore
  mean building both surfaces: the durable store record and the
  comment-thread choreography on top of it.
- **The owner's own usage is served without comment threads.**
  Progress pulse, forensic drill-down on agent/forge disagreement, and
  disposition reconstruction are served by computed readiness, the run
  timeline, and structured dispositions.

## Rejected Alternative

**PR-anchored post-publication review**, as §11's 1B chain read from
revision 25 through 27: publish the verified candidate, then run the
required review rounds against the open PR. Rejected on the rationale
above, not on capability: the trigger falsification (§5.3, #427)
established that a Freeside-invoked reviewer can review either
surface, and that record stands unchanged. The fork text is preserved
verbatim in revision 25's entry in docs/history/decisions.md as the
considered-and-rejected shape.

## Falsification Context

The resolution was made the day real-backlog use began, against
observed behavior rather than projection:

- First production run record (2026-08-05):
  <https://github.com/freeside-ai/freeside/issues/482#issuecomment-5194682158>
  — the run failed closed at the publication gate on genuine
  verification evidence; no PR was opened; review was never reached.
  The publication gate stopped a failing candidate before any PR
  existed: evidence for the decision-queue and
  expensive-post-publication rationale, not proof of the implemented
  order — the outcome is consistent with either anchor.
- Root-cause addendum:
  <https://github.com/freeside-ai/freeside/issues/482#issuecomment-5195225910>
  — the failure was structural (implementer/verifier environment
  asymmetry, #522), strengthening the case that the expensive
  correctness work belongs before anything reaches the forge.

The stage as landed (#427, closed 2026-08-04 via PR #490) reviews the
published PR — the PR-anchored shape under the then-open fork. The
implementation re-anchor is tracked as #527.

## Companion Capability: External Review Response (#524)

Review activity arriving on a published PR from outside the control
plane (human maintainers, native Codex when it fires, other bots) is
one category: identity-gated by an external-reviewer allowlist in the
repository's trust profile, normalized into the finding pipeline with
source provenance, driving the standard remediation → reverify →
re-review cycle with thread-anchored disposition replies, bounded by
the same convergence policy as internal rounds. It never satisfies the
§7 requirement. It shares a re-entry-after-terminal-state trigger
shape with #502; the spine serializes them at scheduling.

## Revisit When

Real usage shows the owner cannot trust review they did not watch —
review passes read from disposition history and the run timeline stop
being credible without the live thread. The fallback is the
PR-anchored shape recorded in revision 25's fork text. The #525
disposition-history slice is the mitigation to exhaust before
reopening the fork.
