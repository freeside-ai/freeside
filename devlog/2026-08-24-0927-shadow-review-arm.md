# Shadow Review Arm (#846)

Chose one observation-only production arm spanning selection, execution,
classification, attention, and observability over splitting the work at the
estimated size boundary. These surfaces share one safety ceiling: shadow work
may measure and escalate, but it must never become routed review authority or
silently permit a credible P0/P1. The owner's direct implementation assignment
provided the size-disposition fiat, and no new domain, store, API, or attention
schema contract was needed beyond #838 and #842. The context-specific attention
policy added during review is recorded below.

Chose a stable hash of run and routed round with the resolved
`telemetry.shadow_review_rate` override, falling back to the composed daemon
rate, over random selection. The same routed evidence is therefore selected
consistently across retries and restarts. Because #702 fingerprints include the
finding source, cross-source comparison projects each shadow finding to the
routed source before computing the comparison fingerprint; persisted shadow
evidence retains its real source.

Chose a 15-minute observation window measured from routed-review completion
over allowing pending shadow work to hold the routed publication loop without
a bound. A late or stuck shadow invocation, including an unpersisted terminal
result first observed at or after the deadline, is abandoned as a transient
shadow failure and its workspace is removed; routed evidence and readiness
continue unchanged. The engine forces the review source's existing
mismatched-authority teardown contract before every post-start abandonment,
including inspection, polling, returned-result, and pre-inspection authority
failures. A transient teardown response keeps the shadow pass incomplete for
retry, so the bound
cannot strand a credential-bearing runtime. A completed result still passes
provider, configuration, instruction, cost-owner, base, head, finding, and
request-authority gates before persistence.

Chose one deterministic attention item per shadow invocation for credible
P0/P1 or second-adjudication findings. An unresolved P0/P1 blocks ready, while
lower-severity classifier disagreement remains telemetry. This preserves the
human safety ceiling without converting shadow findings into the routed
finding-remediation loop.

Chose #912's separate, exact `claude_local` approval over reusing the routed
profile's singleton `Review.ConfigDigest`. The effective approval digest wraps
ward's provider/runtime digest with the configured sampling rate, while the
inner digest remains the value re-authenticated by invocation results and
replay. This keeps rate changes owner-approved without changing routed v6
authority or falsifying ward's runtime-digest contract.

The Claude configuration uses a file-seeded setup token over environment
delivery. The setup-token auth mode and durable auth identity are digest-bound;
the token path and bytes are credential material and remain absent from the
configuration digest, launcher environment, logs, and preflight manifest.
Preflight mirrors runtime's private-file and token-shape checks and forbids both
routed and shadow credential paths as host-instruction sources; runtime repeats
the checks at launch and remains authoritative.

The refute-first review confirmed provider/result identity re-gating,
source-specific finding validation, immutable evidence joins, workspace cleanup,
credential separation, and the distinct approval boundary. It found and closed
the unbounded-pending, preflight-credential, routed-singleton-approval, and
unapproved-rate gaps above. A repository-wide shadow-path sweep confirmed that
startup and preflight use the separate exact gate, disabled composition remains
ungated at rate zero, recovery is constructed only after that startup gate, and
durable shadow observation/replay retains ward's inner runtime digest rather
than projecting routed authority. No additional reachable credential leak,
returned-object trust, routed-authority, doctor, or observe-projection bypass
remained.

The fresh independent pass confirmed one preflight ordering bypass: preflight
reported an absent shadow approval but continued into setup-token reads,
repository observation, image probing, and credential checks. Preflight now
derives and checks the separate approval first. When enabled authority is
absent, stale, or mismatched, every credential-, repository-, instruction-,
seed-, and runtime-touching check is `not_run`; credential shape is inspected
only after the approval succeeds. Regression coverage records zero protected
environment calls on the refused path.

The same class recurred once inside the concrete database inspector. Root cause:
the first sweep treated `InspectDatabase` as one approval lookup, although it
also performed identity, project-image, and re-enrollment probes after recording
the approval error. The widened sweep enumerated every operation after the
separate gate, moved the inspector's return to the denial boundary, and added a
real-store regression proving those later fields remain untouched. The outer
and inner refusal paths now agree.

The refute follow-up then exposed why an early return alone was still too
narrow: #912's gate is repository-aggregate, while the configured target's
canonical name and numeric identity are a separate prerequisite. With owner
approval to correct the misplaced boundary, the inspector now has one explicit
enabled-shadow authorization phase requiring both target-profile identity and
the exact aggregate approval before any later probe. Real-store cases cover
both absent approval and a wrong target numeric identity even when the global
approval itself is valid.

The final lens found one post-transaction query outside that phase. The
authorization verdict is now explicit inspection state, not inferred from the
absence of downstream errors, and gates the complete post-transaction
re-enrollment phase as well. Refusal regressions require that state to remain
false, so a prohibited query cannot hide behind an empty result.

Automated review then found two reachable boundary failures: the disabled arm's
configured fallback rate made daemon composition look partially enabled, and a
resolved dispute did not distinguish an operator's `Stop` command from an
affirmative resolution. The composition now zeroes the rate when no shadow
source exists. Review of the first command-reconstruction repair exposed the
terminalization gap resolved by the final policy below.

A later review found that an immediately completed shadow pass could compare
before routed adjudication had produced classifier annotations, permanently
freezing a timing artifact as an `indeterminate` accuracy sample. The sampled
comparison now classifies the routed side through the same live-or-conservative
annotation path before writing its immutable sample. This moves an advisory
annotation earlier without routing a shadow finding or granting the classifier
transition authority. Mutating stored samples was rejected because their stable
join and immutability are the evidence contract; merely deferring the write was
also rejected because a terminal routed disposition need not revisit the shadow
pass.

The shadow dispute now carries every attention-triggering finding as a
digest-bound, head-bound agent claim instead of asking for approval against a
generic reason alone. The claim preserves the finding ID, severity, location,
summary when distinct, and full reviewer text; legal oversized text is split
on UTF-8 boundaries into separately bound chunks rather than truncated. The
existing app claim renderer therefore presents the dissent without a schema
change. Open pre-repair items reconcile reason, actions, claims, and derived
digests in one version bump, invalidating prepared commands; terminal items
with a mismatched presentation fail closed instead of retroactively rebinding
an accepted decision. Readiness reconstructs the exact creation predicate and
expected claims from the persisted shadow record, findings, and classifier
annotations before honoring Approve or Stop; a second-adjudication case cannot
drop from the gate or change the approved claim set during reconstruction.

A later review exposed that parking `Stop` was itself incomplete: `Discuss`
does not conclude a dispute, while the only concluding action made the card
non-actionable without finishing its queued publication task. The owner chose a
shadow-specific action subset over splitting the safety fix: ordinary routed
disputes retain Adjudicate, Discuss, and Stop, while shadow disputes offer
Approve, Discuss, and Stop. Signet and the app fixture carry the union, but each
created item exposes only its context's subset. Readiness reconstructs the
immutable command history and accepts only one item-bound, terminal Approve;
Approve affirms continuation without routing the shadow finding. Stop takes the
existing definitive trust-block transaction, whose shared reason Signet already
recognizes, which terminalizes the queued task and leaves the ordinary durable
`publish_blocked` operator surface. P0/P1 findings, including the classifier's
critical/high low-confidence ceiling cases, use the same gate. A separate
contract unit was rejected because this is a context-specific safety-policy
decision with no new domain enum or type, migration, StageDriver, ReviewSource,
RunnerBackend, or API schema contract, and leaving it split would keep this
PR's own safety acceptance unsatisfied.

## Revisit When

A provider-neutral durable record for pending shadow attempts is introduced,
or the Claude runtime's setup-token snapshot contract changes. The observation
window and mirrored preflight validation should then move to that shared
contract instead of remaining arm-local.
