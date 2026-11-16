# Ward Holds a Proof per Configuration

Unit #3211 (tracker #3201) makes ward hold one conformance proof per
writer image, and makes admission judge each attempt by the newest record
for the configuration its admission names. Before it, ward proved one
image at a time, and proving a second project's image cleared the first
one's declaration, so every project's attempts waited while ward
re-proved for another's. Plan revision 127 records the commitment; this
note keeps the decisions that rejected a reasonable option, the older
rule it narrows, and the gate tests it changed. The owner handed the
unit to implementation with its plan on 2026-11-16.

## Decisions

- **Per-configuration reads over a per-backend newest row.** The store's
  admission gate, its authenticate re-check, and the doctor now read
  `LatestConfigurationConformance(backend, digest)`. A supersession marker
  or a failed pass for one configuration no longer refuses another's
  attempts. Rejected: keeping the per-backend newest row and proving
  images in turn, which leaves every project waiting on whichever image
  ward is proving.
- **A failed pass closes only its own configuration.** This narrows the
  plan's "a failed pass records the failure and invalidates the
  declaration". A fault the pass found that every image shares, such as a
  broken egress path, closes the other configurations only at their next
  scheduled re-proof or at a failed pre-job probe. Rejected: closing every
  configuration on any failure, which lets one project's broken image stop
  all of them. The pre-job probe still runs before every unattended job,
  so the window is a probe's reach, not a scheduled interval.
- **The admission authority checks the configuration directly.** The
  per-backend newest row was what stopped an attempt admitted under an old
  configuration from starting after a restart under a new one. With
  per-configuration reads that protection is gone, so
  `admissionauth` now refuses unless the daemon yields the admission's
  configuration for its image (`BackendConfigurationDigestFor(ImageRef)`).
  An admission recorded with no configuration digest refuses with
  `ErrAdmissionConfigurationMismatch` in the store; before, it failed only
  because no backend-wide row matched an empty digest.
- **`/v5` over a migration for stored proofs.** Every stored proof was
  written under the per-backend rule, where a later row for image B
  cleared an earlier pass for A. Read per configuration, A's old pass
  would admit again. Moving the configuration version to
  `freeside.ward.conformance-configuration/v5` makes every stored digest
  unmatchable, so each configuration proves once on first use after the
  upgrade. Rejected: a migration that appends a marker per old digest,
  which rewrites Tier A history to reach the same result.
- **Retirement by marker.** At the scheduled pass the follower re-proves
  each published proof selected since the previous pass, and always the
  most recently selected image; it retires the rest by ward's begin step
  with no pass after it. The marker is appended before the in-memory
  proof is cleared, so a retired configuration stops admitting in the
  store and is not restored at the next start. This bounds the scheduled
  pass at the images in use instead of every image ever proved. Rejected:
  re-proving every tracked image, which grows the round by a full suite
  per image the daemon ever ran.
- **A recorded attempt keeps its image's proof alive (beyond the plan).**
  Dispatch starts a recorded attempt under its stored admission and never
  re-selects, so the selector alone never learns of it. An attempt
  recorded on a retired image would hold for good. The unattended pre-job
  hook therefore calls the follower's `use(image)`, which marks the image
  in use and requests its proof when none is published. The scheduled
  pass also always re-proves the latest image, so a quiet window never
  retires the one image the next attempt will most likely want.

- **Only a backend with no passing configuration blocks everything.**
  The doctor files an unhealthy `conformance` or `workspace_handoff`
  finding as a blocking `system_health` item, which holds every
  unattended attempt. Those findings are healthy while any tracked
  configuration passes, and their detail names each configuration's
  verdict; the store gate holds the failing configuration's attempts.
  Likewise a failed scheduled re-proof fails the doctor pass, which stops
  the daemon, only when the round leaves ward no published proof, and an
  unproved image's hold is per invocation, so it does not end the dispatch
  pass for attempts behind it. Rejected: keeping the doctor healthy only
  when every configuration passes, which turns one project's failed or
  running pass into a stop for all of them.

## Refute-First Findings

A fresh-context reviewer read the diff against the contract.

- **Confirmed and fixed:** an unproved image's hold ended the whole
  dispatch pass (it classified as a pass refusal), so attempts on proven
  images behind it waited. It now holds per invocation, at selection and
  at the pre-job gate.
- **Confirmed and fixed:** the doctor's all-or-nothing rule filed a
  blocking item while any one configuration was unproved, including one
  whose pass was running when the doctor read.
- **Confirmed and fixed:** the scheduled round re-proved the latest image
  inside its backoff, and any one failed re-proof stopped the daemon.
- **Allowed:** a retirement marker is the same `superseded` row a recheck
  writes, so the authenticate gate tolerates it at restart recovery of an
  attempt already seeding or running, though no pass may follow. Such an
  attempt passed the pre-job gate under a live proof before its intent
  was written, the authority still checks that the daemon yields its
  configuration, and an attempt that runs without a restart is never
  re-checked mid-run either, so the tolerance grants nothing a run
  without a restart lacks.
- **Allowed:** an image selected while the scheduled round is retiring
  can still be retired. The attempt holds and the image proves again on
  its start: one extra pass, no admission.

## Accepted Risk

- **Stacked markers after a retirement.** The next proof of a retired
  configuration writes a second marker, and the authenticate re-check
  tolerates only one same-configuration marker (#761). An attempt admitted
  before the retirement then holds until its configuration passes again.
  Such an attempt is older than a scheduled interval, and the refusal is a
  hold, not a stop. `TestAuthenticateRefusesRestartStackedSupersession`
  and `TestAuthenticateRefusesStackedMarkersAcrossAnotherConfiguration`
  pin the behavior.
- **The first pass after an upgrade blocks briefly.** The doctor judges
  every configuration ward tracks, including one whose pass is running.
  Until the first `/v5` pass passes, no configuration does, so the doctor
  reads unhealthy during it, as the repoint it replaces did.

## Gate Tests Changed

- **Store:** replaced
  `TestUnattendedAdmissionRequiresTheCurrentBackendConfiguration`,
  `TestAuthenticateRefusesDifferentConfigurationSupersession`,
  `TestAuthenticateRefusesInterveningOtherConfigurationPass`, and
  `TestAuthenticateRefusesStackedCrossConfigurationMarkers` with
  `TestUnattendedAdmissionIgnoresAnotherConfigurationsProof`,
  `TestAuthenticateIgnoresAnotherConfigurationsMarker`,
  `TestAuthenticateToleratesRecheckAcrossAnotherConfigurationsPass`, and
  `TestAuthenticateRefusesStackedMarkersAcrossAnotherConfiguration`, which
  assert the per-configuration rule. Added
  `TestUnattendedAdmissionAdmitsEitherProvenConfiguration` and
  `TestConfigurationRefusalLeavesOthersAdmitting`.
- **Admission authority:**
  `TestReleasedImportKeepsCurrentPolicyAcrossBackendChange` now starts
  beside another configuration's newer proof, and refuses when the daemon
  yields another digest for the admission's image or has no digest
  function.
- **Engine:** `TestAdmissionRefusesAnImageTheBackendDoesNotProve` asserts
  the hold for an image with no published proof, and that the dispatch
  pass goes on to the intent behind it. `TestHoldClassificationGolden`
  adds the not-proved class and its two joined shapes.
- **Ward:** the repoint tests and the two per-capability pre-job tests
  became `TestProofsAreHeldPerImage`, `TestRestoreIsPerImage`,
  `TestRestoreRefusesAPreviousConfigurationVersion`,
  `TestSuitePreJobRequiresThePublishedProof` (both suite-earned
  capabilities now come from one published proof, so one case covers
  each), `TestBackendPreJobProbesTheAttemptsImage`, and
  `TestRetireConformanceWritesTheMarkerFirst`.
- **Doctor:** the case with a second, unproved configuration now expects
  healthy findings that name it, and a new case with only that
  configuration expects both findings unhealthy.
- **Startup:** `TestUnattendedStartupRequiresExactPassingConformance`
  became cases of `TestStartClaudeConformance` (no proof, a failed
  configuration, and a retired one each refuse an unattended start).

Revisit when a fault class shared by every image (egress, the exporter)
shows up in practice as a failed pass that later configurations missed;
then a shared-fault pass result could close every configuration at once.
