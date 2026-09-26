# Attended-Dev Admission Floor for Ephemeral Driverless Daemons

Issue #1503. Owner decision, chosen from the options below.

## Decision

Chose to configure the attended_dev admission floor whenever the daemon runs
in the `ephemeral` tier with `-driver disabled`, not only when
`-seed-fixture` is passed. `config.storeOptions` adds it; every other
disabled or fake configuration keeps no floor, and `-driver claude` keeps its
mode-specific floor unchanged.

## Finding

The representative fixture needs admission-backed runs: a run past
`run_submitted` (running, ready, completed) reads only with its
`invocation_admitted` record, and the store re-gates every recorded admission
against `store.Options.AdmissionFloors` on write and on read. A missing floor
is "no policy configured", which fails closed. A driverless daemon had no
floor, so seeding refused the admissions, and a store seeded with one would
fail its runs closed on the next start.

## Rejected Options

- **Floor only while `-seed-fixture` is set.** Seeding works, but restarting
  the same ephemeral store without the flag (the normal way to keep
  inspecting it) fails every admitted run closed on read.
- **Drop the admission-backed runs.** Keeps the store policy untouched, but
  the fixture could then show only specification and failed runs, which
  defeats its purpose: the app's running, ready, and completed states.
- **Floor for every driverless daemon.** Would reach dev and prod stores,
  whose admissions should only ever come from a configured driver.

## Why It Is Safe

An ephemeral driverless daemon starts no engine, so it admits nothing
itself; the only admissions its store can hold are seeded fixture records or
rows copied from elsewhere, and the floor is the same attended_dev minimum a
claude-mode attended daemon enforces. `-seed-fixture` is refused outside the
ephemeral tier and with any driver, before the store opens.

## Revisit When

- A driverless ephemeral daemon gains any path that admits execution, or the
  ephemeral tier starts serving stores copied from a supervised tier.
- Fixtures need unattended-mode admissions, which would need their own floor.
