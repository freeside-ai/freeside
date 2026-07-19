# Exporter image, evidence emission, and check-5 decouple (#170 + #190)

Combined co-designed unit, 2026-07-19. #170 (ward: build and conform the real
`freeside-export` image) declared a dependency on #190 (gauntlet: emit the
evidence channel from the helper), and the maintainer directed co-designing
#190's agent-workspace evidence source descriptor with #170's workspace layout,
so the two land together. Trust-boundary work (a new untrusted-input surface and
a returned-object trust boundary) plus a safety-conformance change, so the
refute-first rule applies.

## Decisions and rejected alternatives

- **Evidence staging is a reserved `.freeside-evidence/` workspace subtree,
  excluded from the repo channel.** The agent stages transient per-run evidence
  *claims* (they become `AgentClaim`s, never the PR diff; importer.go) under a
  reserved top-level subtree the export walk skips (like the workspace's own
  `.git`), so evidence never enters the repo-change channel and cannot pollute
  the candidate diff or git history. Rationale confirmed on merits: the
  read-only workspace is the *sole conduit* across the §5.6/§5.7 trust boundary
  (the credentialed agent VM is gone; only the workspace volume reaches the
  fresh exporter VM), so evidence must be workspace bytes, and a repo-excluded
  reserved subtree is the physical channel separation §5.6 requires. Rejected:
  evidence sources anywhere with a per-file exclusion list (couples the two
  channels' code paths for no benefit); a side evidence volume (identical trust
  properties, more lifecycle); verifier-only capture (deletes the agent-claims
  channel that #167/#173 already built).

- **The reserved name is `.freeside-evidence/`, NOT `.freeside/`.** `.freeside/`
  is the trusted control-plane directory (`.freeside/verify.json` is the verify
  recipe path; `.freeside/recipe.yaml` is control-plane config). Reserving it
  for untrusted agent evidence would be a category error and would break
  control-plane change detection (the importer's `TestImportControlPlanePaths`
  caught the collision). Evidence is untrusted agent output and gets its own
  namespace, distinct from control-plane configuration.

- **The descriptor is a new schema, agent-authored, strict-decoded but NOT
  canonical-or-reject.** `EvidenceSourceManifest`
  (`freeside.export.evidence-source/v1`, export/evidence_source.go) is distinct
  from the #173 evidence *wire* manifest (unchanged). The agent writes arbitrary
  valid JSON (not a trusted `Encode` output), so requiring byte-canonical input
  would reject legitimate descriptors; strictness instead comes from a UTF-8
  pre-check, `DisallowUnknownFields` (a smuggled `producer_class`,
  `publish_eligible`, or `verification_recipe_digest` is malformed input),
  EOF-after-one-value, and full validation. `producer_class` is absent from the
  schema and forced to `agent` by the emitter; the decoded value is re-emitted
  canonically as evidence.json and independently re-gated by the importer's
  canonical-or-reject `DecodeEvidenceManifest`, so a last-wins duplicate key
  cannot smuggle an unvalidated value downstream.

- **Symlink-safe source resolution by per-component lstat.** `lstatUnderReserved`
  (evidence_emit.go) resolves each declared path one component at a time,
  requiring every intermediate component to be a real directory (never a
  symlink) and the final to be a regular file, so an intermediate symlinked
  directory, a symlinked source, a symlinked `.freeside-evidence`, `..`, or an
  absolute path cannot redirect the read outside the read-only workspace. This
  mirrors the walk, which likewise never follows a link. Emission fails the
  whole export closed on any malformed/missing/non-regular/over-cap source,
  because the evidence schema cannot omit a blob, so a partial or lying channel
  is never emitted (an absent descriptor is the benign pre-evidence shape).

- **Check 5 moves from the per-handoff path to a conformance-time probe.** The
  handoff exporter now runs only the trusted helper (`export.HelperCommand()`),
  which emits the channels but no environment proof, so `verifyExport` no longer
  requires `/handoff-proof.txt`. Check 5 (the in-VM ro-mount / write-blocked /
  credentials-absent / host-home-absent proof) is attested at conformance time
  by a dedicated probe (`probeInExporterVerification` in `Suite.Full`) running
  the real `verifyProof` gate, symmetric with the existing network-free probe.

  **Safety argument:** per-handoff safety is not weakened, because check 4
  (`verifyExporterAllowlist`) runs on *every* handoff, inspect-before-execute,
  and structurally asserts the same topology the proof observes: exactly one
  read-only workspace volume mount (⇒ workspace_mounted=ro, workspace_write
  blocked, credentials absent), no host bind expressible in `ContainerSpec`
  (⇒ host_home absent), zero network. The one property check 4 cannot see is
  guest-kernel *behavioral* enforcement of the ro remount / path absence; that
  in-VM behavioral attestation moves to conformance cadence (§5.7 is the
  capability gate: startup / config change / doctor), exactly as the
  network-free capability already relies on. With a digest-pinned image the
  attestation is *stronger* than the retired per-run shell payload, because it
  proves the real image, not a per-run script.

  **Residual risk (accepted):** per-handoff runs no longer confirm the in-VM
  ro-enforcement / path-absence behaviorally; they rely on check 4's host-side
  inspect plus check 7's digest/scan. Acceptable because the exporter image and
  its spec shape are fixed by digest and generation, so the in-VM view is an
  image+runtime property proven at the §5.7 cadence, not a per-handoff variable.
  Revisit if the exporter image or mount generation ever varies per handoff.

- **Ward verifies both channels.** `verifyExport` now decodes and digest-verifies
  the evidence channel (canonical-or-reject + re-hash each `evidence/sha256`
  blob) alongside the repo channel, and a single `verifyNoStrays` covers both,
  so a stale/planted `evidence.json` or `evidence/` blob with no declared
  channel is an orphan and fails closed (matching the importer's own layout
  audit). This is check 7's "verify exported digests" over both §5.6 channels.

- **Image provenance and the registry finding (accepted limitation).** The
  exporter image (`images/exporter/`, `scripts/build-exporter-image.sh`) is a
  pinned busybox base plus the static helper at `export.HelperPath`. The busybox
  shell is a required capability, not an attack surface: the exporter VM is
  fresh, credential-free, network-disabled, destroyed after one use, and the
  conformance probes (networkless + check 5) run their shell scripts in this
  same image. **Ward requires a digest-pinned image, and Apple `container` 1.1.0
  resolves a digest reference only through a registry** (a locally-built image
  runs by tag, not by digest); its `container image push` to a local `registry:2`
  also fails on 1.1.0 ("Network is down", nothing stored). So the live
  conformance cannot run the pinned exporter image from the local content store
  alone: it needs the image in a registry `container` can pull from. The chosen
  "local ephemeral registry, no external push" path is therefore infeasible with
  current tooling; the live suite consumes `FREESIDE_WARD_EXPORTER_IMAGE` and
  the live exporter-image members are a **Not-run gap** in this PR (the
  container-free Tier-1 real-helper→import test proves the two-channel interface
  in CI regardless). Revisit when `container`'s registry/networking matures, or
  push the image to ghcr to close the live gap.

## Refute-first verification

A fresh-context adversarial reviewer was prompted to disprove seven attack
lenses: symlink/path escape in emission, repo-exclusion bypass, forged-provenance
/ trust-bit smuggling, cap evasion / overflow, ward two-channel stray handling,
the check-5 decouple safety argument, and the emitted canonical form.

**Confirmed and fixed:** one fail-safe spec/deployment divergence. "A malformed
evidence declaration fails the whole export closed" held for `export.Export` in
isolation but not end to end: `Export` wrote the repo channel before emitting
evidence, so a bad descriptor errored only after `manifest.json` was on disk, and
ward (which verifies output, not the exporter's exit status) would ship a valid
repo-only handoff that silently dropped the evidence. Fixed by splitting emission
into `resolveEvidence` (read/decode/validate/resolve every source, no writes) and
`writeEvidence`, with resolution run before the repo channel is written, so a
malformed declaration now fails before any manifest exists and the ward gate
fails the whole handoff closed. `TestEmitEvidenceHostile` now pins that both
`evidence.json` and the repo `manifest.json` are absent on failure.

**Rejected by verification (attacked, not refuted):**

- Symlink / path escape: `lstatUnderReserved` lstats every path component
  (intermediate symlink or non-dir → hard error; final-component symlink →
  non-regular), and `os.DirFS` on Go 1.26 implements `fs.ReadLinkFS` so `fs.Lstat`
  genuinely does not follow links and `Open` refuses a root-escaping symlink. The
  read-only mount + terminated writer + `res.size != size` recheck close TOCTOU.
- Repo-exclusion bypass: the walk skips exactly the top-level `.freeside-evidence`
  before `Info()`, so it is excluded whether dir, file, or symlink, and a
  non-dir/symlink `.freeside-evidence` additionally fails emission closed;
  channels use physically separate blob stores; "hiding" is a non-threat (the
  agent owns its whole workspace).
- Forged provenance: no `producer_class` field exists; `provenance()` hard-codes
  agent; `DisallowUnknownFields` rejects smuggled trust bits; the output is
  re-gated canonically by ward and again by the importer.
- Cap evasion / overflow: headroom comparison cannot underflow or wrap; the
  descriptor read is `LimitReader`-bounded regardless of on-disk size;
  self-reference is blocked.
- Ward two-channel verification: `verifyEvidence` re-hashes every mandatory blob
  through the canonical-or-reject decoder; the combined `verifyNoStrays` allows
  `evidence/` only when `evidencePresent`, so an orphan fails closed; repo strays
  still rejected.
- Emitted canonical form: entries are a non-nil `make` slice, sorted, and written
  via `Encode`, whose bytes are exactly what `DecodeEvidenceManifest`'s
  byte-equality gate requires; the empty-descriptor edge is rejected upstream.

**Accepted by decision:** the check-5 residual risk above (per-handoff no longer
confirms in-VM ro-enforcement behaviorally; relies on check-4 host inspect +
check-7, with check-5 attested at the §5.7 conformance cadence).

## Revisit when

- Apple `container` can resolve a local-only image by digest, or its registry
  push works: run the live exporter-image conformance and close the Not-run gap.
- A concrete need to widen the evidence media-type allowlist beyond images
  (importer, §5.15 rule 3) or to allow nested evidence directories arrives.
- The exporter image or mount generation ever varies per handoff: the check-5
  residual-risk acceptance above must be revisited.
