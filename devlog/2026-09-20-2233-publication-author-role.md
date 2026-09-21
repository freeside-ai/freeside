# Publication-Author Judgment Role

Work unit #1418. Builds the publication-author role (plan §5.13, §5.15) as two
advisory judgment sites, `publication_author_explain` and
`publication_author_propose`, under one refinable role prompt. This note records
the lasting implementation decisions and the refute-first findings for the
returned-object trust boundary (the input builder in
`daemon/internal/inference/publication_author.go`). The ten planning decisions
themselves live in the #1418 issue body, not here.

## Input Builder Re-Gates Evidence Instead of Trusting the Stored Bit

Chose to re-run `domain.ValidatePublishEligibility` against the caller-supplied
approved-recipe set for every candidate evidence artifact, rather than trust the
`publish_eligible` bit on the row. This is the same rule the store's
`gatePublicationAuthoring` runs on put and every read, so any artifact the author
can be shown, and therefore cite, is one the store will later accept. An artifact
is kept only when `ValidatePublishEligibility` passes, `PublishEligible` is true,
and its class is no more restrictive than the target's. Rejected: passing the
evidence through on its stored bit, which would let a forged or stale
`publish_eligible: true` reach the model and produce a citation the store then
rejects, turning a good run into a late failure.

The model answers with artifact ids only; the digest on each returned
`PublicationEvidenceReference` is filled from the supplied artifact, never from
model output, and a cited id the call did not supply forces a fallback. So no
model-controlled field crosses the boundary into a stored reference.

## Role Prompt Reaches the Driver by Flag + Config Digest, Not `go:embed`

Chose a daemon flag (`--judgment-publication-author-prompt`) whose file the
daemon reads at startup, whose content digest folds into the judgment
configuration digest preflight records, and whose bytes pass to the Claude
driver through a `New` option. Rejected `go:embed`: `prompts/` is outside the
`daemon/` Go module, and a copy inside it would stop being the refinable
control-plane file. The bytes stay off the comparable `judgmentConfig`; only the
path rides on it, so a different prompt still yields a different preflight digest
through the receipt's separate prompt-digest field.

## Per-Input Sensitivity Classes Cover Stored Content Only

The call result returns a `domain.SensitivityClass` for the diff, both control
files, and the source issue (the inputs #1419 stores), derived from repository
visibility: the diff and control files take the target repository's tier, the
source issue its own. The operational identifier fields (repository names,
outcomes) carry no stored-content class. The outbound allowlist tier stays on
`inference.Sensitivity` (repository content vs. operational), which `Client.Call`
matches exactly per field; the derived domain classes travel beside the fields,
not on the field policy.

## Refute-First Findings (Returned-Object Trust Boundary)

- Forged/stale `publish_eligible: true`: disproved as a leak. Re-gating against
  the current approved set drops it; covered by `TestAuthorForgedBitExcluded`.
- Model-injected evidence digest: disproved. The model supplies ids only; the
  digest comes from the supplied artifact. An unsupplied id falls back
  (`TestAuthorPublicationCitesUnsuppliedID`).
- Candidate-head control file: disproved. `ControlFile.checked` recomputes the
  digest and requires a trusted-base commit; the input type has no candidate-head
  field. Covered by `TestAuthorControlFileRejections`.
- Cross-visibility leak of a private source issue into a public target:
  disproved. A source class more restrictive than the target blanks the issue
  title and body, keeping only the reference
  (`TestAuthorInputVisibilityClasses`).
- Site output reaching a `Closes`/`Refs` line, a CI skip, or a trailer:
  disproved. The explain validator screens every text field with
  `importer.ScreenMessage` (github/1 ruleset) and `ContainsSecret`, re-screening
  after each HTML unescape; the propose answer is a single boolean. Covered by
  `TestAuthorPublicationExplainRejections`.

Revisit when: #1425 moves these sites to the lineup (ending the pinned-binding
interim), #1428 moves the site prompts out of the driver, or #1419 binds the
target sensitivity class to real repository state rather than the caller-supplied
visibility.
