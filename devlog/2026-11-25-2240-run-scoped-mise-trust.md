# Keep a Cordoned Run's mise Trust in the Run's Directory

Issue #3601. Tier A (runner containment). Found by the shared-host spike
(#3596, `docs/spikes/cordoned-shared-host.md`, item 8). Measured on the
owner's Mac with mise 2026.11.10.

## Decision

A cordoned launch's environment sets `MISE_STATE_DIR` to `mise-state/`
under the run's own `RUN_ROOT`. mise keeps `trusted-configs` and
`tracked-configs` in its state directory, so the trust a plain
`mise install` grants the worktree's config is written there and removed
with the run's directory. mise's install, cache, and config directories
stay the owner's, so a tool one run installs stays shared with later runs.
The rule file doesn't change.

The planning session for #3601 chose the environment over a rule-file
denial (2026-11-25); this unit measured that it holds.

## Why

- **The trust entry was the hole.** Inside a launch, a plain `mise install`
  trusted the project's config without being asked and recorded a link to
  the run's worktree in `~/.local/state/mise/trusted-configs`. The entry
  outlived the run, so a later unconfined mise in that path would read the
  config as trusted. That breaks plan §5.7's rule that nothing outside a
  confined run reads a repository's manager configuration (#3597).
- **mise honors `MISE_STATE_DIR` for trust, and still trusts by itself.**
  With only the state directory moved, and no `MISE_TRUSTED_CONFIG_PATHS`,
  an install from a fresh state directory wrote both links there and none
  in the home. `TestLiveLaunchKeepsMiseTrustInTheRun` repeats this inside a
  launch, with the real home's state directory as the one a regression
  would write. With the variable removed from the launch's environment,
  the same test fails: the home gains a `trusted-configs` and a
  `tracked-configs` link naming the run, and the run's own state directory
  stays empty.
- **Installs stay warm.** A second launch with a fresh state directory and
  `MISE_OFFLINE=1` ran the first launch's jq from the shared install or
  cache directory. Offline, mise refuses a version it would have to
  download, so the test shows the second run fetched nothing from the
  network.

## Rejected

- **A rule-file denial of `~/.local/state/mise`.** It would stop only an
  install that leaves mise's defaults alone, which the environment already
  covers. A run that sets its own variables, or writes mise's global config,
  gets past it either way: that's the residual plan §14 (Cordoned runs on a
  shared account) already accepts for the version-manager directories every
  run and the owner's sessions share. It would also need its own
  conformance probe.
- **`MISE_TRUSTED_CONFIG_PATHS` alone.** The spike saw it stop the trust
  entry but still write a `tracked-configs` link to the home, and mise
  trusts the run's config by itself, so the variable isn't needed.
- **`XDG_STATE_HOME`.** It would move every XDG tool's state into the run,
  not only mise's.
- **Per-run install and cache directories.** They would cost every run a
  fresh download of each tool.

## Consequences

- **The owner's `mise prune` no longer sees runs' configs.** Their
  `tracked-configs` links live and end with each run, so a prune may remove
  a version a later run then reinstalls.
- **A run that unsets the variable, or writes the trust file itself,** is
  the §14 residual above, unchanged by this unit.
- **The agent CLI must pass its environment to the tools it runs.** The
  fake agent can't show this; the CLI already relies on it for `TMPDIR` and
  `CLAUDE_CONFIG_DIR`. A shell that sources the owner's startup files gets
  whatever they export, so an owner who sets `MISE_STATE_DIR` there sends
  runs' trust back to that directory. That's the startup-file reach the
  §14 residual already names.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong.

- **Disproved by a check:** a `/var` against `/private/var` mismatch
  hiding a home entry (the runs root is resolved before the match), and a
  vacuous pass when mise writes no link (the run's own state directory
  must hold one).
- **Confirmed and fixed:** the second launch wasn't checked against the
  home; it now is. Offline mode proves no network fetch, not that the
  install directory rather than the cache served the tool; Why says so.
- **Allowed by decision:** an owner's startup files re-exporting
  `MISE_STATE_DIR` (Consequences), and the stale RUN_ROOT comment in
  `rules.sb`, left because the contract keeps the rule file's bytes.

## Revisit When

- A mise release moves trust out of its state directory, or stops trusting
  a config by itself under a fresh one. The live test then fails, on the
  home check or on its check of the run's own state directory.
- The rule file starts denying writes under `~/.local/state`, or plan §14's
  residual on version-manager directories changes.
