# Agent Summary Trust Boundary

## Decision

Chose a fixed, optional `.freeside-evidence/summary.md` artifact and a
root-authored evidence descriptor over transcript parsing, an agent-authored
descriptor, or a new API field. The fixed path gives every launcher and prompt
one small contract. Rewriting the descriptor after the process exits keeps the
agent from choosing the reserved label, media type, provenance, or invocation
binding. Reusing `AgentClaim` keeps the prose explicitly unverified and avoids
creating a second trust model in the API.

The importer inlines non-empty UTF-8 Markdown only when it fits the existing
claim-text byte limit and is not high sensitivity. Oversized and
high-sensitivity summaries remain artifact-only claims. This preserves the
artifact for inspection without placing unsafe prose in the client cache.
UTF-8 validation streams the verified snapshot, so an artifact-only summary
does not require a second allocation up to the evidence-blob cap.

Before inlining a Markdown claim, the importer applies the same bounded,
high-signal credential scan used for repository content. A match becomes a
secret finding without copying the matched bytes, withholds inline text, and
causes the production stage to reject the export before evidence persistence.
The same scanner rejects credential-shaped structured specification summaries
and bodies before the engine creates their artifact or attention item. Content
above the scanner's configured cap already cannot be inlined and retains the
artifact-only treatment.

Specification summaries use their own deterministic artifact identity instead
of reusing the specification artifact ID. Reuse was the implementation plan's
assumption, but the store correctly rejects one artifact ID that names two
different digests. A separate identity preserves the existing content-address
invariant while binding approval to both artifacts.

Production-attempt reconstruction accepts the new two-claim approval only when
the summary has the deterministic identity, Markdown body, specification
provenance, and the digest pinned by the immutable elaboration terminal. Its
digest set uses the same sorted, deduplicated binding rule as item construction.
The terminal digest is derived from the decoded stage output before the item is
stored, so a coherent item-and-command rewrite cannot substitute different
summary prose during reconstruction. Reconstruction still accepts the
historical one-claim approval shape and its terminal without a summary digest.
The summary is advisory, so requiring old durable approvals to contain it
would break valid lineage without strengthening specification authority.

## Refute-First Findings

- **Confirmed and mitigated:** The agent can replace the fixed summary path
  with a symlink. The launcher admits only a regular, non-symlink file before
  adding the root-owned descriptor entry.
- **Disproved by checks:** Empty or invalid UTF-8 Markdown cannot cross the
  exporter or importer boundary. Focused negative cases reject both shapes.
- **Disproved by checks:** A reserved summary with duplicate authority, the
  wrong producer invocation, or an invalid inline body cannot become rendered
  summary authority. Normalization removes inline text while preserving the
  ordinary artifact claim.
- **Disproved by checks:** A forged summary cannot become part of a new
  specification approval's reconstructed authority. Reconstruction rechecks
  its identity, inline body, provenance, immutable terminal digest, digest set,
  and the operator command's complete binding set.
- **Confirmed and mitigated:** Agent output can contain a credential available
  in the execution environment. Summary and specification text is scanned
  before inline or durable persistence; matches produce only rule-and-line
  metadata and terminalize the execution without storing the credential.
- **Disproved by checks:** High-sensitivity or oversized content cannot enter
  the inline client carrier. Both cases remain attachment-only claims, and
  oversized UTF-8 validation does not allocate the complete file.
- **Allowed by decision:** An absent summary is not a launcher failure. The
  summary is advisory context, so the primary transcript and outcome remain
  authoritative when the optional file is absent.

## Revisit When

Revisit when another launcher adopts the contract, an independent briefer
replaces the producing agent, or the API gains a first-class compressed
artifact reference that can preserve the same unverified and provenance-bound
semantics.
