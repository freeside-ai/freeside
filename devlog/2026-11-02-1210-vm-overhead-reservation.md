# Reserve a Per-VM Overhead With Each Launch (Plan Revision 105)

Chose to reserve each ward launch's declared memory plus one fixed per-VM
overhead over reserving declared memory alone. The overhead is a
control-plane policy value per container runtime, recorded with the runtime
version it was measured on: 256 MiB for Apple container 1.1.0. The owner set
the rule and value in #2188's work contract, from the ward density spike
(`docs/spikes/ward-density.md`, Memory-Budget Recommendation).

## Why

- **Declared memory undercounts every launch.** On Apple container 1.1.0 an
  idle ward VM costs the host 240 to 285 MiB in all. Once a build has touched
  the guest it costs its declared size plus 137 to 155 MiB, and it keeps that
  until the container is deleted, because freed guest memory isn't returned.
  Half of a 32 GiB host fits seven 2048 MiB writers once overhead is counted,
  not eight.
- **One overhead per launch is enough.** No launch runs two container VMs at
  once. The writer deletes its agent before the post-writer helpers start
  (`daemon/internal/ward/handoff.go`), a review starts no helper once its
  review container runs (`codex_review_lifecycle.go`), and a verification
  job runs its preparation and command containers in turn
  (`verification_room.go`). Checked at `c4235c41`.
- **A fixed value, not a conformance measurement.** Conformance probes boot
  idle VMs. The overhead that matters appears only after a build has filled
  guest memory, so a probe would need a build-sized workload to see it. The
  spike's host was at memory-pressure warning throughout, and a number read
  on a loaded host would make admission depend on noise. A fixed value per
  runtime version keeps admission deterministic; recorded peaks are the
  evidence for changing it.
- **A runtime version with no value admits nothing.** A host whose runtime
  and version have no recorded overhead is eligible for no launch, rather
  than reserving a guess, so a runtime upgrade can't silently reuse the
  old version's value.
- **256 MiB, not the measured 155.** The spike built one image at one size
  (the 2048 MiB writer default). The margin covers images and sizes it didn't
  build.

## Rejected

- **Declared size alone.** The current rule; it undercounts every launch.
- **Recorded peak instead of declared size.** A VM that built once holds its
  declared size until delete, so reserving a lower peak overcommits.
- **Measuring the overhead per runtime at conformance time.** See above.
- **One overhead per container.** It reserves for VMs that never run at the
  same time and would shrink every budget by a helper's worth for nothing.

## Consequences

- **#1598 implements this, not the old rule.** Its Objective ("reserve its
  declared memory") and Acceptance (the never-fits checks and the
  larger-size retry) need the overhead before it starts. The stale "roughly
  100 MiB" comment on `ContainerSize` in `daemon/internal/ward/sizes.go`
  belongs to #1598.
- **The rule depends on one VM at a time.** If a launch ever starts a helper
  beside its running main container, the rule becomes one overhead per VM
  that runs at once.

Revisit when the concurrent density measurement (#2187) reports, or when the
ward moves off Apple container 1.1.0 (the 1.6.0 upgrade brings a new guest
kernel and needs its own value).
