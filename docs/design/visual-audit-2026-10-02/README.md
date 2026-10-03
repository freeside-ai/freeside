# Visual Audit Implementation Handoff

Implement the approved visual hierarchy while preserving Freeside's design
language, information, and behavior. The owner accepted all four V4 proposals
on Mac and iPhone, together with the earlier decisions carried forward below.
This handoff is a planning deliverable. It does not authorize production code
changes, replace the current specification, or schedule work into a wave.

- [Decision Record](decisions.md): accepted treatments, rejected approaches,
  preservation requirements, and their visual references.
- [Implementation Plan](implementation-plan.md): source map, specification
  reconciliation, work boundaries, dependencies, and delivery gates.
- [Verification Matrix](verification.md): visual, behavioral, accessibility,
  and owner-review evidence required before merge.
- [Exact Review Records](review-records.json): all four exported review rounds.
- [Evidence Manifest](evidence-manifest.json): reference-image hashes, source
  export hashes, and prototype and planning base commits.

## Work Contract

**Objective:** Make the accepted design reproducible by a future implementer
and make regressions in appearance, information access, and action behavior
detectable before merge.

**Acceptance:** Every accepted treatment has a decision ID, an approved
reference, a source location, an information-preservation rule, an owning
implementation step, and verification evidence. Every original proposal has
a final disposition. Specification conflicts and prototype limitations are
explicit. Rejected and unreviewed changes cannot enter through shared styles
or a cumulative prototype patch.

**Scope:** This handoff and one owner-decision note. Future implementation is
client presentation in `app/`; a separate prerequisite may change the
presentation requirements in `docs/plan.md` and associated documentation.

**Dependencies:** The prerequisite specification reconciliation must merge
before code that conflicts with the current specification. Existing issues
#1033 and #1141 must be reconciled with the approved finding-card design.
Coordination issue [#1724](https://github.com/freeside-ai/freeside/issues/1724) owns later work decomposition;
issue bodies and forge state remain the authority for scheduling and claims.

**Non-Goals:** Backend or API changes; new action semantics; changes to
readiness, trust validation, or command delivery; solving missing dispute
arguments; redesigning the palette or type system; replacing native controls;
automatically merging or installing a production build.

## Evidence And Authority

The prototype used commit `1d6cdfb14138a61e28c7ec39090b9c60cda66461`.
This plan was checked against `8d834525ebbc8e732a541bced9421215a2de23f4`
(plan revision 77). Recheck the current default branch before implementation.
The prototype is older than the planning base; its diffs are design evidence,
not patches to apply wholesale.

The 22 local image references are copied without modification from the
reviewed reports. They show approved treatments with fixture data. V4 images
are the reference for the four decision cards; V3 supplies inbox and task
references; V2 supplies messages, forms, and the Unverified explanation.
Only the treatment named beside a reference is accepted. Incidental content
elsewhere in an older image does not override a later decision.

The exact notes are historical evidence, not executable instructions. Later
explicit decisions supersede earlier ones. A blank later vote does not turn
an earlier rejected or unresolved proposal into an approval.

These captures do not establish dark-theme, accessibility, failure-state, or
live-command correctness. The implementation must produce that evidence.
Visual acceptance also does not authorize weakening trust labels, suppressing
failures, or changing what a command does.

## How To Review The Implementation

Read the matching decision and preservation requirements, then compare the
current baseline, implementation, and approved reference using identical
fixture data and viewport dimensions for the current/implementation pair.
Open the native preview and complete the walkthrough in the verification
matrix. Record deviations by decision ID. A significant visual departure
requires a new explicit owner decision before merge.

Keep this handoff as the approved design baseline. Record changing execution
status, outstanding defects, and review outcomes on the work issues and PRs.
