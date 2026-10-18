# A Base Advance Before Publication Re-Enters The Run

The Wave 8 exit run on gh-imgup#117 lost a finished, adjudicated run to an
ordinary docs merge on the target branch. The publication audit refused with
`ErrTargetBaseAdvanced`, and the run ended blocked with no path forward: the
plan's base-advance re-entry covered only a published candidate. The owner
decided in #2058 (2026-10-16) that an unpublished candidate re-enters too.
Plan revision 89 records the decision in Sections 6, 7, and 13.

Chose automatic re-entry over a `publish_blocked` card action that reruns
from the new base. Both keep the adjudicated history, but the card needs a
person to act. A run that waited at a human gate is the one most likely to
meet an advance, so a person-driven rerun stays exposed to the next merge
while it waits for that person again.

Chose "exactly one re-review, of the prospective merge" as the reading of the
owner's "re-review only where the plan's re-review rule requires it". Section
7 already says an advanced base invalidates the review pass, so the rule
always requires one round. It requires nothing else: findings already
adjudicated keep their dispositions by finding identity (#702), under the
re-emission rule below, and a clean or fully dispositioned re-review adds no
round. This is the planner's reading of the owner's bullet. A narrower rule,
such as skipping re-review when the advance touches disjoint paths, was
rejected because Section 6 binds base-dependent proofs to the base, and
disjoint files do not prove that the merge behaves as the head did.

Chose to carry dispositions forward under a re-emission rule over carrying
every disposition unconditionally. A carried disposition stays in the
history, and a finding the re-review does not re-emit keeps it. A re-emitted
`fixed` identity re-enters adjudication as a failed prior fix, as Section 7
(Finding Adjudication) already requires. A re-emitted declined or deferred
identity keeps its disposition only when its adjudication's bound
specification, instruction, and policy digests are unchanged at the new base;
otherwise it is re-adjudicated, because the new base can change the
repository instructions the decision rested on. Codex review of PR #2063
found the unconditional reading in conflict with those Section 7 rules, and
the owner approved this refinement. Re-adjudicating every re-emitted identity
was rejected because it repeats decisions made under unchanged inputs.

Chose ordinary adjudication and remediation for new findings over the
published re-entry's escalation to a person. The published case cannot
silently change a head a person may already be reviewing. Before publication
no one has seen the head, so the ordinary loop applies and its round limit
still bounds it.

Chose the publication audit as the only trigger over a base watch during
human gates. A watch would detect the advance earlier and shorten the window
in which a run sits on a stale base. Once re-entry exists, it adds nothing a
run needs, and a second freshness signal is a second thing to keep in step
with the audit.

Kept the trust boundary where it was. The re-entry base comes only from the
daemon's own recorded audit tip, never from a task payload, and the store
gate that seals the authority proves that from its own records. Publishing on
the stale audit stays a non-goal.

The bound reuses the existing item. A conflicting prospective merge, or a
`hard_round_limit` with no room for the re-review, ends the run in
`publish_blocked` with hold reason `base_advanced`, its reason naming the
cause. No new card action, API change, or client change is needed.

Revisit when re-entries become frequent enough to cost meaningful review
budget, which would make an earlier base watch worth its upkeep; or when the
owner wants a narrower re-review rule than one round per advance.
