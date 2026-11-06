# Deputy Writes The Stop Coverage Record Before Its Manifest (#1920)

## Decision

- **Chose to open `task-runtime-coverage.json` in `freesided deputy` before
  its self-check writes `composition-manifest.json`.** The step holds the
  database daemon lock while it lists tasks and writes the record, then
  releases it, because the preflight takes the same lock. `run()` and the
  deputy open the record through one helper with the same state directory, so
  a later open reads the deputy's file unchanged.
- **Rejected treating a task with no dispatched invocation as provably
  stopped.** A pre-job probe can start before the stage intent is recorded
  (`2026-09-17-1200-task-runtime-stop.md`), so a missing stage attempt proves
  nothing for a task saved before the record.
- **Rejected a database "first serve" marker.** It needs a migration and a
  contract unit for a case that only a manual pre-start submit reaches.
- **Rejected a daemon-supplied failure reason on the API.** That changes
  `TaskCancellation`, `api/openapi.yaml`, and the generated client. The client
  instead says what `failed_to_stop` always means: the fence starts no new
  work, and running work isn't confirmed ended. A reason field gets its own
  `kind:contract` issue if that text proves too thin.

## Finding

The issue premised that a `--require-composition` submission could land in
the gap between the deputy's manifest and `run()`. It can't on `main` at
`26f8f96b9`: since `143f948cd`, `submit` refuses a launch manifest when no
daemon serves the database, and a serving deputy has already opened the
record. The early record still closes the gap for an offline `submit` that
names no launch manifest during the start, and it keeps the record ahead of
the manifest without relying on that refusal. The Wave 8 evidence came from
the retired `scripts/run-real-work.sh`, which submitted before the daemon
started.

A task with a run saved before a root's first daemon or deputy start stays
unprovable, by design: no recorded fact proves that no other build or state
directory ran work for it. A task with no run has nothing to stop, and its
Stop is confirmed.

Revisit when `submit` accepts a launch manifest with no serving deputy, when
the deputy and `run()` stop sharing a state directory, or when the self-check
starts task work.

Scope: `daemon/cmd/freesided`, `app/`, `docs/`, `devlog/`.
