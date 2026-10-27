# Writer Image Selection Re-Gates Every Record and Moves Ward With It

Mandatory note: returned-object trust-boundary work (#2385). The writer
image now comes from the decoded admitted-image pointer and project-image
rows, and an admission records the conformance identity the backend
returns. The plan is the implementation-plan comment on #2385; this note
records what the implementation decided beyond it and the refute-first
findings.

## Decisions

- **Chose to re-gate every candidate over trusting the pointer.** The
  pointer and every image row are decoded state, so selection reruns record
  validity, the approved preparation command, and this binary's preparation
  digest on each candidate before choosing it, even one built at the base
  itself, where `AdmissibleAt` alone would take the record as it stands.
- **Chose to read the conformance identity per attempt over the digest
  `WithAdmission` cached.** An unattended admission records the digest the
  backend reports for the image it proves now, and refuses an image the
  backend does not prove. A cached digest would admit a repointed image
  under the old image's proof.
- **Chose to let a repoint before any proof keep restore open.** Repointing
  bumps the proof generation only once a pass has begun or a proof was
  restored. Startup repoints to the image it selected before restoring, and
  a bump there would refuse that image's own durable proof.
- **Chose to start a restoring daemon on the proved image, not the selected
  one.** A start without `-run-conformance` cannot prove a new image, so it
  starts ward on the image the latest passed record proves. The first
  attempt then moves ward through the follower. Starting on the selected
  image would fail the start for want of a proof.
- **Chose to hold the follower's window under `backend_not_conformant`.**
  Ward reports the new image as soon as it is repointed, before its pass
  ends. The selector holds an attempt while the follower is still proving
  that image, so no attempt is admitted between the repoint and the proof.
- **Chose to classify the lane's image waits in one place over recording at
  each call site.** The plan recorded the re-entry hold at its call site.
  Instead, the lane's retryable-failure branch records `image_upkeep_pending`
  for a pending re-entry or a busy builder, and `publication_environment`
  for any other back-off. Each wait is a retryable error, so the existing
  defer path paces it. An attempt that ends without waiting clears the
  cause. Clearing it at the attempt's start, as the attended hold is
  cleared, would restart the hold's span on every retry.
- **Chose to bind a re-entry fallback image only when it serves both the
  base and the verified commit.** The fallback skips the rebuild policy
  gate, which judges the candidate's own dependency change. Requiring both
  means the candidate changed no manifest the base holds, so there is no
  change for that gate to judge. The choice is written as the run's rebuild
  row keyed on the admitted image, so the checkpoint and remediation-source
  readers resolve the same image without a checkout.
- **Chose to keep a head-only re-entry on the old stop.** A cycle that
  verifies the head itself shares the rebuild row key (run, head) with the
  run's first cycle, so binding a fallback image there would rebind that
  cycle's checkpoint. Only a cycle that verifies a prospective merge takes
  the fallback.
- **Chose to keep preflight's base check on the configured image.** Moving
  `admitProjectImageAtBase` into preflight keeps today's operator check. The
  daemon no longer refuses that start, so preflight is now stricter than
  the start it checks.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed and fixed:** clearing the upkeep hold at each attempt's
  start restarted its span on every 30-second retry. It now clears after
  an attempt that does not wait again.
- **Confirmed and fixed:** a head-only re-entry could write the fallback
  row under the first cycle's key after a binary upgrade made the admitted
  image stop serving its own base (decision above).
- **Allowed by decision:** after a failed follower pass, ward names the new
  image with no capabilities, so attempts hold under
  `backend_capability_refused`, not `backend_not_conformant`, until the
  doctor's scheduled pass succeeds. The plan expected
  `backend_not_conformant`. The attempt still holds and the doctor retries.
- **Allowed by decision:** an attempt admitted under one image but not yet
  started is stranded once selection moves ward to another image, because
  the store's conformance gate refuses its digest. At a fixed base, ward
  moves only when its image stops keeping what the pointer binds, which
  only #2335 and #2338 can cause. The plan names this risk for those units.
- **Disproved:** a deadlock between `proofMu` and `imageMu`; a pass started
  before a repoint publishing or recording; an admission pairing one image
  with another's digest; a decoded row chosen without re-gating; the
  fallback skipping the rebuild policy gate; the pending error matching the
  re-entry stop; a selection error stopping the reconcile loop.

Revisit when #2335 or #2338 can change what the pointer binds while work
is running. A repoint then can fail an in-flight attempt's import at the
store's conformance gate.

Follow-up: #2401 (preflight), #2402 (follower retry).
