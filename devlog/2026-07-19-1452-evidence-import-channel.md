# Evidence handoff channel: importer side (#167)

Gauntlet work unit, 2026-07-19. Implements the importer/consumer side of the
second §5.6 workspace-exit channel (the evidence channel), the P2 finding #167
from the Wave 1 adversarial audit (#83). Consumes the already-merged #173
contract (schema, strict decode boundary, `AgentClaim` shape) and lands after
the #166 control-plane class. Trust-boundary + safety-policy work, so this note
is mandatory; status lives on #167 and the PR, never here.

## Confirmed owner decisions (rejected alternatives)

- **Scope: importer side only.** This unit parses `evidence.json`, verifies the
  `evidence/` blobs, routes valid entries into labeled `domain.AgentClaim`s, and
  adds the permanent malicious-evidence fixtures. **Rejected: also making the
  real `export.Export()` emit the evidence channel now.** Emission requires
  defining how an agent *declares* evidence inside the read-only workspace (a
  label/media_type/provenance/source-path descriptor) — a brand-new
  untrusted-input surface that deserves its own review and should be co-designed
  with #170's ward workspace layout, since #170 is the consumer that needs the
  real helper image to emit. Filed that emission as a follow-up deferral issue
  (Follow-up: #190). Clean fixtures are built in-tree via
  `EvidenceManifest.Encode()` + blob writes, so adversarial coverage does not
  depend on a real emitter.

- **Bad evidence fails closed as a typed error, not a publish-blocking finding.**
  Any invalid evidence (framing, digest/size/layout, over-cap, magic/type
  mismatch, unlisted media type, forged provenance) aborts the whole import with
  a typed error and no `Result` — the same posture as the repo channel's
  integrity failures. Valid evidence becomes `Result.Claims`. **Rejected:
  surfacing evidence policy violations as `domain.CandidateFinding`s.** None of
  the four existing `CandidateFindingClass` values fit an evidence-channel
  finding (`import_integrity` and `repo_change_policy` are both defined as the
  repo-change channel), so that path would need a new `domain.FindingClassEvidence`
  — a spine `kind:contract` unit in `daemon/internal/domain`, blocking #167 (the
  way #177 blocked #166). Failing closed keeps #167 entirely within gauntlet-lane
  paths (`export` + `importer`) with no domain change and no new contract
  dependency, and is the loudest fail-closed posture; a hostile agent can only
  sink its own already-suspect candidate. Evidence has no waivable class: it is
  agent context that either validates into a claim or fails the import closed.

- **Content address is the claim's artifact id.** An evidence entry carries a
  label + digest but no id, so each claim's `Artifact` is set to the sha256
  content address (`ArtifactID(digest)`). Deterministic, and consistent with the
  domain rule that an artifact id addresses one digest (distinct labels may share
  one id+digest). Revisit if downstream item-assembly assigns opaque ids.

- **Image-only media allow-set with an explicit magic table** (png/jpeg/gif/webp),
  not `http.DetectContentType`. §5.15 rule 3 has the daemon validate magic/type/
  size and treat agent images as opaque blobs; `DetectContentType` is deliberately
  lenient (octet-stream/text fallbacks, HTML sniffing), the wrong bias at a
  hostile boundary. **SVG (scriptable XML) and text/plain (no reliable magic) are
  excluded by design;** the set widens only when a concrete need and its magic
  check land together. Polyglot images (valid magic + hidden payload) are
  in-contract: §5.15 rule 3 treats images as opaque and never decodes them.

- **Head-binding *value* verification is the publisher's job, not the importer's.**
  The importer validates head-binding *shape* only (already enforced by
  `DecodeEvidenceManifest`), and passes `source_head_sha` through verbatim. §5.15:
  the publisher verifies head binding before publication. This avoids embedding a
  base==source-head assumption in the importer.

## Behavior preservation on the repo channel

The two-channel refactor of `blobs.go` (`verifyBlobs`, `auditHandoffLayout`, the
extracted `auditBlobStore`/`accumulateNeeded`/`snapshotChannel`) is
behavior-preserving for the repo channel: the full pre-existing hostile-importer
suite (`blobs_test.go`, `import_test.go`, `guarantees_test.go`) passes unchanged,
and the 7 import-result goldens changed only by gaining `"claims": []` from the
new `Result` field. `auditBlobStore` is the old single-store scan parametrized by
`storeName`.

## Refute-first verification

Trust-boundary requirement: an independent lens was tasked to disprove the
fail-closed guarantees (smuggle a trust field, launder invalid UTF-8,
cross-channel-substitute a blob, forge producer class, oversize past caps,
polyglot/short-file magic, layout orphan, TOCTOU).

- Confirmed defects: **none.** The lens ran nine additional crafted
  hostile-input experiments (cross-channel substitution both directions, trust-
  field smuggling, UTF-8 laundering, layout orphan, symlink/FIFO at evidence.json,
  polyglot/short-file magic, over-cap streaming, duplicate-digest dedup) on top of
  the in-tree corpus; every one failed closed with no `Result`. Repo-channel
  behavior preservation was confirmed decision-for-decision against
  `HEAD:blobs.go`.
- Rejected by verification (not defects; recorded so they are not re-raised):
  (1) `source_head_sha` carries no syntactic shape check (any non-empty string) —
  this is the already-merged domain/export contract's laxity, not introduced here;
  the head is agent-asserted *context* that becomes only a labeled `AgentClaim`
  (no commit, no upload, no `publish_eligible`), and §5.15 defers head-binding
  verification to the publisher. The mode-consistency forgeries (head_bound-without-
  head, head_independent-with-head) *are* caught. (2) Two independent ~1 GiB read
  budgets (repo + evidence) — by design; the caps are tracked separately to keep
  the channels independent, and each is a hard-fail integrity cap.
- Accepted by decision: polyglot images pass magic validation by design (§5.15
  rule 3 treats images as opaque and never decodes them); evidence problems sink
  the whole import (fail-closed coupling of the one untrusted handoff), an accepted
  DoS-not-breach tradeoff.

## Revisit when

- #170 builds the real exporter image: the deferred emission follow-up and the
  agent-workspace evidence-source descriptor land there / with it, and #170's
  live conformance must feed the real helper output through this importer.
- The malicious-evidence corpus or a real consumer surfaces a media-type the
  image-only allow-set should include (a v-next evidence wire concern, per the
  #173 note's "Revisit when #167 lands").
