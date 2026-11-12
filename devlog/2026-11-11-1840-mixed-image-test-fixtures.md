# Test Fixtures Under the Project Image's Proof

Decision record for #3013. Freeside's recipe proof failed in its mixed Go
and Swift project image on three test fixtures, not on production code. The
proof runs each recipe command as root, with `HOME=/tmp/freeside-home`, in
an exact-commit tree with no `.git`; `check.sh` roots `TMPDIR` under
`HOME`. The note is mandatory because one fix changes what a Tier A gate
test feeds the macverify absence gate.

## Root Causes

- **Launch rig, `internal/exec/claude`.** `launchRig.script` built the
  launch command with the rig's absolute `bin/prepare` path, then relocated
  every `'/tmp/freeside-home` prefix under the rig root. With `TMPDIR`
  under that `HOME`, the rig's own path matched the prefix and was rooted
  a second time, so the five `TestLaunchCommand*` tests ran a `prepare`
  that didn't exist.
- **Fake slot uid, `internal/macverify`.** `fakeLauncher` reported the test
  process's uid as the slot uid: 0 under root. `Room.teardown` reads a
  prepared uid of 0 as "prepare reported none" and skips its
  reaped-versus-prepared check, so the "uid changed" case of
  `TestRunQuarantinesSlotWhoseAbsenceIsUnproven` ran clean.
- **`go` stand-in, `scripts/test-check.sh`.** The `partition-tests` stand-in
  ran `git status` on every call to emulate Go's VCS stamping, including
  the `daemon build` call one case makes, which runs in the bare tree with
  no decoy `GIT_DIR`.

## Decisions

- **Chose a stand-in slot uid over skipping the case under root.** As
  root, the fake reports uid 4242 and chowns each job directory to it, as
  slot-run's prepare does. The "uid changed" and "other uid" cases offset
  from that uid, so both refuse whether or not the test runs as root.
  Skipping the case under root would disable a gate test, which the Tier A
  rules count as removing it. Off root, the fake feeds the room exactly
  what it fed before.
- **Kept `Room.teardown`'s zero-uid sentinel.** Changing it is production
  work the unit excludes, and the input can't occur: `parseUID` refuses
  any slot uid that isn't a positive integer.
- **Chose a placeholder over reordering or narrowing the relocation.** The
  command is built with a fixed `/launch-rig/bin/prepare`, which the same
  single-pass replacer swaps for the rig's real path. A `strings.Replacer`
  never rescans what it inserts, so the real path is never relocated, and
  the command stays byte-identical wherever the rig isn't under a
  relocated prefix. A new test builds the command for a rig under
  `/tmp/freeside-home`, so the case is checked on macOS too, where the
  five tests skip for want of `mv -T`.
- **Stamping is emulated only for `go test`.** Only a test step runs under
  `check.sh`'s decoy repository, which is what the decoy cases check.
- **Deferred a fourth failure to #3100 over widening this unit.** With
  these fixes, the proof stops at `scripts vocabulary`: in a tree without
  `.git`, `check-vocabulary.sh` reads the Swift checkouts preparation adds
  under `app/.build/`. Its fix belongs in that script, a protected
  `verification_control` path outside this unit's declared paths. With
  that step dropped from the recipe, the whole proof passed.

## Refute-First Findings

A fresh-context reviewer tried to break the diff; nothing was confirmed.

- **Disproved:** replacer priority could let a relocation key win over the
  placeholder. The placeholder is the first pair and no relocation key is
  a prefix of it.
- **Disproved:** the new launch-rig test passes on the old code. Restoring
  the old `script` line fails it.
- **Disproved:** the macverify change weakens a case under root. In a Linux
  container as root, main's fixtures fail "uid changed" and the five
  launch tests; the fixed ones refuse "uid changed" (prepared 4242,
  reaped 4243) and "other uid" (owner 4242, reported 4243).
- **Disproved:** a `test-check.sh` case needs `git status` outside
  `go test`. The only other calls under the stand-in are the two
  `go build` calls in the bad-partition-count case.
- **Accepted:** a runtime that maps only uid 0 into a user namespace would
  refuse the chown to 4242. Every macverify room test would then fail
  loudly, not pass weakly. Docker, rootless podman, and Apple `container`
  map it.

Revisit when the proof's container stops running as root, or when slot-run
can report uid 0.
