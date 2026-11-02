# Probe Host Credential Copies

Work unit: #1778. Credential-leak surface, so this note records the
refute-first pass the AGENTS.md mandatory-note list requires.

## Decision

Chose to name each older ward live probe's host credential copy in its
recovery manifest before writing it, and to delete it once the seeder has
copied it into the snapshot volume, over keeping the copy for the whole run.
This is #1775's shape in the Codex usage probe, copied rather than shared;
#1776 owns the shared helper.

The Claude probe's post-run unchanged-auth check read the host token file, so
the copy could not be removed early without changing that check. Chose to read
`/probe/token` back from the still-running observer through a private bounded
pipe, the way the capture is read, over two alternatives:

- Keeping the host file until the check. A killed run would still leave the
  operator's real token in an unnamed or late-removed host file for the whole
  run, which is the defect being fixed.
- Dropping the Go-side check and relying on the fixture's in-container
  `authUnchanged`. That check is reported by the code under test; the Go check
  reads the volume independently.

The read-back adds one in-memory path for the real token. It never reaches a
log, a `t.Fatal` message, or a file, and every error string is fixed.

## Refute-First Findings

An independent reviewer, given only the diff and the issue's acceptance list,
tried to prove the change wrong.

- **Copy present while the manifest says `removed`:** disproved by reading.
  `removed` is saved only after `os.Remove` succeeds. Every kill window leaves
  the manifest naming a path whose file may already be gone, never the
  reverse. The synthetic Codex account rerun sampled each case's manifest and
  saw the path, then `removed`, in all three cases.
- **Token in output:** disproved by reading and by grep. The exec's stderr is
  discarded, error strings are fixed, and the synthetic rerun's `-v` output
  holds no token, `auth.json` field name, or synthetic account ID.
- **Observer unreadable or stopped at read time:** disproved. The observer
  sleeps 90 seconds after `/completed`, and `container exec` runs as root, the
  user that wrote `/snapshot/token` 0400 and that the capture read already
  depends on.
- **Byte mismatch from trimming:** disproved. The host write is the raw token,
  `cp` keeps the bytes, and the Go comparison does not trim. A read truncated
  at the 64 KiB bound mismatches and fails closed.
- **Seeder `rm` failure is not distinct:** allowed. In the Claude seeder's
  background command, `set -eu` stops before `/seeded`, so the test fails on
  the marker timeout rather than a removal message. Existing `cp` failures
  there behave the same, and nothing leaks.
- **Stale path after an ordinary failure:** allowed. When a run fails with
  resources still pending, `t.TempDir` cleanup deletes the copy while the
  retained manifest still names it. The record points at a missing file, which
  is the safe direction, and matches #1775.

Not verified: the Claude probe was not rerun live, since every case uses the
operator's real setup token. Whether Apple `container cp` stages its own host
copy is runtime behavior outside this unit.

Revisit when #1776 moves the lifecycle into a shared helper.
