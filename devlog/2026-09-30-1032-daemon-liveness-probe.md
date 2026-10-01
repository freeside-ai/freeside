# Notify Over ntfy When the Daemon Is Dead or Crash-Looping

Work unit #510, the external liveness probe of plan §5.2, from the
supervision contract in `devlog/2026-08-05-0001-supervision-contract.md`.
The probe is `scripts/daemon-liveness-probe.sh`: one `check` polls
`GET /health` and exits, and a per-user LaunchAgent runs it on a timer.

## Chose an Operator-Chosen Topic Over a Device Topic

The probe sends to an ntfy topic the operator picks and subscribes to. A
device topic from pairing can't be reused: the daemon derives each one from
a key only it holds and hands it only to that device
(`daemon/internal/signet/ntfy.go`). Rejected: having the daemon mint a probe
topic. That is a daemon change, and a probe must not depend on the process
it watches to learn where to report.

## Chose One Poll per launchd Start Over a Long-Running Loop

launchd starts every poll (`StartInterval`, `RunAtLoad`), and the alarm
state lives in a file. A poll that crashes loses only that poll, and nothing
has to supervise a probe loop. Rejected: a `KeepAlive` loop. It would need
its own crash handling, and a wedged loop is a silent hole.

## Chose the Thresholds and Kept the Recovery Notice

- **Unreachable after 3 failed polls in a row, 60 seconds apart.** A poll
  fails without HTTP 200, `"status":"ok"`, and an RFC 3339 `started_at`.
  Three minutes rides out a clean restart or a reinstall. It still reports
  a dead daemon well before an away operator would notice.
- **Crash loop after 3 `started_at` changes within 10 minutes.** A single
  restart is one change and raises nothing. A daemon that dies before it
  binds never answers, so the unreachable alarm catches it instead. Either
  alarm meets the acceptance.
- **Each alarm fires once.** The unreachable alarm re-arms on the next good
  poll. The crash-loop alarm re-arms when no change is left in the window,
  with no notice.
- **"Reachable again" on recovery.** The planning assumption stands: an away
  operator told the daemon is dead needs to know whether it came back. The
  owner may strike it; it is one branch in `advance`.
- **A flag flips only after ntfy accepts the notice.** An undelivered
  notice is owed again on the next poll, so an ntfy outage delays an alert
  but does not drop it. A crash-loop notice is kept owed in `state`, not
  recomputed: the oldest change can leave the window before the retry, and
  the count would fall below the threshold. The one drop is deliberate: an
  unreachable notice still undelivered when the daemon answers again is
  superseded, and no "reachable again" follows. The outage is over and was
  never reported, so neither notice is actionable.
- **Alarms are independent.** One poll can owe both the recovery and the
  crash-loop notice, and it sends both.

All four numbers are config keys. The alarm rules sit in one function with
no I/O, which the suite exercises through the real script.

## Chose Secrets on stdin and https-Only ntfy

The topic URL is a capability, and the token is a secret. Both go to curl
through `--config -` on standard input, because arguments are readable by
every process on the host through `ps`. curl runs with `-q`, so `~/.curlrc`
can't add a trace or a proxy. A failed send is logged by curl exit or HTTP
status alone, and curl's stderr is discarded, matching `ChannelRejectionError`.
The notice names only the tier and the alarm. `ntfy_url` must be https, since
a token sent over plain http leaks on the wire.

Revisit when an operator self-hosts ntfy without TLS on a private network.

## Chose a Fixed-Key Reader and the Topic-Key File Rule

`config` and `state` are read line by line against a fixed key list and
assigned with `printf -v`, never sourced. An unknown or repeated key is
refused, so a typo can't silently fall back to a default. `config` must be
a regular file with one link and no group or other permission bits, the
rule `daemon/internal/topicstore` applies to the daemon's topic key. The URL
and token must carry no quote, backslash, or whitespace, so they can't break
out of the curl config's quoting.

The `/health` body is trusted for two checks and nothing more. `started_at`
must match the RFC 3339 pattern before it is compared or stored. The pattern
admits no newline or quote, so it can't inject a line into `state`.

## Refute-First Findings

An independent reviewer tried to break the probe before commit.

- **Confirmed and fixed: a config error echoed its line.** An unknown key
  was printed back, so a pasted URL (`https://ntfy.sh/<topic>?auth=...`) or
  a bare token with `=` padding reached `probe.log` on every poll. Errors
  now name only the line number.
- **Confirmed and fixed: a clock stepped back pinned the crash-loop alarm.**
  A change recorded "in the future" stayed in the window. Such changes now
  drop out.
- **Fixed on known behavior, not reproduced: reinstall races launchd.**
  `bootstrap` right after `bootout` can fail with error 5 while the old job
  tears down, so install retries it twice, a second apart.
- **Declined: an unanchored `"status":"ok"` match.** A nested
  `{"status":"ok"}` passes, but only a hostile listener on the port could
  send one, and that listener can forge any body.
- **Disproved by a check:** the secrets never reach argv, a temp file, or
  the plist; a crafted URL or token can't break the curl config quoting; a
  newline in `started_at` fails the poll instead of reaching `state`; both
  `stat` forms, `head -c`, and `mktemp` agree on BSD and GNU; and a HOME with
  spaces, `&`, or `<>` yields a plist `plutil -lint` accepts.
- **Accepted by design:** a refused config makes every poll fail with
  nothing sent, its reason only in `probe.log` (stated in the header), and
  `probe.log` is never rotated, since it logs only failures and notices.

## Same-Host Limit

The reference install runs on the daemon's host as a per-user agent. It
stops with the host and at logout, as the daemon does, so it can't report a
dead host or a logged-out session. `health_url` accepts another host, but a
second-machine setup is not built or proven here.

Revisit when the operator needs host-down coverage. That is a contract
change: a probe on a second machine, or the managed monitor plan §5.2
names.
