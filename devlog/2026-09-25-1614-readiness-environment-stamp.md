# Refuse a Readiness File From Another Environment

Issue #1504. Trust-boundary work: the app decides which daemon to follow from
fields it decodes out of `readiness.json`. The owner decision to drop a
connect-time `run_id` check is recorded in the issue body and not repeated
here.

## Decision

Chose a stamp the app checks on every read over trusting the file's location.
`freesided` writes `environment` and a per-start `run_id` into the readiness
struct, so the file and stdout carry the same fields. The app accepts exactly
the four keys. A file with exactly the old two keys is refused as a daemon too
old to stamp, and a file stamped for another tier is refused by name. Every
other defect (unreadable, malformed, unknown environment value, empty
`run_id`) stays absence, as before.

- **The refusal outranks the pairing-code window.** The reader checks the
  stamp before the ten-minute age limit, so a refused file stays refused after
  its code expires. Only a matching file ages into absence. The earlier reader
  skipped parsing a stale file, which would have hidden the refusal.
- **A refusal stops at the connect screen.** Launch returns it after the
  explicit URL, pairing-demo, and mock arguments and before readiness, the
  persisted URL, and the tier's fixed port. Either fallback may be the very
  daemon the refused file describes, so falling through would be the silent
  connect the stamp exists to stop.
- **A typed address stays the override.** The connect screen explains the
  refusal above a working address field.
- **The menu watch passes on only a matching file.** A refused file reads as
  nil there, which clears a readiness prefill rather than offering another
  tier's code.
- **The daemon rejects an empty environment in `run`,** not only in flag
  parsing, so no composition can publish an empty stamp.

## Rejected Options

- **Treat an unstamped file as absence.** The app would then fall back to its
  fixed port or persisted URL and could connect to the old daemon anyway,
  with no explanation. Refusing with an upgrade message makes the upgrade
  order visible.
- **Refuse unknown environment values by name.** An unknown value is not a
  tier the app can name, and it only comes from a writer outside this
  contract; it stays malformed, like any other bad field.
- **Keep the age check first.** Cheaper, but it makes the refusal disappear
  after ten minutes and the app falls back as if no file existed.

## Consequence

An app built from this change refuses a `prod` or `dev` daemon started before
it until that daemon is upgraded. `install-mac-app.sh --daemon-path` installs
both together; an app-only install shows the upgrade refusal. After a refusal
the Mac app stays on the connect screen until the operator relaunches or types
an address: post-launch rechecks are a non-goal of #1504.

A refusal also outranks a persisted remote URL. A supervised tier whose own
state directory keeps a stale file from a pre-stamp daemon that no longer
runs, while the operator uses a remote daemon, stops on the connect screen at
every launch; a typed address works for that session only. The pre-change
reader let such a file age into absence. The contract requires the refusal to
outrank the persisted URL, so this stays; removing the stale file, or starting
an upgraded daemon that rewrites it, clears it. Found by the refute-first
review, left for the owner to weigh.

Refute-first review, other findings:

- **Disproved:** a refused file reaching a prefill or a silent connect. The
  menu watch maps a refusal to nil, `rePair` replays only that filtered value,
  and every app reader goes through the stamp check.
- **Allowed by decision:** a partial stamp, an unknown environment value, or
  an extra key reads as absence (see Rejected Options).
- **Allowed:** a pre-stamp key set with wrongly typed values reports "too old"
  rather than malformed. It fails closed; only the message is imprecise.

Revisit when an app must verify which daemon it reached without pairing (the
condition in the issue's owner decision), or when a reader other than the app
starts trusting `readiness.json`; it needs the same stamp check.
