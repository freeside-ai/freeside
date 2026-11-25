# The Installed Daemon Runs Real Work From Its Settings

`prod` started by launchd with no `-driver` now composes its Claude driver
from the stored settings records (#2082) when execution is on, exactly one
project is enabled, the settings load, prod's gated order lets it, and the
host checks pass. Otherwise it serves driverless with one blocking card
naming why and the fix, and never exits for those reasons. Issue #2083,
which closes the revisit condition of
`devlog/2026-09-08-0900-explicit-demo-modes.md`. Tier A by path
(`internal/daemoncompose/stored_execution.go`, `host_checks.go`, and the
settings and admission files it reads through).

## Start Order

Read the switches, open the store, decide from the records, run prod's
gated order (pause, writer lock, rig lease), then run the host checks, then
compose. The host checks come after the gated order so a daemon that may not
execute anyway runs no image-store or seed-root probe. A failure during
composition releases everything the start acquired, and `main` starts it
again with the failure carried in, so the second start raises the card. A
signal during composition is a shutdown, not a composition failure, and is
not retried.

## Chose Typed Errors Over Running the Conformance Pass First

Composition's own failures (the macOS verification executor's conformance
pass, a lineup line that does not resolve, a reviewer whose auth mode the
review section refuses) are named by error type after composition fails:
`HostCheckError`, `RoleAdmissionError`, and `ReviewProviderMismatch` map to
`host_check_failed` with the check's name and fix. Running the conformance
pass as a host check ahead of composition would run the full pass twice per
start. An unnamed failure keeps `composition_failed` with its text, which the
daemon authors.

## Host Checks Left Out, and What Covers Them

- **`claude_credentials`** needs the rig's runtime authorizer; composition
  and admission cover the credential.
- **`codex_credentials`** needs identity inspection against the database;
  composition reads it.
- **`judgment_roles`** makes model calls, which a start should not.
- **`publication_author_inputs`** and the writer image are per project; the
  readiness record covers them.
- **`build_egress_configuration`** is already a settings check
  (`settings_refused`), and **`build_egress_reachability`** the preflight
  never runs either.
- **`host_tools.container`** stats the executable instead of running
  `--version`: the image-store check right after it proves the tool runs.

## Chose to Approve Every Project's Recipe

The store opens approving the `approved_recipe` of every project that has
settings, enabled or not, rather than only the enabled project's. Disabling
a project is not revoking its recipe; evidence it recorded stays readable.
`freesided inspect task` on a stopped root still needs `-approved-recipe`,
as on a deputy root, and its note now names the settings section.

## Auth Adopt Through a Driverless Daemon, Without Identities

`auth adopt` goes through the running daemon's control socket, so the
installed `prod` need not stop, and an executing daemon refuses it and names
`freesided execution pause`. It does not record identities there, unlike
plan step 16: identities come from `auth add` on a stopped root, which the
issue leaves out of scope. The adoption lease is a fenced store row, so a
race with usage collection or the integrity probe ends in a refusal, not a
lost write; no owner question was needed.

## Late Tailscale Serves Loopback With a Card

The installer may set `-listen` to the Mac's Tailscale IPv4 address, and an
update keeps it. At login launchd can start the daemon before Tailscale
connects; an exit would loop under `KeepAlive`. The installed daemon waits up
to a minute, then serves on loopback at the same port with an advisory
`listener_tailscale_absent` card, which the next start on the address
concludes. Only that one failure (`ErrTailscaleAddressAbsent`) is waited on;
a taken port still exits. Rejected: retrying in the background and rebinding
while running, which would add a listener lifecycle for a case a restart
fixes.

## Refute-First Findings

One fresh-context review of the branch, before push:

- **Confirmed, fixed:** the memory-budget check, the initial doctor pass,
  and durable-stop recovery failed a composing start with a plain error, so
  the daemon exited and `KeepAlive` looped. They now fail as a composition
  failure; one that also fails driverless still exits, as before. No test
  reaches them, since the host checks' probes are fixed.
- **Confirmed, fixed:** the installed daemon ignored the stored
  `manual_submission_config` section, which the deputy honors, so a
  composed driver would refuse every client submission. Start reads it while
  composing, and the plan's inventory had missed it.
- **Confirmed, fixed:** the `auth adopt` route's driver gate sat in a file
  off the Tier A list; it moved to `cmd/freesided/auth_adopt.go`.
- **Allowed:** a card's reason carries an error's text, so a start that
  fails differently replaces the card. The daemon authors that text.
- **Allowed, unverified:** Tailscale may report its address a moment before
  the interface holds it, so the bind fails with a different error. That
  start exits once, and launchd's next start waits again.

Revisit when the project gates read the settings while the daemon runs
(#3209): then a settings change need not restart it, and the start order's
switch reads may move.
