# Production Walkthrough And Recovery

`scripts/run-real-work.sh` keeps the production daemon and its rig holder alive
after publication and final verification. The same database, state root and
listener remain available to the paired Mac and iPhone. The production deadline
ends at publication; the walkthrough has no automatic deadline or stdin prompt.
Closing the shell is an interruption, not a request to complete the walkthrough.

## Start And Verify

Use the harness header's required environment and input files. Before acquiring
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
Do not upload the directory: acquisition material and operator inputs are private.

Before startup, the verifier seeds the two required auth identities. After
publication, it opens the existing database read-only, checks the recorded
identities, and rechecks policy, evidence and the exact published head. It does
not migrate the database or create missing backup keys or artifact directories.
Both a successful exit and the explicit verification success marker are required.

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

Startup checks the listener's build and an authenticated retained publication
checkpoint, including the remote PR head through the operator's `gh` access.
A failed or pending feedback attempt may leave the previous ready item
superseded. That historical checkpoint permits resuming the walkthrough but
does not count as a completed successor publication. A failed restart uses the
same checked rig cleanup and release path as an ordinary session. Once the
upgrade has opened writable, it never rolls back the database or restores an
unchecked older service. This protects startup writes and preserved evidence;
the saved encrypted checkpoint is recovery evidence, not an automatic rollback.

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

`daemon.log`, `verify-final.log`, `rig-cleanup.log` and `restore.log` retain
earlier diagnostics. Keep the entire private session until recovery and the
operator's evidence review are finished. There is no detached daemon continuation
promise and no dependency on the former scratch `serve-78.sh`.
