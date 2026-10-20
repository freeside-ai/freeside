# Close Abandoned Workflow Episodes

The owner chose administrative workflow closure in #1993 on 2026-10-20.
Abandonment must stop asking for decisions and prevent further work or
publication without claiming that an already-running provider stopped.
Confirmed Stop remains the operation that requests cancellation and proves
quiescence. The merged #1985 decision note remains historical.

Closure belongs to an exact admitted-start ordinal and its authenticated run
membership. Task or campaign identity alone would fence a later reattempt or
let delayed old results affect newer work. Task-only notices keep a private
producing-run binding; proposal ownership comes from its stored target.
Readers corroborate copied keys, lifecycle authority, and membership instead
of trusting a deserialized closure flag.

Legacy held-work notices predate the private producing-run binding. Both
producers encoded their run in the notice ID, including the `raise-item`
form. Readers recover only those known forms and corroborate the run's stored
task and project, stable notice metadata, and the pre-upgrade canonical body.
The body cannot select an owner, and a current hold is unnecessary because
that mutable projection may have cleared. Unknown or contradictory ownership
still fails. This authenticates existing history; it creates no closure
authority and never chooses the newest start.
Independent refutation checked both historical producers, contradictory
bindings, and absent hold state; it found no remaining defect in this recovery.

Slot release, closure, and cleanup commit separately. This deliberately leaves
a visible incomplete operation if closure fails after release. It prevents a
bad card or revoked recipe from retaining the admission slot, and a committed
closure remains effective when cleanup fails. The result names committed steps
and an exact episode repair selector. Repair never retargets the newest start.
Migration creates no closure authority from old abandonment facts because
Stop and intake use the same slot-release writer.

Expired cards preserve canonical reasons, timing, and history without
fabricating decisions. The closure record explains why no decision is owed.
Authenticated late execution, review, discussion, and forge results remain
history and resource-accounting evidence, with no successor or publication
authority. Missing external evidence remains unresolved.

An explicit reasoned reattempt may use closure instead of a terminal parent.
It still needs approved implementation inputs, intact lineage, no
cancellation fence, and atomic cap-checked admission. Pending accepted
commands cannot acquire this operator-only exception.

Refute-first findings: immutable inbox replay must compare payloads, not just
idempotency keys; specification discussion retirement must consume both
authenticated markers; external review replies need their own dispatch gate.
These defects were confirmed and corrected. The earlier conclusion that
`BeginRunWork` alone closed the dispatch race was wrong: engine preparation can
continue after registration. The owner chose a separate engine dispatch claim,
serialized with live closure under the task-execution mutex. A winning claim
authorizes downstream preparation and launch without holding the mutex over
provider execution. Closure does not revoke it; retries need a new claim.
The owner declined an extra Claude adapter guard because it would restrict
this permitted window. Ward changes remain unapproved unless inference
redispatches internally. The traced `Client.Call` makes one driver attempt;
role-admission errors, driver failures, and deadlines return fallback without
redispatch. Refused claims preserve the closure sentinel
instead of fabricating fallback replies. Restart uses the durable closure
without an additional persistent claim marker.
The independent refutation pass checked claim/closure ordering, inference and
review entry coverage, retry claims, refusal retirement, and late observation;
it found no remaining defect under this boundary.
Follow-up filing and review replies serialize their durable dispatch claims
with closure through the store writer. A later closure cannot revoke that
winning claim: suppressing its send would leave a dispatch marker for a
request never sent and turn recovery into false ambiguity. Their redundant
post-claim reads were removed. Deterministic claim-order tests preserve
pre-claim refusal, authentic success and uncertain-response recovery, and
refusal of a fresh retry after a real transient rejection.
Independent refutation confirmed those boundaries and found no remaining
post-claim revocation in the adjacent publisher paths.
That correction exposed a separate lifecycle obligation: a closed workflow's
never-dispatched or proven-rejected intent must release its repository or PR
reservation. Leaving it pending refuses the old retry but also blocks later
unrelated workflows. Retirement now rechecks closure and the current claim in
one store transaction, reuses the existing terminal refusal, and suppresses
closed-work notices. A dispatching or unproven request still requires authentic
observation. Regressions follow later workflows through the same repository
and PR, and challenge retirement while a winning send is still active.
Fresh independent refutation found no unsafe release or remaining reservation
leak across those states and adjacent publication paths.
Historical observations must authenticate recorded admission
rather than current launch policy and must not block unrelated reconciliation.
The real driver's combined launch/recovery callback requires a separate
observation path. The owner expanded scope to `exec/stage` for that change.
The concrete adapter now separates observation of a confirmed-started handoff
from launch, preserving Stop registration without fabricating a terminal
failure for suppressed work. Restart tests cover both paths.

A transient review-source error must not occupy the immutable terminal-result
key. That defect was confirmed and corrected; a regression follows a transient
failure with an actual result or terminal failure. Historical review results
remain authenticated against their retained request and admission even after
configuration changes, and terminal failure usage remains accountable. The
independent refutation pass found no outstanding defects after these fixes.

Automated review found that intake can admit an already-reserved run without
writing it again. A new start could then remove that run from a closed episode.
The lifecycle-start writer now captures closure membership for commit-time
validation too. A regression reproduces the bypass and verifies that refusal
preserves the closure and slot state while a fresh run can still be admitted.

Revisit when episode membership changes, a new attention kind is registered,
or a new provider/effect adapter introduces its own dispatch or recovery path.
