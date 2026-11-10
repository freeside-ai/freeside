# The Writer's Launch Turns Off the CLI's Commit and PR Attribution

The Wave 9 exit run (#2386) found Claude Code's built-in attribution
reminder inside the ward: it told the writer to add a `Co-Authored-By`
trailer, which Freeside's commit plans refuse (#3028). That writer dropped
the trailer, but every writer spends effort on the conflict, and one that
follows the reminder writes a plan the daemon refuses. New Claude launches
now pass `"attribution":{"commit":"","pr":""}` in their `--settings` JSON
on every stage, under a new prompt delivery, `file_v8`.

## Decision

- **Chose a new delivery, `file_v8`, over adding the setting to
  `file_v7`.** The settings JSON is part of the launch command. Recovery
  rebuilds a running launch's command from its stored delivery, and ward
  refuses a rebuilt spec whose digest differs, so changing what `file_v7`
  builds would strand every in-flight `file_v7` launch across a daemon
  upgrade. This follows the rule on `stage.PromptDelivery` and the `file_v7`
  precedent (`devlog/2026-11-07-1832-review-guide-producer.md`). `file_v8`
  differs from `file_v7` only in the `--settings` value, and a test pins
  that on every stage.
- **Chose the launch settings over the agent image's managed settings.**
  Managed settings would also work and would bind every launch on the image,
  but they take effect only after an image rebuild and re-pin. The launch
  settings take effect on the next launch. The managed settings set no
  `attribution`, so the flag value is not overridden.
- **Chose to apply it on every stage, specification included.** The
  specifier writes no commits today, but the reminder is noise there too,
  and one value on every stage keeps the delivery's rule simple.
- **Chose the object form with empty `commit` and `pr` over
  `"attribution": false`.** The issue contract names the object form, and
  the pinned CLI's schema says older versions reject the boolean form, so
  the object form survives a CLI downgrade. `sessionUrl` is left unset: it
  applies only to web and Remote Control sessions, which a ward launch is
  not.

## Evidence

- **The pinned CLI reads this key.** The 2.1.319 build's settings schema
  (searched in the host's cached 2.1.319 binary, which bundles the same
  source as the image's npm build) describes `attribution.commit` and
  `attribution.pr` as "Attribution text for git commits, including any
  trailers. Empty string hides attribution." It also accepts `false`, the
  same as empty `commit` and `pr` with `sessionUrl: false`, and keeps
  `includeCoAuthoredBy` only as a deprecated alternative.
- **Refute-first, disproved by check.** Each claim below was tried against
  the change:
  - *Stored `file_v1` through `file_v7` launches rebuild a different
    command.* Disproved: every existing handoff golden is unchanged, and the
    new test finds no `attribution` in any older delivery's command.
  - *Some dispatch outside the Claude spec misses `file_v8`.* Disproved:
    the stage pipeline validates through `valid()` and `usesPromptFile()`,
    both extended; no SQL constraint or other list names a delivery; the
    `exhaustive` linter covers the dispatch switches.
  - *The longer settings push the command past the argument headroom.*
    Disproved: `TestPromptLimitLeavesLinuxArgumentHeadroom` now builds the
    longest command with `file_v8`'s settings and stays under a quarter of
    Linux's limit.
  - *Conformance must be re-proven.* Disproved: the conformance record binds
    a configuration digest, not the launch command, and the `file_v5`
    through `file_v7` bumps needed no re-proof.

## Revisit When

- A later exit run still shows an attribution reminder in a writer's
  session: the reminder then comes from somewhere this setting does not
  reach, which is a new finding.
- The agent image's CLI pin changes: confirm the new build still reads
  `attribution` before relying on it.
