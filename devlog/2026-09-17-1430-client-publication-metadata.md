# Candidate-Bound Public PR Metadata

Work unit: #1382. Plan revision: 63.

New client submissions freeze a versioned publication recipe and commit
attribution before allocating their implementation identity. The recipe may
also bind a descriptive source link, but only when the entire trimmed source
is a canonical GitHub issue URL. Task names keep their existing precedence and
approval rules; they never choose or rename PR metadata. Client work remains
`bound_pr_merged`, with no bound issue or generated closing directive.

Chose a separate public-intended artifact over the raw request, generic task
placeholder, or sensitive implementation summary. A source URL does not
explain an implemented change. Reusing the summary would change its
confidentiality contract. Editing publication inputs after approval would
invalidate identity and approval bindings. The new launcher-declared
`freeside.publication` channel is ordinary agent evidence, never authority to
publish or upload an artifact.

Recipe v1 requires one inline Markdown claim from the authenticated current
producer and candidate import, with agent provenance, normal sensitivity,
head-independent transport and no source-head value. The complete artifact is
bounded at 8 KiB and screened before any text is rendered. The title is its
opening heading, bounded at 256 bytes. The description renders as escaped
text under Agent-reported implementation (claim), with producer and digest.
The publisher still owns Verification, review history, advisories, scope
decisions and the final identity marker. Claimed results never become verified
facts. A public label or sensitivity value grants no publication authority.

Missing or invalid public output holds publication. Its imported bytes cannot
be replaced, and the same-task retry/continuation paths do not authorize a new
producer for a metadata failure. Recovery therefore uses the existing
paired-device `stop_task` command on `POST /commands`, with task/project IDs,
the current task snapshot version and sync epoch. After cancellation is
confirmed, admit an updated producer prompt package when needed and submit a
fresh task with a new command ID. Existing PRs stay intact and require explicit
inspection/disposition before replacement work. The card remains inspect-only:
its Stop action would resolve the card without cancelling implementation work.
Task-level UI controls remain #1369; no CLI or UI affordance is claimed here.

Chose this supported terminal recovery over adding same-task retry authority,
which would require shared domain, Signet and store contracts outside this
unit. No silent fallback, inference call, original-artifact rewrite or
private-summary publication is permitted. The original artifact stays private.

Same-candidate retries reconstruct the same title and body from the versioned
recipe and immutable authenticated import. A new authorized remediation or
feedback producer supplies its own account of the whole current change.
Submission, approval and publication identities remain unchanged. Recipe v1's
rendering is fixed; future rendering changes require another recipe version.

This extends, rather than reverses, the literal-publication decision in
[Publisher-Owned Verification Results](2026-09-07-1928-published-verification-results.md).
Explicit CLI and label-intake title/body bytes remain literal. Omitted optional
recipe fields preserve historical canonical encodings and their digests;
backup reconstruction still uses its separate retained-prose validation.

The unit stays together because transport, screening and recovery must agree
before client activation. No public schema, migration or live configuration
change is required.

## Refutation Findings

- Confirmed in automated review: feedback invocation IDs include the raw client
  command ID, so provenance authentication did not prevent a Markdown code-span
  escape. Screen the complete producer through the public-text gate and render
  it as escaped text in a standalone pre/code block. Inline HTML code still
  permits Markdown parsing on GitHub; the standalone block keeps link, image,
  backtick and HTML input inert. Other interpolations are the validated digest,
  screened artifact text and constrained source URL.
- Disproved by independent refutation, feedback-ID fixtures and GitHub's GFM
  renderer: backticks, blank lines, links, images or closing HTML can escape the
  new producer block. Encoded secrets, controls and automation directives fail
  screening; accepted syntax remains literal text without links or images.
- Confirmed in automated review: the original error instructed an operator to
  produce a new candidate while leaving an inspect-only hold on an immutable
  import. Both publication paths now give the supported task cancellation and
  fresh-submission procedure. Recovery fixtures cover a stable hold after
  restart, real Signet Stop acceptance, confirmed quiescence of the fixture's
  owned fake runtime, WIP release, and a new client identity that publishes.
- Confirmed in self-review and fixed: a repository segment of `.` or `..`
  matched the initial source-URL pattern but resolves to a different path.
  Reject dot segments rather than publishing them as canonical issue links.
- Disproved by targeted fixtures: a public label can promote private, foreign,
  head-bound, duplicate or artifact-only prose. Selection rejects each shape,
  and digest substitution in retained artifact bytes cannot reach a forge write.
- Disproved by full-artifact screening and rendering fixtures: secrets,
  controls, closing directives, encoded reserved headings or HTML can escape
  the claim section. Screening rejects refused input without echoing it;
  accepted formatting is escaped and the original artifact stays unchanged.
- Disproved by production recovery fixtures: lost responses or restart create
  another PR, drift repair regenerates metadata, or a successor reuses its
  predecessor's account. The same candidate converges on identical bytes;
  remediation and feedback publish their own current producer's account on the
  same owned PR. Cancellation prevents both initial publication and repair.
- Disproved by client-through-publication and backup fixtures: blank,
  operator-provided or refined names select PR prose, configuration replay
  changes the recorded recipe, or optional fields rewrite literal metadata.
  Client approval carries the original recipe into its reserved implementation
  run; canonical historical bytes and all recipe backup payloads stay intact.
- The independent review found no reachable production defect. It identified
  two verification gaps, now covered: one fixture spans client submission,
  specification approval and publication; the feedback fixture explicitly
  checks replacement title, description and producer provenance.

Revisit when a later recipe needs rich public formatting or another artifact
transport. Preserve the labeled-claim boundary and immutable-input replay.
