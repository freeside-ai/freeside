# Ward image allowlist: bind to the reference's pinned digest, not the descriptor

Work unit: #143 (ward lane, `kind:fix`, fiat-assigned). Mandatory note:
returned-object trust boundary (the `container inspect` report is
attacker-influenced and the image half of checks 2/4 re-verifies it) and a
consequential decision that rejects a plausible alternative (which observed
field is the authoritative pinned digest).

## The defect

Checks 2 (writer) and 4 (exporter) re-inspect the runtime-realized container
before it executes and bind the observed image to the digest-pinned spec. The
comparison (added in #76/#129 rounds 16 & 19) assumed a `container inspect`
shape Apple container 1.1.0 does not produce for a digest-pinned image:

- It expected `configuration.image.reference` to be the **bare** name and
  `configuration.image.descriptor.digest` to be the pinned digest.
- 1.1.0 actually reports `reference` as the **full pinned reference**
  `name@sha256:<PIN>` (any tag dropped, the pinned digest kept), and
  `descriptor.digest` as a **different**, arch-resolved digest.

So every reference-runtime handoff failed check `control_plane_isolation` /
`exporter_allowlist` with "reported the wrong image". The merged
`TestLiveHandoffLifecycle` failed identically; CI stayed green only because the
fake and the fixtures were written to the same wrong assumption
(`runtime_fake_test.go` split the spec image the way the buggy verifier did;
`testdata/cli-inspect-volume.json` even carried a tag-only, unpinned
`reference`). Surfaced during #77 verification; a pre-existing #76 defect.

## Decision: the pinned digest is the one in `image.reference`

The authoritative pinned digest is the `@sha256` **inside
`configuration.image.reference`**, not `descriptor.digest`.

- The spec pins the image by that digest; 1.1.0 echoes exactly it in
  `reference`. It is the only observed field we can predict from the spec.
- `descriptor.digest` is the runtime's arch-resolved digest (the platform
  manifest / index resolution). It is unpredictable from the spec, differs from
  the pin on a multi-arch image, and therefore cannot be a comparison anchor.
  Content-addressing makes the pinned (index) digest transitively commit to the
  resolved manifest, so binding to the reference digest loses no integrity.

Consequences:
- `descriptor.digest` is dropped from the decode entirely (`cliImage` no longer
  has a `Descriptor` field; `InspectReport.ImageDigest` is removed) so the wrong
  anchor cannot be reintroduced by a later edit. The `descriptor` object in the
  real JSON is simply ignored.
- `splitImageRef` parses both the spec and observed references into
  `(name, digest, ok)`, stripping an optional `:tag` from the final path
  segment (a `:` before the last `/` is a registry port and is preserved). This
  is the tag/digest normalization that makes a spec pinned by *tag+digest*
  (`…/alpine:3.22@sha256:PIN`, as the live fixture is) match the runtime's
  tag-dropped `…/alpine@sha256:PIN`.
- `sameImage` fails closed unless **both** references carry a digest and both
  name and digest match. This also removes a pre-existing asymmetry: the old
  agent path discarded the cut `ok` flag while the exporter path checked it; the
  new path checks it for both, so it is strictly stronger than the old agent
  check.

Trust still binds to the digest (the issue's non-goal); only the field the
digest is read from changed.

## Rejected alternatives

- **Keep comparing `descriptor.digest`.** It is not predictable from the spec
  and differs from the pin on multi-arch, so it can never match; retaining it
  is what caused the bug.
- **Retain `ImageDigest` as unused evidence.** Leaving a trusted-looking field
  invites a future re-added descriptor comparison. Removed structurally.
- **Digest-only comparison (drop the name check).** The digest is the anchor,
  but the tag-normalized name is cheap defense-in-depth and matches the issue's
  "reference comparison is tag/digest-normalized" ask, so it is kept.

## Verification

- `go test ./internal/ward/...`, `go vet ./...`, `golangci-lint run`: pass.
- Mutation check: reverting `sameImage` to the pre-fix "observed reference ==
  bare name" assumption fails the conforming-report, decode-plus-verify, and
  exporter-payload-mismatch tests — the faithful fixture/fake now encode the
  real shape and would catch the regression.
- Not run: the env-gated live tests `TestLiveHandoffLifecycle` (main) and
  `TestLiveConformanceSuite` (#77/#144) require an Apple container 1.1.0 host
  (`FREESIDE_WARD_LIVE_TEST=1`); the fix targets exactly their failure.

## Coordination with #144

All edited files are byte-identical on `main` and #144's branch
(`feat/ward-conformance-suite`), which only adds `suite*.go`. This fix lands on
`main`; #144 inherits it on its next rebase (no conflict) and its live
conformance suite passes then. Owner-confirmed: fix on main, #144 inherits.

## Refute-first pass

A fresh-context adversarial lens got the diff plus the stated intent (not this
reasoning) and was tasked only to disprove the fix. It could not refute the
security property.

**Confirmed / accepted-by-decision.**

- *Trust binding.* The anchor is the exact `expDigest == obsDigest` check
  (`sameImage`). A digest is a content hash, so an observed reference passes
  only by carrying the true pinned digest, i.e. the pinned content. Every
  hostile input walked (`no @`, `name@`, empty reference/name, multiple `@`)
  fails closed. Name normalization is genuinely defense-in-depth.
- *Fail-closed on drift.* Dropping the descriptor presence check is consistent
  (descriptor is read nowhere); `reference != ""` is still required and
  `sameImage` independently rejects a present-but-tagless reference.
- *Decode.* `rejectDuplicateJSONKeys` works on raw bytes independent of the
  struct shape, so removing the `Descriptor` field does not weaken duplicate-key
  or identity rejection.
- *Strictly stronger.* The new path checks the cut `ok` on both sides; the old
  agent path discarded it. Since spec images are validated digest-pinned, the
  added rigor only tightens the observed side. No case is weaker.

**Rejected by verification (do not re-raise).** `splitImageRef` on a *pathless*
`host:port@sha256:PIN` (no `/`) strips the port as if it were a tag. This is not
a valid OCI image reference (no repository path), the parse is symmetric on both
sides, and the digest still gates, so it can never admit a wrong image. Fully
qualified references (both our specs and the runtime's observed reference always
carry a repository path) are handled correctly. Documented at `splitImageRef`;
no code guard added, per no-defensive-complexity-for-impossible-input.

**Accepted limitation (Verification gap, not a defect).** The "1.1.0 reports the
full pinned reference and a different descriptor digest" premise is encoded in
the fixture/fake, not observed against the runtime in CI. If the premise were
wrong the gate would reject *legitimate* images (fail-closed availability
failure, not a security bypass). The env-gated live tests are exactly what
closes this; recorded as a `Not run:` gap in the PR pending a reference-host run.

Revisit when: Apple container changes how `container inspect` reports
`image.reference` (e.g. stops embedding the pinned digest, or starts emitting a
tag alongside it in a way the final-segment tag-strip mishandles), or a second
runtime backend reports image identity differently and the seam must generalize.
