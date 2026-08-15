# Project Run Observation Timestamps on the Wire

Work unit: #775 (`kind:contract`, `lane:spine`). This mandatory note records
the sync-contract and presentation decisions for run ordering.

## Decision

Chose required-present, nullable `created_at` and `last_activity_at` fields on
the wire `Run`. Both derive during the existing authenticated observation
projection, so they carry no independent workflow authority and require no
storage migration or runtime write. `created_at` is the `run_submitted`
milestone's `recorded_at`; `last_activity_at` is the newest milestone
`recorded_at`, invocation `observed_at`, or current hold `last_observed_at`.

The issue plan described `created_at` as null only when every milestone is
absent. The existing authenticated corpus also permits retained admission or
invocation-start milestones without a retained `run_submitted` milestone, so
the precise contract is null when `run_submitted` is absent. Such a run can
still have a non-null `last_activity_at`. Whenever both fields exist,
`last_activity_at` is at least `created_at` because the reduction includes the
submission milestone.

Chose client-owned presentation ordering: activity (falling back to creation)
descending, nulls last, then run ID ascending. The store retains its canonical
ID order, while every consumer of the wire fields receives the same durable
facts and can choose its own presentation.

## Rejected Alternatives

- Persist timestamp columns on `runs`: rejected because migration 0024
  already stores the authoritative observation facts and projection can reduce
  them without another write path.
- Sort the store list by activity: rejected because activity is a joined
  projection concern and store collection order is deliberately primary-key
  canonical.
- Derive timestamps in the production app: rejected because it would require
  transferring the whole timeline and duplicate daemon domain logic. The app
  mock derives its fixture fields from its timeline solely to preserve mock
  parity with the daemon boundary.

## Refute-First Findings

The daemon lens found no remaining projection, authentication, reconstruction,
JSON-nullability, or storage-boundary defect. It verified that every run read
authenticates observation facts before deriving the new fields and that invalid
or non-UTC persisted facts still fail closed.

The app lens found two mock-boundary gaps. Synthesized Swift encoding omitted
required nullable fields when their value was nil, and the first parity test
seeded already-correct timestamps, so it could not disprove trust in caller
values. Both were confirmed and fixed: run-bearing mock responses now restore
explicit null keys in raw JSON, with raw transport coverage, and a forged-seed
test verifies list, detail, and bootstrap projection from contradictory timeline
facts, including invocation-newer-than-hold and no-observation cases. The
follow-up lens cleared both fixes after inspecting the raw response and rerunning
all seven sync-surface tests.

## Revisit When

Observation history gains compaction or pagination. The projection must then
preserve these aggregate timestamps without assuming the complete history is
materialized in one read.
