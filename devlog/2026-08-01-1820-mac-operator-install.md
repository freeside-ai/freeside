# Installing the Mac Operator Client

Issue #444 (plan §10's install paragraph). Records the icon findings
that redirected the asset work, the signing mechanism chosen for
Keychain stability, and the refute-first pass over the install script's
destructive path.

## Decisions

- **Chose full-bleed icon slots derived from the design export over the
  export's inset artwork** (agent judgment, pending owner ratification),
  because macOS 26 masks a full-bleed app icon into its own superellipse
  but wraps a transparent-cornered one in a grey containment plate: the
  export as shipped renders the signet tile inside a second, system-drawn
  frame. The derivation scales the export's 824-pixel art area out to the
  1024 canvas (`magick … -resize 1274x1274 -gravity center -extent
  1024x1024`), and app/README.md carries the command so a re-export
  re-derives the same way. The cost is macOS 14/15, where a full-bleed
  square gets no rounding; nothing runs the operator client there, and
  the deployment target stays 14.0.
- **Chose signing through `xcodebuild` manual style over
  `CODE_SIGNING_ALLOWED=NO` plus a post-hoc `codesign`**, because the
  build already knows the inside-out order for the bundle's nested dylib,
  and the unsigned build's linker signature identifies the app as
  `FreesideMac` with an unbound Info.plist and no sealed resources. The
  installed bundle instead carries the real bundle identifier, so its
  designated requirement is stable across rebuilds for a fixed identity.
- **Chose an explicit `FREESIDE_MAC_SIGNING_IDENTITY=-` opt-in over an
  ad-hoc fallback when no Apple Development identity exists**, because an
  ad-hoc designated requirement is the code directory hash. A silent
  fallback would produce an install that works once and costs the pairing
  on every later update, which is the precise failure this unit exists to
  prevent. The script also prints the designated requirement each run and
  warns when an update changes it.
- **Chose the persisted `FreesideServerURL` preference as the installed
  app's daemon binding**, because `AppSession.fromEnvironment` already
  reads `UserDefaults` and nothing forwards launch arguments to a
  Dock-launched app. No client code changed; `--server-url` writes the
  preference, and a launch argument still wins through the argument
  domain.

## Verification Findings

- **An asset catalog cannot express a per-appearance macOS app icon**
  under Xcode 26. `actool` silently drops `luminosity` appearance
  variants on `mac`-idiom slots (the compiled `Assets.car` holds ten
  entries, all `Appearance: None`), and the iOS-style single-size
  layouts it does accept for appearances are rejected for macOS:
  `platform: macos` warns "Unknown platform value", and a
  `mac`/`1024x1024` entry compiles to zero app-icon entries with
  "unassigned children". The export's dusk variant is therefore unused;
  a per-appearance macOS icon needs an Icon Composer `.icon` document,
  for which Xcode 26 ships the app but no CLI and no documented schema.
- **The macOS 26 containment plate keys on edge contact, not merely on
  transparency.** A control icon that is opaque to all four canvas edges
  is masked with no plate; the export's inset tile is plated; and a tile
  scaled to exactly tangent the edge midpoints is *still* plated, which
  is why the derivation overscans slightly rather than fitting the art
  area to the canvas.

## Refute-First Pass (Destructive Path)

The install script replaces a bundle under the operator's
`~/Applications`. Adversarial enumeration of the input space, with
dispositions:

- **Confirmed and fixed**: `[[ -n "$candidates" ]] && count=…` aborts
  under `set -e` when the test fails, so a machine with no Apple
  Development identity exited silently instead of printing the
  instructions — the exact path a first install hits. Rewritten as `if`,
  and the same shape removed from the requirement capture.
- **Confirmed and fixed**: deleting the destination before moving the
  replacement in left a window with no installed app if the rename
  failed. The swap now renames the previous install aside, moves the
  staged bundle in, and only then deletes the superseded copy.
- **Confirmed and fixed**: the replaceability guard ran once, before a
  build that takes minutes. It now runs again immediately before the
  swap.
- **Rejected by verification**: a symlinked, foreign-bundle-id, or
  non-directory destination being destroyed — each guard was exercised
  against a symlink to another app and a planted foreign bundle, and
  both survived untouched. An unreadable `Info.plist` yields an empty id
  and fails closed on the same guard.
- **Rejected by verification**: an unquoted or metacharacter-bearing
  install path reaching `pgrep`/`rm` — the running-instance pattern
  escapes regex metacharacters, and an empty `FREESIDE_MAC_INSTALL_DIR`
  takes the `:-` default rather than resolving to `/`.
- **Accepted by decision**: two concurrent runs race on the staging
  path, and a stale `.install-staging` or `.install-superseded`
  directory is deleted unconditionally. This is a single-operator tool
  invoked by hand; guarding the race would cost a lockfile for no real
  exposure.
- **Accepted by decision**: terminating a running installed instance
  rather than requesting a graceful quit. `CacheStore` writes the
  disposable cache atomically, so termination cannot tear durable state,
  and a graceful quit would need an Automation permission grant.

Automated review then found three more, each reproduced before fixing:

- **Confirmed and fixed**: the running-instance pattern anchored at the
  end of the command line, so `pgrep -f` missed a client launched with
  any of the documented launch arguments and the swap proceeded under a
  live process. Demonstrated by launching the installed executable with
  `-FreesideColorScheme dark`: the old pattern missed it, the
  boundary-terminated one matches.
- **Confirmed and fixed**: the `--server-url` pattern accepted strings
  Foundation rejects, which is precisely the silent stranding the check
  existed to prevent — `URL(string:)` returns nil for `http://[` and
  `http://%`, and `AppSession.fromEnvironment` then falls through to the
  mock. Validation now runs the client's own parser and additionally
  requires the host `deploymentKey` needs, because any pattern drifts
  from Foundation.
- **Confirmed and fixed**: an interruption between the two renames left
  no app at the canonical path and a backup under a name nothing looks
  for. A restore trap now closes that window for everything short of
  SIGKILL, verified by fault injection rather than inspection: a
  deliberate failure between the renames restored the previous install
  with its signature intact.
- **Rejected by verification, with the comment corrected**: the
  bundle-identifier guard was said to prove this script installed the
  bundle, and it does not. Requiring an installer-owned marker was
  declined, because a hand-copied or differently signed Freeside build
  at that path *is* the client and replacing it is an update, which the
  changed-requirement warning already surfaces; a marker would refuse to
  manage exactly those builds. The overclaiming comment was the real
  defect and is fixed.

A second review round then found two defects introduced by those very
fixes, which is the lesson worth keeping: a rollback path needs its own
adversarial pass, because the first pass only established that the
happy path and the failure path work, not that the *interrupt* path
does.

- **Confirmed and fixed**: the restore trap covered INT and TERM but did
  not terminate, and bash resumes an interrupted script once a signal
  handler returns. The handler therefore restored the bundle and then
  let the staging rename land *inside* it. Proved by mutation in both
  directions: with the restore-only handler, an injected SIGTERM between
  the renames leaves the staging directory nested in the bundle root and
  `codesign --verify` reports unsealed contents; with a terminating
  signal handler beside a separate EXIT handler, the same injection
  rolls back cleanly with the signature intact. The staged rename now
  also refuses a destination that exists, since renaming onto a
  directory nests rather than replaces.
- **Confirmed and fixed**: `--server-url ""` (an unset variable's
  expansion) skipped both validation and the preference write, so the
  install succeeded while silently ignoring the requested binding.
  Validation and the write now key on whether the flag was passed, not
  on whether its value is non-empty.

A third round found the rollback window that remained:

- **Confirmed and fixed**: the superseded bundle was deleted, and the
  traps disarmed, *before* the installed bundle's signature was
  verified, so a bundle damaged in the copy or the rename left an
  unusable app and nothing to fall back to. Verification now happens
  while the rollback copy still exists, and the restore handles the
  post-swap state as well as the mid-swap one. Verified by injecting a
  failing verification: the original bundle comes back with the same
  inode, so it is the rollback and not a reinstall.

A fourth round found the last uncovered branch:

- **Confirmed and fixed**: on a *first* install there is no superseded
  copy, so the restore handler was a no-op and a failed verification or
  an interrupt left an unverified bundle at the canonical path. The
  handler now removes it in that case.

A fifth round found the state marker's own race:

- **Confirmed and fixed**: `staged_installed=true` sat *after* the
  rename it records. Bash defers a trap until the running external
  command finishes and then runs the handler before the next statement,
  so a signal during the rename reached a handler that still believed
  nothing was installed, and the first-install branch above became a
  no-op again. The assignment now precedes the rename, which is safe in
  the other direction because the rollback's `rm` then targets a path
  the rename never created. Proved with a minimal repro rather than by
  reading the manual: with the assignment after the command the handler
  observes `marker=false`, before it `marker=true`.

A sixth round closed two input spaces that had been sampled rather than
enumerated:

- **Confirmed and fixed**: a *dangling* symlink at the destination read
  as vacant, because `-e` follows the link. The staged rename then
  failed against it (a directory cannot replace a non-directory) and the
  rollback deleted the operator's link — the exact outcome the guard
  advertises preventing. An earlier round had verified only the
  live-target symlink and generalized from it wrongly. Both vacancy
  tests now examine the directory entry (`-e || -L`), which closes the
  enumeration over every file type rather than the two that came up:
  dangling symlink, live symlink, regular file, fifo, directory without
  an `Info.plist`, and foreign bundle identifier are each refused with
  the entry left intact.
- **Confirmed and fixed**: Foundation parses an out-of-range port
  happily (`:99999`, `:65536`, `:0` all yield a URL with a non-empty
  host), so the client could be durably bound to an unreachable
  endpoint. The validator now bounds the port to 1–65535, the same range
  `DeviceNtfySubscription` already enforces on the other URL the client
  stores.

A seventh round closed the last two gaps, both in spaces the previous
fixes had narrowed but not finished:

- **Confirmed and fixed**: the rollback removed whatever sat at the
  destination whenever a superseded copy existed, so if another process
  claimed the path while the previous install was set aside, restoring
  destroyed that entry. It now removes the destination only when this
  run's own rename put the bundle there, and otherwise refuses and
  reports where the previous install was left. A stranded install the
  operator can move back beats destroying someone else's bundle, which
  is the invariant the guard exists to hold.
- **Confirmed and fixed**: `url.port` is nil for both an omitted port
  and an unparseable one, so a port that overflows `Int`
  (`:18446744073709551616`) read as "no port" and bypassed the range
  check added a round earlier. The port is now validated against the raw
  authority text, requiring the canonical rendering to match the digits,
  which also rejects `:0080`.

An eighth round retired the mechanism rather than the case:

- **Confirmed and fixed**: the rollback decided "is this ours?" from a
  boolean set around the staged rename, and that flag cannot be correct
  in both directions. Set after the rename, a signal arriving during it
  runs the handler while the flag still reads false (round 5); set
  before, an entry appearing between the vacancy check and the rename is
  mistaken for ours and deleted (round 8). Four of the eight findings
  were this same question asked from different angles. The flag is gone:
  the rollback now compares the destination's inode against the staged
  bundle's, recorded before any rename. A rename preserves the inode, so
  the check answers the real question at the moment the answer is
  needed, independent of timing.

That fix also demonstrated why the cases belong in a test rather than in
a session: the first attempt left the initialiser *after* the
assignment, blanking it, and three previously passing cases went red
immediately. The by-hand matrix caught it; a merged script with no test
would not have.

The pattern across the eight rounds is the durable lesson, and it
changed how the fix was written. Each round's findings were consequences of the
previous round's fix, all on the rollback path, and none was reachable
by reading the diff, because only the happy path is. Three rounds of
patching one branch at a time is the signal to widen the boundary, so
the fourth stopped patching and enumerated the swap's whole state space
— prior app present or absent, crossed with how far the two renames
got — as a table in the script itself, with a test per cell. That table,
not the individual fixes, is what should be checked if this code changes
again.

Generalizing past this script: a destructive path's failure and
interrupt branches need fault injection as a matter of course. Every
finding above was proved by injecting the failure (a SIGTERM between
renames, a failing `codesign --verify`) and, where a restore had to be
distinguished from a plausible-looking reinstall, by comparing the
restored bundle's inode.

## Revisit When

- The mark is re-exported: if the export ships full-bleed artwork or an
  Icon Composer document, the derivation step in app/README.md becomes
  unnecessary and the dark variant becomes expressible.
- The operator client must run on macOS 14 or 15: the full-bleed slots
  render unrounded there, and the icon would need a per-OS answer.
- A second machine or operator installs the client: the identity
  resolution assumes exactly one Apple Development identity in the login
  keychain and otherwise asks for the override.
