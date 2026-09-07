# Presenting Specification Handoff and Retry Supersession

Chose source-run classification over comparing attempt numbers or fetching a
successor for #1196. This preserves the daemon decision in
[the specification supersession note](2026-09-06-2009-specification-run-supersession.md):
`superseded_by` names the run that now owns the work, either a retry or an
implementation run taking over an approved specification.

A handoff has campaign and attempt metadata, finished lifecycle, a successor,
and exactly one canonical specification stage. The specification producer
creates that stage shape, and retry reconstruction requires an implementation
parent. Equal attempt numbers alone carry no such meaning. The source shape
works with a partially loaded list; the successor snapshot only supplies its
attempt label, with the existing raw-ID fallback when absent.

Finished lifecycle suppresses every current-stage marker. Only the pending
stage of an approved specification handoff becomes completed. A superseded
implementation with pending or blocked outcome stays pending because transfer
of ownership does not prove successful execution. Failure and completion
outcomes retain their existing rail states; unrecorded stages remain pending.

## Refute-First Check

An independent review tried the counterexample of a specification retry
masquerading as a handoff. The daemon's production-attempt write and
reconstruction checks require an implementation parent, disproving that path.
The invariant comes from those producer checks, not from Swift decoding.
The client presents the daemon's binding; it does not establish a new
authorization or completion outcome.

Revisit when #1083 changes specification identity or liveness, or when stage
construction, retry-parent rules, or supersession producers change. A wire
handoff-kind field would then be preferable if the source shape no longer
identifies the daemon's handoff unambiguously.
