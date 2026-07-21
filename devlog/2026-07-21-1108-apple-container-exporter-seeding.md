# Seed the Exporter Through a Temporary Loopback Registry (#196)

Issue #196 closes the last live conformance gap from the exporter/evidence
unit recorded in
[`2026-07-19-1753-exporter-evidence-conformance.md`](2026-07-19-1753-exporter-evidence-conformance.md).
It also verifies the commit-plan member added by #212 through the same real
helper-in-image seam. This is safety-conformance and returned-object
trust-boundary work, so the refute-first rule applies.

## Decision

Chose a temporary, pinned `registry:2` on an unprivileged loopback port to seed
the exact exporter digest into Apple `container` 1.1.0's local store. The build
script captures the locally built digest before registry interaction, pushes
the tag with explicit `--scheme http`, then pulls
`127.0.0.1:<port>/freeside-exporter@sha256:...` with the same explicit scheme.
That exact pull both rejects substituted registry content and records the
name-at-digest reference that Apple `container` can later create after the
temporary registry has been removed.

Ward remains unchanged: it accepts only digest references and uses its ordinary
`container create` path, with no HTTP scheme override. Passing HTTP through
Ward was rejected because Apple applies the create-time scheme to every image
lookup, including the runtime init image; registry transport belongs only in
the setup boundary. The exporter still runs with zero networks, and the
temporary registry never exists during exporter execution.

The registry helper is pulled and run by its reviewed `registry:2`
manifest-index digest; it never depends on the mutable tag resolving to the
same content later. Rejected alternatives: a mutable exporter tag (weakens the
trust boundary), a hosted registry (adds an external dependency and
distribution surface), Docker or Skopeo (adds a second runtime solely for
transport), port 80 (requires privilege), and teaching Ward to tolerate a
missing digest reference (fails open).

The earlier conclusion that Apple `container` 1.1.0 cannot push to a local
plain-HTTP registry was too broad. Its automatic scheme selection fails for a
loopback host carrying an explicit port, while explicit `--scheme http` works.
The remaining local-name lookup defect is tracked upstream in
[apple/container#1962](https://github.com/apple/container/issues/1962), with a
proposed fix in
[apple/container#1963](https://github.com/apple/container/pull/1963).

## Refute-First Verification

**Confirmed and fixed:** Apple build metadata can change between otherwise
identical builds, contradicting the old script comment that every run
reproduces one digest. The safety property does not require reproducibility:
the script now documents and verifies the exact digest produced by each
invocation.

**Rejected by verification (attacked, not refuted):**

- Registry substitution: a disposable registry carrying only a different
  exporter build returned `404 Not Found` when asked for the expected digest;
  it could not satisfy the exact pull.
- Mutable-reference admission: Ward's targeted rejection test still rejects
  a tag, while its live tests accepted only the seeded digest reference.
- Cleanup ownership: an occupied-port run failed during bootstrap, removed its
  uniquely named temporary container, and left the pre-existing listener
  reachable. A fault-injected `container delete --force` failure made the
  otherwise successful script return nonzero with zero stdout bytes, rather
  than handing a caller the seeded reference while the registry remained
  online; the intentionally retained registry was then recovered explicitly.
- Registry dependence: both live exporter-image tests passed against
  `127.0.0.1:5005/freeside-exporter@sha256:c5a97603039a2f2138de321c8510e77968834100bd91af99d3a319ad6193afbf`
  after the temporary registry was removed. The lifecycle test carried the
  repo, evidence, and commit-plan members through the real helper, Ward, and
  importer together.
- Restart persistence: after a full `container system stop` / `start`, ordinary
  `container create` by that exact reference succeeded with the registry still
  offline. No temporary registry container or named volume remained.

## Revisit When

Apple ships a release containing the local digest-reference lookup fix: remove
the temporary registry path after proving a locally built exact reference works
through the same live suite.
