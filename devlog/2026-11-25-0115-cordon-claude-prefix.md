# The Cordon Denies Every Home Entry Named .claude*

The cordon rule file now denies reading and writing every path that starts
with `$HOME/.claude`, through one regex anchored at the home directory. It
replaces the `~/.claude` subpath and `~/.claude.json` literal rules. The
`OwnerClaudeConfig` denial stays. Issue #3600, found by the shared-host
spike (#3596); tracker #3462. Tier A by path (`internal/cordon/`).

## Chose a Prefix Regex Over a Configured List

On a shared host, the run account's home holds `~/.claude-bnw`, a second
Claude configuration directory the owner's own sessions use, with session
transcripts and project history. A cordoned run read it, because the rule
file denied only `~/.claude`, `~/.claude.json`, and `OwnerClaudeConfig`.

A list of Claude directories in `Config` would have to be found at daemon
start (a change to `daemoncompose.LookupCordonAccount`), would miss a
directory made after the daemon starts, and would put per-host names in
the rule file's bytes. The regex has none of those costs: the rule file's
bytes stay the same on every host with the same configuration, and a
directory made later is covered. It also catches files nothing denied
before, such as `~/.claude.json.backup` and the CLI's other
`~/.claude.json.*` copies.

Rejected: denying only names matching `~/.claude-*`. It would leave the
`.claude.json.*` copies open, and the prefix is just as simple.

`OwnerClaudeConfig` keeps its own subpath rule, because it may sit outside
the home, where the regex doesn't reach.

## Accepted the Over-Denial

Anything in the home named `.claude*` is refused, `.claudeignore`
included. None of it belongs to a launch: its CLI configuration is
`CLAUDE_CONFIG_DIR` under the run root, which the run root's later allow
reopens. The dot is literal (`\.` in the regex, and `HOME_RE` arrives
quoted), so a name such as `~/Xclaude-x` stays open. If a tool a launch
needs turns out to read a `.claude*` home path, narrow the regex rather
than add an allow.

## How the Suite Proves It

- **Read probe:** the conformance suite opens each `.claude*` entry the
  home holds, read with `os.ReadDir` and a prefix test rather than a glob,
  so a glob character in the home's path can't change the match. Each path
  is proved once beside `OwnerClaudeConfig`.
- **Create probe:** a new `~/.claude-freeside-probe-<nonce>` directory
  must be refused, so the regex is proved on a host that has no second
  configuration directory.
- **Departure: the CLI's atomic-write temporaries are left out of the
  read probe.** Seatbelt answers a denied path that no longer exists with
  ENOENT, not EPERM. The CLI writes `~/.claude.json.tmp.<pid>.<hex>` and
  renames it away, so a temporary the suite found could vanish before the
  launch opens it, and the pass, or a pre-job probe, would fail on a host
  where the owner's sessions run. The plan probed every entry. The same
  regex denies the temporaries, the other entries and the create probe
  prove it on every pass, and the darwin launch test probes a temporary.
- **A symlinked entry fails the suite.** Seatbelt matches resolved paths,
  so a `~/.claude-x` that links outside the home isn't covered by the
  regex. The read probe then succeeds through the link and the pass fails,
  which is the right answer: the host isn't protecting that directory.

## Refute-First Findings

A fresh-context reviewer ran sandbox-exec experiments against the rule.

- **Disproved by a check:** reads through `//`, a relative path, a symlink
  the launch made, or a hard link it tried to make all meet the rule, and
  so does renaming an open directory to a `.claude*` name. Case variants
  (`.CLAUDE-BNW`) are denied too on the case-insensitive home volume.
- **Allowed by decision: other transient `.claude*` entries.** An entry
  other than the CLI's `.tmp.` temporaries that vanishes between the
  control and the launch would fail the pass. No current CLI is known to
  make one, and the failure is closed, so nothing more is left out.
- **Allowed by decision: the prefix test is case-sensitive.** A
  `~/.Claude-x` is denied but not probed. On a case-sensitive volume the
  regex may not match it (unchecked), and probing it would then fail the
  pass for a name the rule never meant to cover.
- **Allowed by decision: a FIFO named `.claude*`** would block the read
  control's open. Nothing makes one in a home.

## Recorded Proof Goes Stale by Design

The rule file's bytes changed, so its digest (`RuleDigest`) changed, and
every host's recorded conformance for the class no longer matches
(#3470). Cordoned work pauses on each host until its suite reruns.

Revisit when: another agent's state directory on the shared account needs
the same treatment (`~/.codex` is already denied), a tool a launch needs
reads a `.claude*` path in the home, or a pass fails on another transient
`.claude*` entry.
