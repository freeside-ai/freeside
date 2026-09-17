# Production Walkthrough And Recovery

`scripts/run-real-work.sh` keeps the production daemon and its rig holder alive
after publication and final verification. The same database, state root and
listener remain available to the paired Mac and iPhone. The production deadline
ends at publication; the walkthrough has no automatic deadline or stdin prompt.
Closing the shell is an interruption, not a request to complete the walkthrough.

## Start And Verify

Use the harness header's required environment and input files. The three
trusted prompt packages ship in the repository, so no operator authors one:
pass `prompts/phase-1a/implementer.md` as `FREESIDE_REAL_RUN_PROMPT_PACKAGE`,
`prompts/phase-1a/specifier.md` as
`FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE`, and
`prompts/phase-1a/remediator.md` as
`FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE`, each from the approved
default-branch commit. The daemon admits the content digest of these exact
bytes; a workspace copy is never prompt authority.

Before acquiring
the rig, stop the supervised daemon through the installed Freeside menu. If
using launchctl for an already registered service, the equivalent suspension is:

```sh
launchctl disable "gui/$(id -u)/ai.freeside.daemon"
launchctl bootout "gui/$(id -u)/ai.freeside.daemon"
```

Keep the harness shell open. It prints the retained session directory, then the
implementation run, invocation, endpoint and exact completion command. Sessions
default to `~/Library/Logs/Freeside/real-work-session.*`; set
`FREESIDE_REAL_RUN_DIAGNOSTIC_DIR` to an existing directory outside the source
checkout to use another destination. Each session is private and retains the
built binary, submission input copies, rig acquisition material and diagnostics.

Each new session saves `submission-id` before preflight and passes that identity
to both preflight and submit. Identical input in another new session creates
separate work. If submit's output is lost, its private journal is under
`<database>.submissions/<submission-id>.json`; manually resolve it with
`freesided submit --db <database> --retry-submission-id <submission-id>`.
This loads the saved inputs and never creates a replacement identity. Neither
the CLI nor the clients automatically resend unresolved submissions. Existing
retained-session attachment performs no submission. Sessions predating the
identity field keep their legacy composition checks.
Do not upload the directory: acquisition material and operator inputs are private.

Before startup, the verifier seeds the two required auth identities. After
publication, it opens the existing database read-only, checks the recorded
identities, and rechecks policy, evidence and the exact published head. It does
not migrate the database or create missing backup keys or artifact directories.
Both a successful exit and the explicit verification success marker are required.

If pairing has not finished before the startup code expires, keep the session
running and use its retained binary to request a fresh code as the daemon's OS
user:

```sh
/absolute/session/freesided pairing-code \
  -state-dir "$(cat /absolute/session/state-root)"
```

Use the active campaign's state directory and the returned `api_url`, not the
normal installed service's endpoint. The command prints a private code and its
ten-minute expiry. Keep that output out of shared logs and evidence; record a
redacted mint/preview receipt and successful client pairing instead. Renewal
does not consume or create a workflow attempt, restart the session, or authorize
another submission. Any campaign-specific minting budget still applies.

During the walkthrough, fetch the printed run on both paired clients and exercise
its offered ready-card actions. Record which actions actually ran, the run and
invocation IDs, endpoint, and the published head that final verification checked.
Later actions can change the run, so the earlier verification does not prove a
later head. Health from the default service on port 7331 does not prove this run
is still available at its paired endpoint.

## Replace A Retained Runtime

A runtime upgrade uses a fresh rig acquisition after the old session releases
its ownership. First run that session's printed `complete` command (or `recover`
after an interruption), finish the requested app restoration actions, and verify
its status is `completed`. Do not transfer a live lease, replace its binary in
place, delete its rig manifest, or replay the original submission as Retry.
An interrupted upgrade may instead remain `recovery-required` after `recover`
has verified rig release. That session can be resumed directly with the command
below; its release proof and upgrade marker are required. Do not change its
status file to claim completion.

From a clean checkout of the reviewed replacement, export the same required
configuration as the original run, including its state root, seed root, listener,
base, images and review settings. Suspend the supervised service as described
above, then run:

```sh
bash scripts/run-real-work.sh --resume-session /absolute/old-session
```

The harness requires the existing database and binds the retained run and
invocation to the original submission and composition manifest. It builds a new
daemon and matching verifier, acquires a fresh rig, preserves a copy of the
latest encrypted checkpoint before opening the database for migration, and
uses the retained binary's non-migrating preflight to check approved inputs before
the replacement can migrate or seed the database. It checks the new composition
again afterward. Original composition, submission receipt and run IDs are saved
before migration or identity seeding, so a refused or interrupted upgrade remains
resumable. It does not submit work or issue a client command.
Pairing, signing material, approvals, attempts and prior session evidence remain
in their existing locations. The new session records its predecessor.

### Retain Client Submission Configuration

`FREESIDE_REAL_RUN_MANUAL_SUBMISSION_CONFIG` is an optional absolute path to
the operator-owned JSON described in the
[daemon guide](../daemon/README.md#configure-client-task-submission).
On a fresh run, set it explicitly to enable new client tasks. The harness
copies those bytes to `submission-inputs/manual-submission.json` in the private
session directory, records their digest and presence in
`manual-submission-input.json`, and passes the staged path through the
daemon-only `-manual-submission-config` flag. It never derives this input from
the seed task's policy, publication file, or database.

A normal `--resume-session` with the variable unset reuses the retained bytes.
Edits to the original operator file have no effect. Sessions that predate this
input, and sessions created without it, remain disabled. Missing or changed
retained bytes fail the resume instead of silently dropping configuration.

To add configuration to an older session or replace it, complete/recover and
restore the prior session as described above, review the new file and current
build, then set the variable explicitly for `--resume-session`. To disable
new submissions, supply a valid file with an empty `projects` array. The
restart records a fresh input receipt; it does not change existing task or run
bindings. The old binary's composition preflight receives no new flag. An
interrupted upgrade must retry the same build and inputs, including the
manual file's presence and digest. It refuses a replacement, addition, or
missing file during that retry. Configuration is also rechecked immediately
before the new daemon launch.

### Verify The Resumed Endpoint

Startup checks the listener's build and an authenticated retained publication
checkpoint, including the remote PR head through the operator's `gh` access.
A failed or pending feedback attempt may leave the previous ready item
superseded. That historical checkpoint permits resuming the walkthrough but
does not count as a completed successor publication. A failed restart uses the
same checked rig cleanup and release path as an ordinary session. Once the
upgrade has opened writable, it never rolls back the database or restores an
unchecked older service. This protects startup writes and preserved evidence;
the saved encrypted checkpoint is recovery evidence, not an automatic rollback.

A dismissed current ready card can also supply a retained historical checkpoint.
Resume restores access to the same run and decision history while the card stays
dismissed. It does not grant a Retry, return-to-agent, or publication action.
For incomplete history, the remote PR must still be open at the authenticated
head. Stopped cards do not qualify as dismissed history. Preserve the successful
publication checkpoint from before
dismissal: the ordinary `verify` command still requires an open ready card and
will refuse the dismissed one.

A completed work unit instead supplies a `completed` history checkpoint. The
verifier re-derives its durable completion from the declaration, published binding,
and recorded merge/issue fact timelines. It checks that the remote PR is merged
at the same accepted head and recorded merge commit. A later issue reopen does
not erase an authenticated earlier completion. This restores access to completed
history without granting submission, Retry, execution, or publication authority.
The ordinary `verify` command continues to refuse completed work; the retained
startup check establishes history restoration, not a new publication acceptance.

Before completing the upgraded session, install its exact reviewed daemon using
the existing Mac installer, with `--daemon-path /absolute/new-session/freesided`.
The restoration gate compares the installed daemon's Go build ID with the retained
binary, opens the retained database read-only with the matching verifier, and
checks that the restored service reports the same build. Rebuilding separately
may produce a different build ID; supply the retained binary to the installer.
Go and the installer prerequisites remain required. The default installed binary
is `~/Applications/Freeside.app/Contents/Resources/freesided`; for another install
location, set `FREESIDE_REAL_RUN_RESTORE_DAEMON` before completion, or pass the
installed daemon path as the third argument to recovery:

```sh
bash /absolute/new-session/real-work-session.sh recover /absolute/new-session \
  /absolute/Freeside.app/Contents/Resources/freesided
```

A refused build, schema or health check leaves `recovery-required`. After installing
the session's daemon, repeat `recover`. Alternatively, resume that released session
from the same reviewed source version with unchanged inputs. Before migration,
the harness records an atomic receipt containing that version and a digest of
the configuration and input contents, including prompt and review snapshots.
Matching that receipt allows an interrupted migration to finish even if its
intermediate schema cannot pass preflight. Changed versions or inputs are refused;
full composition verification still gates daemon startup after migration.
Original approved composition remains separate from observations. Neither route
deletes a marker manually or replaces the database. Preflight may write authority
audit records under an existing schema; it does not migrate or seed identities.

Once the retained endpoint is available, use the supported client Retry action
on its execution-failure card when offered. After the workflow publishes a new
ready result, run the new session's printed verification command:

```sh
bash /absolute/new-session/real-work-session.sh verify /absolute/new-session
```

This read-only check requires the current open ready item, its authenticated
producer/export and publication outcome, a review covering that head, and an
open remote PR with the same repository, number, branch, base ref and head.
Superseded cards remain history. Each invocation preserves separate diagnostics;
a premature or failed verification returns nonzero and leaves the daemon
available for recovery. Only a successful `ready` checkpoint and remote check
establish publication acceptance. Verify both paired clients separately against
that run and head, then complete the new session deliberately. Fixture results
alone do not establish live replacement or client acceptance.

## Complete Deliberately

Run the exact command the harness prints in a second terminal. Its form is:

```sh
bash /absolute/session/real-work-session.sh complete /absolute/session
```

This requests completion; the foreground harness reports the result. Repeating
the request is safe. It stops its owned daemon, waits up to 30 seconds before
escalating, cleans only authenticated recorded rig resources, and requests clean
holder release with SIGUSR1 only after cleanup succeeds. Cleanup and holder
release use `FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS` (default 30).
Failed cleanup or release returns nonzero and retains recovery inputs and the
stale gate. Logs remain available after success too.

## Restore The Supervised Daemon

After release, the harness calls its retained copy of
`app/scripts/restore-supervised-daemon.sh`. It clears the launchd disable override
for the current UID and prints the app actions:

1. In **Stopped** or **LaunchAgent unavailable**, choose **Start**.
2. In **Daemon unreachable**, choose **Stop**, wait for **Stopped**, then **Start**.
3. If requested, choose **Open Login Items…** and approve Freeside.

The app owns SMAppService registration. `launchctl enable` alone does not
register a service removed by `bootout`; reopening an app with a current
registration marker can also leave it unregistered. The helper requires both:

```sh
launchctl print "gui/$(id -u)/ai.freeside.daemon"
curl --fail --silent --show-error http://127.0.0.1:7331/health
```

Registration errors, pending Login Items approval, or health timeout leave
restoration incomplete and return nonzero. The wait defaults to 120 seconds;
`FREESIDE_REAL_RUN_RESTORE_TIMEOUT_SECONDS` sets another positive bound. Retry
the helper after resolving the app error or approval. Client endpoint preferences
are never changed automatically.

These are the implementation's supported recovery steps. Repository fixtures
exercise registration and health outcomes with stand-ins; they do not prove
SMAppService behavior. Live Mac/iPhone evidence must be recorded separately in
the implementation PR before claiming the issue's full acceptance.

## Recover An Interrupted Session

SIGINT/SIGTERM makes the foreground harness stop its owned daemon and attempt
the same checked cleanup. A surviving owned holder receives clean release only
after cleanup. If the holder already died, recovery uses:

```sh
bash /absolute/session/real-work-session.sh recover /absolute/session
```

The helper runs the retained binary's
`rig recover -state-root <recorded-state-root> -confirm`, with bounded process
group cancellation. That command rejects a live holder, locked database, live
listener or resources that cannot be cleaned. Interruption of `rig hold` itself
deliberately leaves `.freeside-rig.json`; checked recovery is the expected path,
not permission to delete the manifest manually. A live holder still owned by
the harness must finish through that harness. Recovery never kills a stored PID.

If cleanup failed, inspect `rig-cleanup.log` and correct the reported condition
before retrying recovery. If only restoration failed, the saved `rig-released`
status lets recovery retry restoration without repeating resource cleanup.
Concurrent recovery attempts are refused. A helper killed with SIGKILL may
leave `recovery.lock`; establish that the helper has stopped before removing
that empty lock directory and retrying. Never remove rig manifests or clear a
gate while the daemon or recorded resources remain live.

The recovery command first removes exactly the manifest-recorded containers,
then recovers review-owned volumes and networks through their existing journal.
The full review namespace must belong to the stale manifest; journal ownership,
fingerprints and lease checks still govern persistent resource removal. A lost
review receives its normal interrupted outcome, without running a provider or
advancing the workflow. Unknown persistent resources keep the rig gate closed.
Recovery opens only an existing database at the binary's exact schema; it never
migrates or recreates one.

If an older retained binary lacks the required recovery fix, use the current
reviewed helper with an explicit reviewed recovery binary:

```sh
FREESIDE_REAL_RUN_RECOVERY_DAEMON=/absolute/reviewed/freesided \
  bash /absolute/reviewed-checkout/scripts/real-work-session.sh recover /absolute/session
```

This selects the binary only for `rig recover` and uses the helper's adjacent
lifecycle script. It preserves the session's saved binary, scripts and inputs.
Supervised restoration keeps its existing installation and schema checks;
choosing a recovery binary does not install it or authorize a runtime upgrade.

`daemon.log`, `verify-final.log`, `rig-cleanup.log` and `restore.log` retain
earlier diagnostics. Keep the entire private session until recovery and the
operator's evidence review are finished. There is no detached daemon continuation
promise and no dependency on the former scratch `serve-78.sh`.

## Recover Expired Reviewer Credentials

If composition preflight fails `codex_credentials`, use the matching recovery
hint. Preflight still enforces its lifetime floor, identity binding, and
re-enrollment hold. Recovery creates no task and starts no agent execution.

### Renew An Existing Store

Complete or recover any prior harness session, then stop the supervised daemon
as described above. Use a retained binary containing `renew-codex`, or build
the current reviewed version with `go -C daemon build -o /tmp/freesided
./cmd/freesided`. With the exercise's approved environment loaded, run:

```sh
/tmp/freesided renew-codex \
  -db "$FREESIDE_REAL_RUN_STATE_ROOT/freeside.db" \
  -auth-identity "$FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY" \
  -auth-store-root "$FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT" \
  -auth-store "$FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT" \
  -approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE"
```

Substitute the retained binary's path if using it. Pass every approved recipe
when the production store uses more than one. Success reports only readiness
coordinates and creates no hold. Rerun the intended harness command; its
composition preflight must now pass `codex_credentials`. If renewal was
interrupted, rerun it first to recover any pending rotation.

### Replace A Revoked Chain And Resolve Its Hold

When renewal reports a rejected or unavailable refresh chain, follow
[Enroll A Codex Subscription Identity](../daemon/README.md#enroll-a-codex-subscription-identity):
run `codex login`, stage its output in a separate private input root, and run
`enroll-codex` against the production database and live store. Enrollment
leaves a hold that requires a paired-device command.

With the same required exercise environment loaded, start the recovery service:

```sh
bash scripts/run-real-work.sh --recover-codex-credentials
```

The recovery daemon receives `FREESIDE_REAL_RUN_APPROVED_RECIPE`. If the
database also contains evidence for other approved recipes, pass each with a
repeatable `--approved-recipe sha256:<digest>` argument so paired-client sync
can reconstruct all retained evidence. These additional approvals apply only
to this recovery session.

This mode requires no submission files. It reuses the harness's environment
validation, clean build, and rig acquisition, then serves the production
database with `-driver disabled`. Before startup, a read-only schema check
refuses a database that this binary would migrate; use a schema-compatible
build or complete the supported runtime upgrade first. It skips identity
seeding, composition
preflight, submission, and the production driver. The full exercise environment
is still required, including values recovery does not otherwise use.

Use the printed endpoint and pairing-code command. In the paired client,
inspect the hold's exact digest, fence, and expiry, then choose **Resolve
re-enrollment**. The recovery service cannot accept the hold on your behalf.
Use its printed `real-work-session.sh complete` command when finished. Normal
completion and interruption stop the daemon, release the rig, and restore the
supervised service through the existing cleanup path. If cleanup fails, follow
the printed recovery command before retrying.

Stop the restored supervised daemon again before rerunning the exercise.
Recovery has left the task count unchanged; for an exercise requiring its
first task from the client composer, keep the database at zero tasks and use
the client-submission path below. The ordinary harness invocation with source
files submits a task and is not a substitute for that exercise.

## Submit A New Task From A Client

This is the live creation exercise for #1360 and the Mac/physical-iPhone
acceptance retained by #1211. Use a production session configured with the
explicit manual-submission file above, current installed clients, and the
normal authenticated App, policy, admission, and specification-approval gates.
Hermetic HTTP tests do not replace this evidence.

1. Choose a configured project already visible in sync. Project discovery is
   tracked separately in #1332. If visibility needs a CLI seed, use different
   source from the client exercise.
2. Confirm the chosen project/source has no existing task. For the requested
   gh-imgup exercise, use `Please handle
   https://github.com/freeasinbird/gh-imgup/issues/82.` as one line. Never
   pre-create that source through the CLI.
3. Enter that source in the client composer and submit. Record the command
   result, new task ID, specification-run ID, and the returned sync state.
   Verify the task name and run timeline on the Mac and physical iPhone.
4. Follow the normal clarification or specification flow. Leave the human
   specification approval gate in place. Submitting the same project/source
   from the second device proves reuse, not a second task creation.

The retained harness's publication verifier checks its original run. It is
not evidence that this new client task was created or published. Record the
new task's evidence separately, and leave live acceptance outstanding if the
approved inputs or either physical client are unavailable.
