# Specification Vocabulary (#986)

Contract change: the stage formerly named for its process is named for its
product, like the other stages: `specification` stage, `specifier` role,
`spec_approval` gate, specification artifact. The rename covers the domain
enums, the `elaborate` package (now `specify`), the engine's outbox and
inbox kinds, the identifier prefixes, the `production_attempts` column, the
API `StageName` enum and its app label, the `freesided` flags and result
lanes, the real-run scripts, the prompt file, and the plan. Owner decision,
2026-08-28, recorded on #986.

## Decisions

- **Rename the vocabulary, not only the UI label.** Rejected keeping
  `elaboration` in code and relabeling the clients because the cost only
  grows (#990 adds more `elaboration_*` identifiers) and the surface had
  barely reached the API and app: one `StageName` enum value, one label case,
  one test. The rename is one mechanical commit, listed in
  `.git-blame-ignore-revs`, with the compatibility, check, prose, and note
  commits separate so every commit is green on its own.
- **Fix the input-side collision in the same unit.** The submitted work
  item was also called a spec: `freesided submit --spec`, source kind
  `spec_artifact`, source field `spec_artifact_id`. After the rename the input
  is the work item, which is what the plan already calls it: `--work-item`,
  `work_item_artifact`, `work_item_artifact_id`. The output-side fields that
  name the specification itself (`spec_artifact_id` on terminals and sync,
  `prior_spec_artifact_id`, `approved_spec_digest`) and the `spec_approval`
  gate keep their names because the specification is still the product.
- **`--spec` is removed, not aliased.** Every consumer of the flag lives in
  this repository (the real-run scripts and the cmd tests), so a hidden
  one-release alias would only keep the old word alive in the surface the
  vocabulary check exists to close.
- **Never rewrite stored bytes; canonicalize on read.** A database written
  by the pre-rename daemon must open, migrate, and reconstruct through the
  renamed code. Rejected rewriting rows in a migration (SQL JSON functions or
  a Go data migration) because outbox payloads carry a stored digest that
  backup closure re-checks, resolved policies are digest-addressed over their
  key names, the engine's decoders re-encode payloads and compare bytes, and
  idempotency keys embed run identifiers that appear in attention subjects
  and backup manifests. Instead:
  - Identifiers keep their family. A run minted before the rename keeps
    `run-elaboration-<hex>`, `elaborate-<run>`, `inv-elaborate-<run>-<n>`,
    and its `claim-elaboration-implementation-` row forever; the family is a
    pure function of the run ID prefix (`domain.LegacySpecificationRun`), and
    every site that mints or parses a derived identifier goes through the
    domain helpers. The implementation run ID carries no family marker, so
    the specification run selects the family for the claim key, and a
    lookup that knows only the implementation run tries both derivations.
  - The store translates at the row boundary every consumer shares: queue
    kinds map to the current names as rows are scanned, SQL kind filters
    bind both spellings, and the one legacy JSON key plus the three versioned
    payload names are substituted in place after the stored payload digest is
    checked against the stored bytes. Each substitution swaps one quoted
    token for the current encoder's, so a canonical pre-rename payload stays
    byte-equal to what the current encoder produces from its decoded value.
  - Enum values canonicalize on decode (`StageName.UnmarshalJSON`,
    `Run.CanonicalizeStoredRow`, the source union's kind and legacy field
    name), following the existing `CanonicalizeStoredRow` precedent.
  - The resolved policy keeps its stored keys; `specify.ParsePolicy` reads
    the `elaboration.` prefix as `specification.` and the lineup validator
    accepts the legacy role spelling, so the policy digest still verifies.
  - Migration 0064 renames only the `production_attempts` column; no row
    content changes.
  - A replayed `freesided submit` on a pre-rename database resolves the
    specification run against the store (`engine.ResolveSpecificationRunID`)
    and converges on the legacy run instead of minting a second one under
    the current family for the same implementation identity.
- **The fixture is real pre-rename bytes.** The store test loads a
  `sqlite3 .dump` of a database the pre-rename binary wrote through submit,
  invocation, terminal, approval, and the reserved implementation run; the
  cmd test replays the same submission against it. A fixture built from the
  renamed encoder would prove nothing about the old bytes.
- **The vocabulary check covers code, not prose.** `scripts/check-vocabulary.sh`
  greps daemon non-test Go (except one `legacy_vocabulary.go` per package),
  migrations after 0064, and everything under `api/`, `app/`, `prompts/`, and
  `scripts/`. Prose keeps the ordinary English sense ("request further
  elaboration", plan §5.14), so `docs/` is reviewed by hand; `devlog/` and
  `docs/history/` are frozen records.

## Verification Findings

Refute-first pass on the persisted-compatibility change (a reconstruction
trust boundary), by a fresh-context reviewer prompted to break it:

- **Confirmed and fixed: quarantine record and release disagreed on
  family.** The release side looked under the run's family prefix while the
  record side always wrote the current one, so a legacy run quarantined after
  the upgrade could never be released. Both sides now follow the run's
  family, and a notice the pre-rename daemon wrote (legacy prefix, legacy
  reason text) is recognized on release.
- **Confirmed and fixed: the discussion intent validator pinned the current
  prefix.** A pre-rename discussion marker keyed `elaboration-discussion-`
  failed `DecodeSpecificationDiscussionInvocationIntent` even though the
  engine parsed its key. The discussion identity helpers moved to `domain`
  and accept both families; the fixture now carries a dispatched legacy
  discussion marker and its terminal.
- **Confirmed and fixed: the inbox duplicate path skipped canonicalization.**
  `RecordInbox` on an already-recorded key returned the raw row, so a
  concurrent double completion of a legacy terminal would have reported an
  immutable-transition conflict instead of replaying. The scanned row is
  canonicalized like every other read.
- **Confirmed and fixed:** the store's migration-head test pinned schema 63.
- **Disproved by reading, with one accepted residual:** byte substitution
  cannot match a token embedded in user text (a quote inside a JSON string
  is escaped) or a string value spelled like the renamed key (that pattern
  needs the trailing colon). It does match a JSON string whose entire value
  is one of the three versioned wire names, so a message body or policy
  value that repeats a retired wire name verbatim reads back under the
  current name (a policy row would fail its digest re-check). Reaching it
  takes an operator storing the wire name itself as a whole value through
  the local API; accepted over per-shape structural substitution, which the
  shared row boundary cannot do without knowing every persisted shape
  (raised in review of #1077). Of the renamed JSON tags only
  `elaboration_run_id` persists; the canonical re-encode checks keep field
  order; `PromoteOutbox` callers carry no specification key; every outbox
  list filter binds both kinds; `RENAME COLUMN` on the STRICT table applies
  under the pinned modernc build; nothing else references the old column.
- **Allowed by decision:** `preflight` still derives the current-family run
  only, because that value feeds the path-boundary check and nothing the
  store compares. The source union decodes as leniently as the store's row
  decoder instead of rejecting unknown fields, matching the only path that
  decodes it from storage.
- **Not covered:** neither compatibility test drives `engine.Reconcile` or
  the observe path over the fixture; the store test reconstructs every row
  kind through the store and engine decoders, and the cmd test replays the
  submission. A pre-existing gap surfaced alongside: the quarantine notice
  matcher never recognizes the discussion prefix on release, on `main`
  before this unit; filed as a follow-up issue.

## Revisit When

- The legacy identifier family can be retired: when no operator database
  predates the rename, the `legacy_vocabulary.go` files, the SQL kind
  aliases, and the fixture can go, and `SpecificationRunIDMatchesImplementation`
  collapses to the current derivation.
- A persisted shape gains a stage name or specification identifier outside
  the JSON bodies and queue payloads the store canonicalizes (a new column or
  a non-store reader), which would need its own read-side translation.
