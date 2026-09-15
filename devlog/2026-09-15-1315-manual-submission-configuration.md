# Configure Manual Submission Independently Of Label Intake

Chose an operator-owned startup JSON file for #1360 over adapting label
initiators or introducing the full rein resolver. The owner assigned this
serialized contract unit. The deployment boundary recorded in the
[client-submission decision](2026-09-12-1950-client-task-submission.md) left
paired clients unable to create tasks on a production host. Reusing label
configuration would couple manual submission to an unrelated workflow. A
bounded startup snapshot supplies the missing policy and attribution while
keeping the existing command and submission identity rules.

## Decisions

- Validate the entire file before serving commands. Use the existing resolved
  policy, specification policy, declared-path, and author validators. The
  specification-policy parse is also the gate in `SubmitSpecificationRunTx`;
  doing it at startup prevents accepting a file whose tasks could never be
  submitted. Reject duplicate projects even when they agree, so entry order
  cannot choose a policy. Canonicalize keys and copy lookup results to protect
  the startup snapshot from later mutation.
- Keep label initiators separate, with no fallback in either direction.
  Syntactically valid attribution remains a claim. The selected App and
  production admission still determine whether execution and import proceed.
- Keep the fetch-before-policy order. A restart can change newly created
  tasks, but replay and same-source commands retain the original name, task,
  specification run, and immutable bindings. Reject re-resolving those inputs
  on a retry because it would move an existing identity.
- Retain bytes and an explicit presence/digest record, rather than only a
  path. Normal resume uses the snapshot. A reviewed restart may replace or
  add the file; an interrupted upgrade must reuse its recorded bytes and
  presence. Absence preserves older sessions' behavior. Keep the new input
  out of the old binary's preflight arguments and add its digest to the new
  upgrade receipt without changing receipts for absent configuration.

## Refute-First Verification

The loader and paired HTTP tests challenge malformed input, unknown-project
rollback, mutable lookup storage, and changed/removed configuration across
restarts. Script tests challenge lost or altered retained bytes, original-file
edits, optional-input addition during interrupted recovery, and compatibility
with sessions that have no manual configuration. Independent review found one
reachable recovery defect: a failed retry could inherit an interrupted upgrade
without inheriting the restriction against changed manual configuration.
Fixed by checking both `runtime-upgrade-started` and
`runtime-upgrade-inherited`; regression cases reject replacement and addition
through the inherited path.

Disproved: lookup mutation changes the snapshot (all nested fields are values
and both slices are copied); duplicate JSON members override bindings (the
existing duplicate detector rejects them, including case variants); claimed
author metadata bypasses authenticated App authority (production rechecks it);
ordinary retained-file changes or deletion go unnoticed (receipt checks refuse
both). Kept the existing domain definition of provenance validity: a
nonempty digest with a registered source. This file does not create a new
approval-provenance format or grant authority from that digest's spelling.

## Revisit When

The daemon gains a complete rein/workflow resolver, needs live configuration
reload, or routes a single running production composition across repositories.
This file supplies policy at task creation; it does not provide those systems.
