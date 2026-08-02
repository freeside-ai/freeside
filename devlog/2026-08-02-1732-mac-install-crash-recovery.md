# Recovering an Interrupted Mac Install

Issue #464. This revises the stale-backup assumption recorded in
`2026-08-01-1820-mac-operator-install.md` after fresh destructive-path
evidence showed that a superseded bundle can be the only recovery object.

## Decision

- **Chose verified startup restoration over unconditional stale-backup
  deletion**, because SIGKILL can land after the installed app is renamed to
  `.install-superseded` but before the staged app reaches the canonical path.
  In that state the superseded bundle is not stale: it is the operator's only
  known-good installed client. The next invocation restores it before identity
  resolution, building, or cleanup, so any later failure leaves the canonical
  client available.
- **Chose fail-closed preservation over best-effort promotion or deletion for
  an invalid recovery object**, because the path can contain a symlink, foreign
  bundle, or damaged app. Restoration requires a real directory, the Freeside
  bundle identifier, and a valid code signature. Failure leaves the object at
  its recovery path and gives explicit manual instructions; entries that fail
  those Freeside-client checks are preserved rather than destroyed.
- **Chose Darwin's exclusive rename over BSD `mv` after a vacancy check**,
  because `mv` nests its source inside a directory that appears at the target
  and reports success. `renamex_np` with `RENAME_EXCL` makes target
  nonexistence part of the filesystem operation, so a last-moment interloper
  leaves both directory entries untouched.
- **Chose the future #458 matrix path for the focused regression**, because
  `scripts/test-install-mac-app.sh` is already the named home for hermetic
  installer state-machine tests. This work pins the newly discovered restart
  state now; #458 remains the owner of the broader existing rollback, guard,
  and URL matrix.

## Refute-First Verification

- **Confirmed**: the restart-equivalent state (canonical path absent, valid
  superseded app present) restores the same inode before a deliberately failing
  build and before a missing-signing-identity failure, rather than copying or
  silently stranding the client.
- **Rejected by verification**: a bad signature could be promoted. The focused
  suite makes signature verification fail and proves the canonical path stays
  absent, the recovery inode remains unchanged, and the build never starts.
- **Rejected by verification**: a symlink at the recovery path could redirect
  validation or restoration into foreign contents. The suite proves it fails
  as a non-bundle entry while both the link and its target remain untouched.
- **Rejected by verification**: a correctly shaped but foreign bundle could be
  restored based on path alone. A foreign bundle identifier fails before the
  build, with its inode and contents preserved under the recovery name.
- **Confirmed and fixed**: an interloper could create the canonical directory
  while signature verification ran, after the initial vacancy check. BSD `mv`
  would then nest the recovery bundle inside that foreign directory and report
  success. Recovery now rechecks the directory entry after verification and
  uses an exclusive rename; fault injection at both the verification boundary
  and the rename primitive proves the interloper and original recovery inode
  remain untouched.
- **Confirmed on the reference Mac**: `renamex_np(RENAME_EXCL)` moved a source
  directory into a vacant target, then returned `EEXIST` with both source and
  target intact when the target already existed. The Linux stand-in rejects an
  embedded program missing `renamex_np` or `RENAME_EXCL` and injects the
  last-moment interloper; macOS CI executes the exact embedded Swift program so
  a syntax, type, or platform-API regression also fails the suite.
- **Confirmed and fixed (Codex P1)**: exclusive destination creation did not
  bind the rename's source to the app that passed verification. A replacement
  at `.install-superseded` could therefore be promoted after the gate. The
  installer now applies the same type, bundle-ID, and signature gate to the
  moved object and exclusively returns any mismatch to the recovery path before
  failing. Source-side fault injection proves the foreign replacement is rolled
  back and the displaced verified inode is not deleted.
- **Confirmed and fixed (Codex P1, round 2)**: SIGKILL after the staged rename
  can leave both an unverified canonical app and the known-good superseded app.
  Startup now validates the canonical app when both paths exist. An untrusted
  canonical object is exclusively moved to a persistent rejected path before
  the verified backup is restored through the same source-race gate. The test
  reconstructs that state and proves both original inodes survive at their
  safe destinations.
- **Confirmed and fixed (Codex P1, round 3)**: post-move validation alone left
  a SIGKILL window when the recovery source was replaced. Recovery promotion
  now creates a persistent guard directory before the exclusive rename. There
  is no atomic validate-and-disarm operation, so the guard is deliberately not
  removed automatically: every later installer invocation re-gates the
  canonical object. A mismatch is exclusively quarantined and the installer
  fails closed. Combined source-replacement, SIGKILL, and post-quarantine
  interloper injection proves the next two invocations reject both untrusted
  objects before the build while the guard stays armed.
- **Confirmed and fixed (Codex P2, round 4)**: the permanent guard originally
  treated an empty canonical and superseded state as corruption, which blocked
  a clean reinstall after the operator removed the app. An empty guarded state
  now proceeds as a fresh install while retaining both the guard and any
  quarantined evidence. The guard's strict signature check is repeated
  immediately before the destructive swap, so an untrusted canonical app that
  appears during the build is preserved rather than moved to the deletable
  backup path. After the canonical entry is moved aside, the full recovery gate
  runs again on that exact object before it can become deletable, closing the
  check-to-rename window. The cross-platform suite injects both boundaries and
  binds the injected objects by inode at creation time.
- **Preserved invariant**: the existing destination foreign-entry checks and
  staged-inode rollback ownership test are unchanged. Startup recovery adds a
  separate verification gate before their state machine begins.

## Revisit When

The installer supports concurrent operators or a transaction mechanism that
can atomically exchange the destination and rollback paths; either change would
replace the single-operator rename-recovery model rather than add another case.
