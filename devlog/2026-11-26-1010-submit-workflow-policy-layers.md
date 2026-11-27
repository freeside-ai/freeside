# Run Identity Stays With the Policy File on `freesided submit`

Issue #3183 brings `freesided submit` onto the store's workflow policy layers
so a task created from the CLI resolves the Workflow settings pane's six keys
the same way paired-client and label-intake tasks do (`project` over `general`
over `preset`). This note records why the submit path overlays those keys
inside the daemon's write transaction while keeping the run identity derived
from the `-policy` file as submitted.

## Decision

- **Chose to keep the run identity derived from the `-policy` file and overlay
  the store's workflow policy layers inside `applyLaunchSubmission`**, over two
  alternatives:
  1. **Having the daemon derive the run identity after overlaying.** Rejected
     because `freesided preflight`, `freesided prelaunch`, per-submission
     composition manifests, and saved submission retries all compute the run
     identity from the submitted input files before the daemon accepts the
     request. Moving identity derivation into the daemon would break those
     callers and strand saved journals.
  2. **Having the `submit` CLI overlay the store's layers before deriving the
     run identity.** Rejected because `submit` holds only the control client,
     not a store handle, when a daemon serves the database, and reading layers
     ahead of the daemon's write transaction would race a concurrent Workflow
     pane change.

## Narrowing of Earlier Decision Note

This narrows a statement in
`devlog/2026-11-12-2201-workflow-policy-layers.md` (Decisions: "the run
identity digest covers the overlaid keys"; Refute-First Findings: "The run
identity digest equals the stored policy's digest"). That statement still holds
for paired-client submissions (`TaskSubmitter.submit`), which derive the run
identity inside the store transaction after overlaying. On the `freesided
submit` path, the run identity covers the `-policy` file's keys and no longer
covers the overlaid workflow keys, so the stored policy's digest
(`specification_policy_digest`) can differ from the policy digest inside the
run-id derivation.

The divergence is safe because:

- Every CLI submission carries a distinct submission id (`cli:<submission_id>`)
  that already separates run identities across submissions.
- Replay and crash recovery read the stored run's existing workflow keys via
  `engine.ResolveRunWorkflowPolicyKeys`, so a repeated submission or retry
  after a layer change converges on the stored policy rather than colliding
  with it, including submissions stored before this change.
- Nothing re-derives a run id from a stored policy's digest.

## Revisit When

- A caller needs to re-derive a CLI submission's run id from its stored
  resolved policy rather than from its submitted `-policy` file.
- Per-submission composition manifests and `freesided preflight` are retired in
  favor of launch manifests only, allowing the daemon to own CLI run identity
  derivation.
