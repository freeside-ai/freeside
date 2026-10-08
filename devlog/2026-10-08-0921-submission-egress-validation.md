# Submission Egress Validation

Issue #1771 refuses malformed egress policy at project-key intake. Chose one
engine helper that calls the existing domain readers over a new parser or
validation inside admission. A malformed declaration cannot become usable
later because a run's policy is immutable; the operator needs the error
before that run is stored. Reusing the readers keeps their error identities,
canonical spelling rules, and policy digests unchanged.

## Validate Every Declared Set

Chose to read `execution.registry_set` independently of
`execution.egress_profile`. Image rebuilding consumes the set even when the
writer uses `provider_only`, so accepting malformed unused writer policy
would postpone the same error to a different consumer.

This refines intake without changing the admission decision recorded in
`devlog/2026-10-05-1731-registry-set-policy-keys.md`: admission still ignores
the set for a writer using `provider_only`. Existing stored policies and
their rebinds retain that behavior. An absent profile still means
`provider_only`; validation inserts no default key.

## Keep Syntax Separate From Enforcement

A valid `provider_registry` declaration passes intake even when the
composition cannot enforce it. Admission owns that verdict, which can change
as the composition gains enforcement. Host validation remains syntactic;
the operator decides whether a public name is the intended package registry.

The six entry-point checks reject fresh work before retaining a CLI submission
or writing new policy bytes, artifacts, or runs. Client submission keeps its
earlier source-artifact registration inside the accepting transaction. Label
intake keeps its occurrence allocation; rejection leaves that occurrence
unadmitted.

Historical replay is a lookup, not a new intake. When a malformed declaration
comes from an exact stored manual submission, legacy run, or pre-bind label
reservation, the boundary preserves that immutable record. A journal alone,
or any mismatched stored policy, remains new work and is refused before policy
bytes are written.

Preflight follows the same distinction: fresh composition input validates
egress syntax. The documented empty identity mode inspects a retained legacy
run, while retained modern sessions can carry their prior nonempty identity.
For the latter, preflight opens the authenticated rig database read-only only
after egress validation fails and accepts only an exact stored resolved-policy
match at the derived specification run. The run/policy digest binding and
foreign key make that record proof of the immutable policy, without letting a
reused identity authorize fresh work.

## Revisit When

A new project-key intake path appears, or the domain readers change their
accepted declarations. Policy rebinds from existing runs remain outside this
intake gate.

## Refute-First Findings

An independent reviewer found no reachable bypass in the six original
project-key intake paths. The other production policy constructors rebind
existing keys, recover recorded submissions, or construct fixed fixtures.

- **Disproved by checks:** A malformed set with an absent or `provider_only`
  profile passes intake. Both declarations invoke the independent set reader.
- **Disproved by checks:** CLI apply bypasses file validation, or a refused
  submission leaves a new run, resolved policy, policy artifact, or policy
  blob. Boundary regressions exercise consistent request bindings and inspect
  those durable records after rejection.
- **Allowed by the issue contract:** A valid opt-in can still hold at
  admission if the composition cannot enforce it. Existing malformed runs
  keep their immutable policies and are not repaired by this change.
