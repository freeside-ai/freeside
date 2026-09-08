# Publisher-Owned Verification Results

Work unit: #1215.

Freeside owns the PR's Verification section. Operator publication prose remains
immutable, including trailing newlines, and cannot contain the section's heading
or markers. Appending a differently named results section would leave prospective
verification text looking authoritative. Replacing a placeholder or rewriting
operator prose would break the publication-input contract.

Render the verifier's stored report bytes, not a second serialization of an
engine result. The existing authorization binds the report artifact through its
evidence snapshot; publication checks the bytes' digest, canonical report shape,
head, base, recipe, and outcome. The engine loads exactly one report for the
verification invocation. Missing, oversized, or mismatched evidence cannot
publish. Both production and attended fake publication use this path without a
new domain, store, or API schema, or publisher access to the artifact store.

Agent evidence renders only as labeled artifact references, after the import
result is checked against the authorization's import digest. Claim text never
enters this section. A fixed statement distinguishes the commands Freeside ran
from all other checks mentioned in operator or agent prose. Parsing arbitrary
prose for check names would introduce another unreliable source of facts.
Publisher-owned review evidence remains a separate executed account; the
not-run statement applies only to operator and agent claims.

The section remains outside publication identity. Existing PRs converge onto
the new rendering, and retries reconstruct it from the same immutable inputs.
Reserve 8 KiB for the section and five separators in the overall body budget;
operator prose gets 23,432 bytes. Render at most 24 commands, bound each rendered
argument vector to 512 bytes, identify truncation by digest, and count omitted
steps. Refuse an oversized section rather than silently dropping claims. JSON
argument arrays preserve argument boundaries that a space-joined command loses.

## Refutation Findings

- Confirmed and fixed: invisible comments and inline markup could disguise a
  Verification heading. Both validators share conservative heading normalization
  in `publicationrecord`: comments, entities, inline formatting and link targets
  cannot grant authority. It recognizes ATX, setext, HTML, and nested headings
  without changing prose bytes. Ambiguous formatting is treated conservatively;
  distinct titles such as "Verification details" stay allowed. A full Markdown
  renderer would add a dependency merely to reserve one title.
  The wider refutation pass confirmed multiline HTML headings, quoted delimiters
  in tags and link titles, and entity decoding order as members of this class.
  Regression cases pin source-syntax parsing before visible-text decoding.
  A later pass found an entity-encoded zero-width suffix. Final title comparison
  ignores Unicode format/default-ignorable characters and variation selectors,
  so literal or encoded invisible text cannot impersonate the reserved title.
  Visible spelling, accents, and distinct titles are not transliterated.
- Confirmed and fixed: the plan's ATX-only heading matcher allowed CRLF and
  underline-style Markdown headings. Both input validators now reject these
  ordinary forms as well as case variants and closing hashes.
- Confirmed and fixed: the plan's proposed "networkless workspace" sentence
  overstated the attended fake path. Its `verify.ProcRoom` cannot deny network
  access. The shared section now reports only executed commands and bound facts;
  the report contains no isolation fact to justify that assertion.
- Disproved by targeted checks: substituted report bytes, foreign evidence
  snapshots, and edited import results can reach publication. The authorization
  gate rejects them before forge writes. Count, invocation, size, and digest
  mismatches also fail at report loading.
- Disproved by recovery fixtures: loss of the report after verification permits
  publication, or a retry invents replacement results. Publication records a
  retryable environment hold; restoring the blob and restarting recovers the
  original report. Existing checkpoint reconstruction treats an absent durable
  blob as a contradiction, preserving its prior failure policy.

## Upgrade Consequence

A persisted publication body with a Verification heading, or beyond the smaller
prose budget, now fails validation when decoded. This is intentional fail-closed
behavior. Check queued operator runs before deployment; this work does not deploy
the daemon or inspect the operator's live state. Known terminal Wave 7 attempts
motivated the issue, but they are not evidence that every stored run is terminal.

Revisit when #691 introduces a daemon-owned verification execution record. At
that point, prefer rendering its authenticated record over looking up report
artifacts, without changing publication identity.
