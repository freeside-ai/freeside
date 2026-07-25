# Split Keystore Enumeration by Name Before Entry Kind

Work unit: #284. Credential-path policy change (what the keystore
accepts as an enumerable directory), plus an owner decision that
resolves a contradiction inside the issue, so this note is mandatory.

## Problem

`listAppsLocked` rejected every entry under `github-app/` that was not
a real directory, before looking at its name. `ListApps` is the only
enumeration behind `InstallationResolver.Resolve`, every
`InstallationJanitor` cycle, onboarding, and the doctor, so one
`.DS_Store` denied credentials for every owner. Finder rewrites that
file whenever it displays a directory and `DSDontWriteNetworkStores`
covers only network volumes, so no operator hygiene prevents it on the
reference platform. Observed three times on the maintainer's machine
(deleted during #250 verification, present again 2026-07-24, deleted
again 2026-07-25).

The diagnosis was also wrong: `ErrCredentialPermissions` with no
attributable owner made the doctor report `bad_keystore_permissions`
with owner 0, about a keystore whose permissions were correct, and
per-owner quarantine is not offered for it.

## Decision

Enumeration splits by **name** first. A name carries the whole
registration contract (canonical numeric owner ID, at most one
`.staging`/`.old` suffix), so failing it does not mean a damaged
registration, it means the entry is not a registration at all.

- Name is a canonical owner key: the entry must be a real
  (non-symlink) directory, else `ErrCredentialPermissions`. Unchanged.
- Name could never be a registration: skipped **only** if the entry is
  a regular file. A directory, symlink, or irregular entry is still a
  hard error.
- The legacy singleton's own file names (`app.pem`, `app.json`,
  case-folded) are excluded from the skip whatever their kind. They are
  keystore state rather than foreign noise, and the legacy migration
  enumerates without the gate `ListApps` runs first; the verification
  section records what that cost before it was caught.

This does not weaken #279's refusal to skip an unreadable *record*.
Skipping a registration narrows the set resolution picks among, which
can resolve to the wrong App or miss an ambiguity; a name that could
never be a registration was never in that set, so skipping it narrows
nothing.

`Keystore.UnexpectedEntries` reports what was skipped, and the doctor
turns a non-empty result into a new `unexpected_keystore_entry`
finding, gathered before enumeration and carried through its
failure returns so a damaged record cannot hide the artifact next to
it.

## Owner Decisions Recorded Here

- **The issue contradicted itself** and the owner chose the stricter
  reading. Its body says to skip any entry whose name could never be
  an owner key; its Acceptance 3 says a *directory* with such a name
  still fails closed. Acceptance governs, on the merits: the observed
  harm class is entirely regular files (`.DS_Store`, `._*`, `Icon\r`),
  while an unexplained directory in a 0700 credentials directory is
  operator error, a botched migration, or tampering, and keeping it a
  hard error costs nothing against this defect.
- **A symlink at an impossible name stays a hard error** for the same
  reason, though the issue's bullet would have skipped it. No
  operating system writes symlinks into a directory it displays.
- **The advisory finding is code-only.** `CredentialFinding`
  deliberately carries safe numeric coordinates; naming the skipped
  entry would put an arbitrary filesystem-supplied string into that
  shape. The operator gets "something here is not a registration" and
  runs `ls`.

## Rejected Alternatives

- **Skip every impossible name, including directories** (the issue
  body's literal bullet). Loses the tamper/migration signal for the
  case the defect never involved; see the owner decision above.
- **Special-case `.DS_Store`, or dot-prefixed names generally.**
  Encodes one operating system's product knowledge and would have to
  grow for the next artifact (`._*`, `Icon\r`, `Thumbs.db`); the name
  test already answers the real question, which is whether the entry
  could be a registration at all.
- **Plumb the skipped names out of `ListApps`.** Would change the
  signature its four callers share for a diagnostic only the doctor
  wants; a separate lenient read is one `ReadDir` and keeps the
  enumeration contract intact.
- **Leave the skip silent and file the advisory as follow-up.**
  Silently swallowing an entry is the failure mode this project keeps
  paying for; the advisory is what makes the skip auditable.

## Refute-First Verification (Required for This Risk Class)

An independent fresh-context lens was prompted to refute the change,
after a first-pass self-audit of the legacy-migration call path.

**Confirmed, fixed in the branch:**

- *The skip reached the legacy migration.* `migrateLegacyLocked`
  enumerates without the legacy gate `ListApps` runs first, so with a
  `github-app.legacy` journal present a legacy `app.pem` stranded in
  the registration root was skipped and the migration completed around
  it, leaving private key material in a root the daemon then treated
  as active and denying every later enumeration. The legacy singleton's
  own file names are now never skippable.
- *That guard was case-sensitive; the reference platform's filesystem
  is not.* `legacyFilesExist` lstats `app.pem`, which resolves a file
  named `APP.PEM` on case-insensitive APFS while its directory entry
  keeps its own spelling, so the gate and the guard disagreed and the
  same stranded-credential state returned through a different door.
  The comparison is case-folded, which over-refuses on a case-sensitive
  volume: the fail-closed direction.

**Rejected by verification** (probed, no failure constructed): name
classification is unchanged for every non-file entry, checked by a
differential fuzz against the pre-change predicate (~950k executions,
zero divergences) plus an adversarial name table (empty, `.`, `..`,
bare and compound journal suffixes, `0`, `-1`, `+42`, `007`,
non-ASCII digits, int64 overflow, embedded NUL and newline); no
credential-bearing regular file other than the legacy singleton can
live in the registration root, since the janitor journal, lock, and
authority snapshot live under the state directory and swap journals
are directories; `DirEntry.Type()` cannot be spoofed by a symlink to a
regular file; `UnexpectedEntries` returns base names only, holds the
same lock and preconditions as enumeration, and shares its skip
predicate, so the two cannot disagree; the doctor's `findings` slice
is fresh per call and never aliased, and an operational failure still
cannot surface as a clean bill of health.

**Accepted by decision** (not fixed here):

- A directory named `0`, `-1`, or an int64 overflow now fails closed as
  `ErrCredentialPermissions` rather than an unwrapped `appOwnerKey`
  error, so the doctor reports a finding instead of an operational
  error. Both diagnoses are equally imprecise about a stray directory
  and route to the same inspection; consistency with every other
  unexpected-entry rejection is worth more than preserving the old
  sentinel.
- The advisory names no entry. That is the owner decision recorded
  above; `CredentialFinding` carries safe numeric coordinates by
  design.
- `CredentialDoctor.Check` now reads the keystore under three
  independent critical sections rather than one, so a concurrent save
  or withdrawal between them can make the advisory stale. The doctor's
  split-lock reads predate this change, the finding is advisory, and
  narrowing it means a locking contract for the whole doctor, which is
  its own unit.

## Revisit When

The keystore gains a legitimate non-directory member (a lock file, an
index) directly under `github-app/`. At that point the name test needs
a positive registry of expected non-registration names rather than a
"could never be a registration" predicate, so an unexpected artifact
does not read the same as an expected one.
