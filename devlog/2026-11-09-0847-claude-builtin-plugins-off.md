# Disable Claude Code's Built-In Plugins in the Agent Image (#2922)

Work unit: #2922. Scope: `images/agent-claude/`,
`scripts/build-agent-claude-image.sh` and its regression suite,
`daemon/internal/agentbaseline/` (test),
`daemon/internal/exec/claude/` (live probe and fixture),
`daemon/README.md`, and this note. Corrects
`2026-11-08-1709-claude-2-1-319-reproof.md`. Tier A by substance: the image
file decides what prompt content a writer launch loads, and admission's
`context_severance` proof depends on it.

## The Miss

The 2.1.319 re-proof (#2895) recorded that `cc-plugin-sec-default`,
`cc-plugin-privacy`, and `cc-plugin-plugin-authoring` join the writer's init
event, and judged them harmless because they add no egress or credential
path. It didn't rerun the conformance judge, which proves severance only
when the init event's `plugins` and `mcp_servers` lists are both empty. The
third #2396 rehearsal found every writer role refused at admission:
conformance on the 2.1.319 adapter digest no longer proved
`context_severance`. Admission was right to refuse. `cc-plugin-agents-md`
can load repository instructions, and the others add prompt content outside
the daemon's instruction bundle.

## Root Cause

- **`cc-plugin-sec-default` is policy-only.** The 2.1.319 bundle registers
  it with `enabledFromPolicyOnly`, so its enabled state is read only from
  managed settings, never from `--settings`. The first plan, a `file_v8`
  delivery with a longer `--settings` value, couldn't have worked.
- **The image's own managed settings seat it.** The bundle seats
  `sec-default` on any machine with non-user-writable managed settings, and
  the image has shipped `/etc/claude-code/managed-settings.json` for its
  auto-update switch since #304. So every launch in the image loads it,
  with or without a login.
- **The registered set is fourteen ids.** Which load depends on the
  entrypoint, account, interactivity, and machine, and no single switch in
  2.1.319 disables all built-ins.

## Decisions

**Disable all fourteen registered ids in the image's managed settings.**
Managed settings are the one tier `sec-default` reads, and the other
thirteen read them too. Chose the full registered list over the three
observed ids because one environment's init event can't prove a shorter
list complete: a plugin that loads only under another account would break
severance again, or worse, load repository instructions unnoticed.

**Ship the file in the image, not from the launcher.** A root-launcher write
before the privilege drop would also reach the policy tier, but it would
change the launch command, and recovery rebuilds a running launch's command
byte for byte from its stored delivery. The image already owns this file.
`file_v5` to `file_v7` stay byte-identical, and their `--settings` flag
stays, now redundant for `agents-md`.

**Keep the judge's rule.** An empty `plugins` list stays the proof of
severance. Accepting `sec-default` by id would trust the vendor's
description of it instead of an observed empty list.

**Move the file beside the Containerfile.** The daemon's offline guard and
the image then read the same bytes.

## The Permission Trap

The first rebuild still loaded three plugins. `COPY --chmod=0644` gave the
parent directory it created the same `0644`, so the agent user (uid 1001,
after the launch's privilege drop) couldn't traverse `/etc/claude-code`.
The CLI skipped the unreadable file, yet `sec-default` still loaded, since
the file's presence is what seats it. The build check ran as root and
passed. The Containerfile now creates the directory `0755` first, and the
build check reads the file as uid 1001. Only the live probe caught this.

## Measurement

On the rebuilt image, networkless and credential-free, the production
`file_v7` argv under an API key and a loopback mock API reports
`plugins: []` and `mcp_servers: []` on a specification and a
non-specification stage. Before the fix, the same image lists the three
plugins above.

## Guards

- `TestClaudeImageDisablesEveryBuiltinPlugin` pins the file's exact
  contents and fails when `ClaudeCodeHarnessBuild` moves off
  `claude-code 2.1.319`, until someone re-measures the new bundle's
  registered built-ins.
- `scripts/build-agent-claude-image.sh` reads the file in the built image as
  uid 1001 and requires every `enabledPlugins` value to be `false`.
- `TestPinnedClaudeInitSeveranceLive` runs the writer launch on the image
  and requires the conformance judge itself to report severance, so the
  probe can't drift from the rule admission depends on.

The probe runs under an API key with no account. A Team or Enterprise
account seats `sec-default` from the account too, and can serve remote
managed settings that join the same policy tier. The bundle says a
deployed managed-settings file takes precedence over them by default, but
only an authenticated conformance pass on the rebuilt image proves the
file's `false` wins there. That pass is the authoritative check; the probe
catches regressions in the image itself.

The daemon tests that guard image files run only when daemon CI runs, and
a PR that touches only `images/` doesn't select it. Follow-up: #2937.

## Consequences

- **The shadow reviewer loses the plugins too.** It runs in the same image.
  The plugins are prompt content, not tools, so no reviewer behavior the
  daemon relies on changes.
- **The adapter digest doesn't change.** It's built from the harness
  builds, not the image digest, so the first conformance pass on the
  rebuilt image restores `context_severance` for the 2.1.319 adapter
  digest. A daemon still pointed at the old image would be admitted by that
  record; the daemon runs one pinned image, so that is the existing design.
- **The file isn't on the Tier A path list.** `scripts/check-tier-a-paths.sh`
  resolves entries under `daemon/` only. Follow-up: #2936.

Revisit when the CLI pin moves (the offline guard forces it), or when a
later CLI offers one switch that disables every built-in plugin.
