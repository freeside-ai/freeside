# Controlled Task Cancellation Check

A Stop receipt proves that the daemon accepted a durable fence. Only a matching
`confirmed` acknowledgement proves that owned work ended and the held WIP
episode was released. `requested` and `failed_to_stop` keep that episode held.
Provider cancellation, a resolved card, a timeout, and a terminal run display
are insufficient on their own.

The native Claude judgment adapter joins its CLI and observes the exact owned
process group before recording quiescence. A daemon crash before recording
that proof remains uncertain. Do not replace missing proof with a guessed
process sweep or edit an acknowledgement into the database.

## Prepare A Controlled Run

Use the retained, private environment described in the
[production walkthrough](production-walkthrough.md), including its existing rig
ownership, authenticated pairing, approved image, and provider prerequisites.
Use an expendable task whose provider and forge effects you are authorized to
exercise. Leave the production harness lifecycle instructions unchanged.

Record the daemon commit, task/project IDs, database sync epoch, start ordinal,
captured run IDs, reconcile interval, and Stop command ID. Create the task after
the daemon initialized `task-runtime-coverage.json`. Tasks already present at
that checkpoint cannot acquire missing runtime evidence retrospectively.
Keep the state directory and database together across the restart case.

Use an existing specification question or approval card's Stop action on the
paired client. The daemon adapter submits the same task fence in the card
command transaction. For a direct `/commands` exercise, use the authenticated
client's `stop_task` command with the exact current task snapshot version and
sync epoch. Reuse that command ID only for exact replay. Never record device
credentials in a transcript or evidence bundle.

## Exercise The Boundaries

1. **Queued work:** Stop before dispatch. Verify that no invocation starts,
   confirmation invents no task start, and another task remains admissible.
2. **Active work:** Stop during specification or implementation, then repeat
   with review and verification. Record request acceptance and first runtime
   cancellation observation. Discovery should occur within the configured
   reconcile cadence in a healthy daemon, independently of another task's
   provider or teardown. A worker may take up to two minutes before the daemon
   records its bounded failure; a child that ignores cancellation must retain
   WIP and must not accumulate concurrent stop attempts.
3. **Restart:** Interrupt the daemon after Stop acceptance, preserving its
   database and private ownership journals. Resume through the existing
   walkthrough recovery procedure. Verify the same fence remains and no
   queued invocation resumes. A missing session, interrupted host command,
   changed epoch, or missing ownership record must stay unconfirmed.
4. **Publication:** Stop before the publication intent, before a forge write,
   and while an already-entered write completes. Verify no later branch/PR
   repair occurs. A returned or exactly rediscovered PR retains its binding.
   A missing external result remains unresolved; Stop never closes the PR.
5. **Replay and late results:** Replay the exact Stop command and retain a late
   result if one arrives. Verify the original receipt is unchanged, no
   successor starts, and repeated reconciliation adds no lifecycle fact or
   sync revision once settled.

## Record Runtime Evidence

Keep these observations with the controlled run's private evidence:

- The accepted request and its exact target digest, plus the final
  acknowledgement and its evidence digest. The daemon retains a bound inventory
  artifact; concrete ownership and teardown records remain in the state root.
- Every captured run's stage attempts and native/shadow review requests,
  including children that finished before Stop. Inspect their ward journals
  and exact owned resource identities. A successful delete call alone is not
  absence evidence.
- Verification records under `task-verification/`: the exact owner label and
  CID, host-command join result, and subsequent owned-container absence. An
  interrupted command without a join record remains uncertain even if no
  container is visible.
- Judgment records under `task-judgments/`: actual provider entry, return, and
  the concrete native adapter's quiescence result. A successful operation and
  a successful process-group join are separate facts.
- Any pending publication intent and exact matching forge outcome. Preserve
  the PR URL and head; do not retry a forbidden write to obtain evidence.
- The task's WIP state and another task's admission after confirmed release.
  A failure state retaining WIP is truthful but does not pass the confirmed
  release scenario.

Inspect only recorded owned resources. Do not kill by process-name guesses,
delete a shared namespace, remove private journals, or use administrative
abandonment as evidence that execution stopped. Review evidence for credentials
before sharing it; private runtime files are not public PR attachments.

## Hermetic Verification

From the repository root:

```sh
go -C daemon test ./internal/engine ./internal/exec/stage ./internal/ward ./internal/publish ./internal/integration -run 'TestTaskCancellation|TestCancel'
bash scripts/check.sh daemon
bash scripts/check.sh docs
```

The dedicated integration fixture uses the durable fake driver. It verifies
live cancellation and refuses confirmation when a reopened driver has lost its
active session. These checks do not establish real-provider termination.

## Pinned Native CLI Check

This separate opt-in fixture runs the actual native CLI against a localhost
provider that keeps its response open. It uses synthetic credentials and makes
no paid provider call. A test-only exec shim supplies the local endpoint; both
that shim and the real CLI are verified private copies. Production has no new
endpoint override.

Set `FREESIDE_JUDGMENT_CLI` to the installed executable and
`FREESIDE_JUDGMENT_CLI_SHA256` to its approved SHA-256 pin, then run:

```sh
FREESIDE_TASK_CANCELLATION_LIVE_TEST=1 go -C daemon test ./internal/integration -run '^TestTaskCancellationPinnedNativeCLI$' -count=1 -timeout=2m -v
```

The fixture waits for actual provider entry, refuses confirmation while the
call is active, cancels the owned task context, and requires retained process
group exit proof after reopening the journal. Keep its output and executable
pin with the verification record. It does not exercise Apple container cleanup,
real subscription service behavior, or the complete paired-client workflow.
Record any unavailable full real-run check separately as **Not run**.
