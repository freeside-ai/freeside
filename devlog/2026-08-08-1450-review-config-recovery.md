# Recover Runs From Superseded Review Configurations

Chose to park a configuration-class review failure instead of terminalizing
its publication task, revising #527 decision 3 for this class only. That
decision assumed the approved reviewer configuration could always be
restored; #610 broke the assumption by making the configuration change
itself the required safety fix (the topology version rides the approved
digest, so the fix moved `cf6a0b88…` to `373c72bf…` and the pinned approval
could never match again). State-482's first production run was concluded by
exactly that interaction; an already-terminal run stays out of scope here
(#502-class re-entry), so this unit protects every future run instead.

Chose one uniform recovery route: an operator-authorized adoption of a
review-configuration-only profile supersession, on the #580/#582
transition pattern (command-backed, append-only, bound to the exact failure
row's body digest, original rows immutable). A restored configuration
degenerates into the same route (the superseding revision is the pinned
revision itself), so there is no separate restoration path to reason about,
and every resume is an explicit human decision. Rejected: engine-initiated
resume on restoration (no operator decision); refreshing the item as the
approved revision advances (sync churn; instead the adoption target is
resolved at decision time as the repository's currently activated revision,
and re-gated on every read).

The supersession gate decides by content address, not field comparison:
overlaying the superseded revision's `review.config_digest` onto the
superseding body must reproduce the superseded revision's own profile
digest. Any other delta, however encoded, fails closed; a widened profile
can never ride a review recovery. The gate runs at the signet write, on
every store read of the transition, in the engine (which adds the
run-binding and effective-configuration equalities), and in the publish
drift gate.

The publish extension was forced by verification: the drift gate requires
the candidate's authorized profile revision to be current, so an adopted
run reviewed clean and then blocked at publication. The candidate now
carries the adopted digest as a claim (`AdoptedTrustProfileDigest`), and
the gate honors it only after re-deriving, from its own trust source, that
the adopted revision is current and review-configuration-only against the
authorized revision. The durable candidate authorization stays bound to
the original revision. The trust source's by-digest read is an optional
capability interface, so existing fakes keep the strict gate.

An ineffective adoption (the adopted revision later superseded, the
effective configuration moved again, or a tampered row) grants nothing and
the run stays parked. A concluded item ends the run only when it carries
no adoption at all: the operator's explicit decline (stop) terminalizes
exactly as #527 decision 3 always did, while a concluded adopt keeps the
run parked however ineffective the recorded adoption currently is, since
terminalizing an operator rescue is the one outcome the decision cannot
have meant. The engine treats the store's binding-gate refusals as
no-adoption rather than lane errors, so a tampered row parks visibly
instead of error-looping the reconciler. The recovery-bearing item is
raised by the tick after the failure record (the failure writer lacks the
trust binding in scope); the one-tick gap is bounded by the reconcile
interval.

Verification: integration fixtures for park-without-terminal, adoption
resume through review and publication readiness, stop conclusion,
trust-widening rejection at the decision boundary, and replay stability;
store tamper/mismatch/moved-on/widening re-gate tests; signet
atomic-and-idempotent adoption; domain overlay-gate tests including
tampered profiles; app fixture, mock-server, contract-validation, and
display-row parity in the same push.

The refute-first pass produced one confirmed blocking finding and three
confirmed hardening gaps, all fixed before handoff: an adopt/tick race
(the signet conclusion landing between the engine's transition and item
reads read as a decline and terminalized an adopted run; the concluded
arm now distinguishes decline from adoption by transition presence,
deliberately authority-free, so effectiveness still gates every advance);
a premature or later-outlived adoption terminalizing instead of parking
(same presence fix); a hard-limit exhaustion item colliding with the
concluded recovery item's identity (the recovered-identity namespace now
covers configuration adoptions); and two upgrade seams (a pre-#611
terminalizing dispute item at the same identity is honored as the
contract it presented, and a legacy admission without a profile pin keeps
the old dispute path, since its recovery binding would drift under the
parked item). Rejected by execution: digest spoofing and canonicalization
drift against the overlay gate, invocation reuse and limit bypass in the
round arithmetic, riding a later failure on an earlier transition,
unactivated profiles becoming "latest", and forged adopted-digest claims
at the publish gate — each attacked and held at every call site.

Codex review round 1 confirmed one blocker the refute pass missed: the
pre-publication configuration gate consulted the adoption only while the
parked failure outranked the latest review row, so a crash between the
adopted round's persisted record and publication (or a later transient
failure) re-recorded a configuration failure and re-parked a recovered
run. The gate now falls through on the effective adoption itself, with
the exact-failure binding still gating advancement past the parked row;
an adopted-replay integration fixture pins the crash seam.

Revisit when #502-class re-entry lands (an already-terminal run could then
offer this same adoption on resurrection), or when trust profiles gain
revision lineage that could replace the latest-activation check with an
explicit successor link.
