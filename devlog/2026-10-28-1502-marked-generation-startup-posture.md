# Keep Credential-Integrity Holds On Their Own Generation

The owner decided on 2026-10-27 that a marked credential generation holds
only its own identity's admissions. The decision is recorded in
[issue #1740](https://github.com/freeside-ai/freeside/issues/1740) and
[Wave 9 tracker #2386](https://github.com/freeside-ai/freeside/issues/2386).
This supersedes the wider startup posture described in
[the #1624 note's Posture section](2026-10-03-1444-credential-integrity-mark.md#posture),
which held the entire lineup for a marked writer and stopped an unattended
start for a marked reviewer.

Chose per-attempt refusal over a startup hold because a damaged subscription
must not prevent a healthy second subscription from admitting. Startup skips
a marked writer role and composes marked review sources from their checked
lines. Every writer attempt and review admission still refuses the marked
generation. Re-enrollment appends an unmarked successor, so the same running
daemon can admit again without a restart. Startup raises no additional
`agent_selection_inactive` item for a mark and resolves such items left by
earlier starts. The probe's advisory `credential_integrity` item remains the
operator's notice.

The review check reads the mark and generation in one transaction, then runs
all other review checks before reporting the mark. A mark never conceals a
prompt, provider, expiry, or launch failure that would still stop startup.
Only the marked-generation error comes with a populated checked agent for
composition; any non-nil error still refuses admission.

Two limits remain explicit in the issue contract:

- Preflight still fails on a marked generation and blocks a deputy launch
  and submissions until re-enrollment. Its remedy names the enrollment to
  re-enroll, rather than incorrectly asking the operator to adopt the tree.
- A marked writer fails during resolution, so startup skips its later
  prompt, launch-coverage, expiry, offer, and egress checks. After
  re-enrollment, a later fault refuses an attempt rather than holding the
  lineup at startup. Writer admission keeps its existing refusal order.

## Refute-First Findings

- Disproved: composing a marked review source permits admission. Tests cover
  both review roles and verify that `AdmitReview` returns no snapshot and
  the source's launch check refuses. Independent review traced every
  `CheckReviewRole` caller and confirmed that only startup accepts its
  checked agent alongside the marked error.
- Disproved: a mark conceals a review configuration fault. Tests combine a
  mark with prompt, offer-expiry, and launch failures; each returns the
  other refusal and a zero agent.
- Allowed by the owner decision: preflight remains blocking and a marked
  writer's later checks wait until re-enrollment, as described above.

## Revisit Conditions

Revisit if preflight must permit a degraded launch or writer startup must
report later faults on a marked generation. Either changes the acceptance
contract, rather than merely changing the mark's remedy.
