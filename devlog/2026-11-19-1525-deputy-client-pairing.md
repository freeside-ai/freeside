# Keep an Ephemeral Client Paired for the Life of Its Root

#3448: attach a matching client to a deputy and keep it paired across
daemon and app restarts.

## Decisions

- **Chose a credential file in the readiness directory over the Keychain or
  memory.** An `ephemeral` app followed by `-FreesideReadinessDir` keeps its
  device credential in `<state-dir>/ephemeral-app-credential.json` (`0600`,
  written by temporary file and rename). Memory lost the pairing on every
  relaunch, so each deputy restart cost a fresh code. The Keychain is out
  because a Debug build shares prod's Keychain identity, and a probe could
  prompt for or migrate a prod item; it also outlives the root, so a new
  root on the same port would send a dead token. The state directory already
  holds the readiness file's pairing code under the same same-user trust
  (plan §5.2), and the credential dies with the root, which is the lifetime
  plan §10 now names ("pairs afresh on each root"). The file names the
  deployment URL it was minted for and loads only for that URL. That binds
  the token to an address, not a daemon: if the root's daemon is stopped and
  another local process takes its port, the app sends it the token. That is
  the exposure the supervised tiers' fixed ports already carry, and the
  token is useless to any daemon but its own. Tier A by category
  (credential storage), though AGENTS.md's Tier A path list covers `daemon/`
  only.
- **Chose to follow an expired readiness file's URL over asking for a
  connection.** Once the startup code expires, the ephemeral app still opens
  on the run's URL, with no code: either the directory holds its credential,
  or that run's daemon is still the one to pair with. The plan's version
  followed the URL only when a credential was stored; dropping the probe
  removed a launch-time file read without changing any contract case.
- **Chose a "still starting" answer over serving pairing earlier.** Ward
  conformance runs before the API or control socket serves, and a code is
  useless until the API serves. `freesided pairing-code` with no
  `pairing-control.json` probes the state directory's pairing-control lock
  without creating the lock file (command-side discovery stays read-only, as
  `TestPairingControlCreatesStateOnlyOnDaemonStartup` requires), rather than
  taking it with `daemonlock.Acquire` as the plan said.
- **Chose a log warning over a host notification** for an open item with no
  active device, once per item per process.

## Reconnect After a Daemon Restart

Not reproduced; no reconnect change. The issue reported that the ephemeral
app stayed on "Daemon unreachable" after its deputy restarted. On this
branch's tip, an ephemeral Debug app attached to a `freesided` on a fixed
loopback port (the #2382 exit roots also served on `127.0.0.1`) returned
to fresh within one heartbeat (5 to 25 seconds) after every restart on the
same root and port, observed both by its connections and by window
captures of the banner:

- launched already paired, and paired in-app after launch (so the menu-bar
  heartbeat task started on a phase change);
- a 3.5-minute outage with the window in the background;
- three minutes with the port bound but never accepting, the shape of a
  deputy's conformance window (`daemoncompose.Start` binds the listener
  before it composes the Claude workflow and serves).

`SyncCoordinatorTests.unreachableDaemonDegradesToTheBannerAndRecovers`
already pins the recovery. The likeliest reading of the exit run is that the
deputy's minutes of start-up conformance looked like a failed reconnect, and
the app's relaunch then lost its in-memory pairing, which the credential
file above fixes. A real `freesided deputy` restart was not exercised
(it needs the operator's credentials), so a deputy-only cause is not ruled
out.

Follow-up: #3498

## Refute-First Record

A fresh-context reviewer tried to refute the change before push.

- **Confirmed and fixed:** `dev-instance.sh --attach` launched an earlier
  build's app when `xcodebuild` failed (`set -e` does not reach `$(...)`),
  and accepted a stopped deputy's leftover readiness file. The build now
  exits on failure, and the attach waits until the file's URL answers
  `/health`. A test comment claimed an expiry case it does not exercise.
- **Declined:** guarding the token against whatever holds the port (above);
  the pairing-control probe racing a starting daemon (one non-blocking
  `flock`, as `store_access.go` does, and a rerun recovers); the refused-code
  message showing on a partial code (the app has no fixed code length, a
  partial code is not valid, and the preview is debounced); quoting the
  deputy's printed command path.
- **Disproved:** an ephemeral launch reaching the Keychain; a credential
  loading for another deployment; a readable or partly written credential
  file; any change to the supervised tiers; the lock probe creating a file;
  the no-device warning growing without bound or logging a secret; and
  older consumers breaking on `expires_at` (the app accepts both shapes).

## Revisit When

- A client other than the Mac Debug app attaches to ephemeral roots, or the
  readiness directory stops being private to the daemon's OS user.
