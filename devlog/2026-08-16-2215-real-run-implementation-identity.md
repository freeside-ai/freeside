# Real-Run Implementation Identity

Issue #802 changes the operator-facing environment contract between the
production acceptance script and its live verifier.

## Decision

Chose the explicit `FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID` and
`FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION` pair over compatibility aliases
for the generic former names. The harness scrubs the former names, while a
manual verifier run fails fast when either is present. Both new values must be
non-empty and present together, and the invocation's admitted run must match
the bound implementation run. Environment shape validation precedes every
durable precondition write; admission lookup performs the later cross-lane
check.

This makes the implementation lane visible at every boundary and turns stale,
partial, or cross-lane operator bindings into an immediate diagnosis. The
elaboration run remains the subject owner for specification-approval
attention; the verifier's implementation binding does not change that
contract.

## Rejected Alternatives

- Keeping either generic name would preserve the ambiguity the issue removes.
- Accepting the former names as aliases would let stale operator recipes keep
  expressing an unnamed lane and could silently select or skip the wrong run.
- Inferring the implementation run from the invocation without checking the
  supplied pair would hide a cross-lane operator mistake instead of reporting
  it at the verifier boundary.

Revisit when the production verifier no longer supports direct manual use or
the submit contract provides one typed implementation-identity object that can
replace this environment pair atomically.
