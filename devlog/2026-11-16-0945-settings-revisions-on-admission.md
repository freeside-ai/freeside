# Settings Revisions on the Admission, and the Wider-Allowlist Rule

Unit #3205 prepares running work for settings an operator edits while the
daemon runs (#2083 loads them from the store). Two decisions here change
Tier A contracts: every admission records the settings revisions it was
composed from, and the start and import path gate lets an in-flight run
continue after its allowlist is widened. The owner approved both with the
issue's plan.

## Decisions

- **Chose a record on each admission over a pin set once on the run.** A
  pin like `Run.Base`, written at the first admission, would say which
  settings a task started under, not which settings each attempt ran
  under. The daemon loads settings once per process, so after a restart a
  task's next attempt runs under whatever the new process loaded. Only a
  per-admission record lets an operator who edits a setting tell which
  attempts ran before the edit. The field is optional and enters the
  admission's content address under a new encoding label (v7), so an
  admission without it keeps its bytes and ID, and no stored row or
  existing golden changes. A composition from a settings file (the
  deputy) records none, so it stays `null` in production until #2083
  composes from stored settings.
- **Chose a subset rule over exact equality.** Start and import
  required a run's recorded allowlist to equal the configured one, so
  adding a pattern held every in-flight task. A daemon that runs for weeks
  needs a wider edit to leave running work alone. The gate now admits a
  run when every pattern it recorded still appears in the configured
  allowlist, and returns the recorded patterns, so import still enforces
  the run's own paths and never the wider set. A narrower edit that drops
  a recorded pattern refuses with `ErrPathBoundaryMismatch` and holds the
  run until the pattern returns, as any mismatch did before.
- **Chose pattern-for-pattern matching over glob containment.** Deciding
  that one glob covers another (`daemon/**` covers `daemon/internal/**`)
  is a second matcher on a Tier A gate, with its own edge cases, for a
  case an operator can avoid by keeping the old pattern beside the new
  one. Exact string match is the whole rule.
- **Kept intake on exact equality.** New work records the current
  allowlist, so the subset rule applies only to runs admitted before an
  edit.

## Refute-First Results

An independent reviewer tried to prove Part A wrong before it was
committed and found no defect. These claims were checked and disproved:

- The v7 label changes the ID of an admission without revisions: the
  existing goldens are byte-identical, because the v7 label is chosen
  only when the field is set.
- A copy path drops the revisions: every admission constructor copies
  the field, and the composed-admission and edit-after tests read it back.
- The subset rule widens what import enforces: import receives the
  recorded allowlist, not the configured one.
- A consumer depends on exact equality: none outside intake, which keeps
  it.

Two gaps were accepted: no test drives the one line that passes
revisions from the driver configuration into the admission environment
through a full composition, and the revisions are `null` in every
production admission until #2083.

Revisit when the daemon reloads settings without a restart, since the
per-admission record then needs a reload point inside a process.
