# Agent Question Producers (#990)

Contract change: the specifier and the implementer gain a typed way to stop
and ask the human, both producing the existing `agent_question` item that
#919's answer transactions consume. One decision shape lives in the domain
package; the specifier returns it as a fourth output variant, the implementer
reports it through a launcher-declared evidence file, and both render as one
card with typed facts. The design is an owner decision made in chat
(2026-08-28), refined at planning (2026-09-02); this note records what the
implementation settled beyond the issue contract.

## Decisions

- **One `domain.Decision` shape for both producers and the card.** A decision
  is a question, why it blocks, two to six labeled options, and a
  recommendation that must equal one option's label exactly; a result carries
  one to eight, each text field at most 4 KiB. The same validator runs at the
  specifier decoder, the blocked-outcome decoder, and the item facts, so the
  prompt limits and the accepted limits cannot drift apart. Option labels are
  unique, which keeps the recommendation unambiguous.
- **`AgentQuestionFacts` are required on every `agent_question` item, with
  exactly one `Question` claim from the asking invocation.** Rejected leaving
  the facts optional the way the other card facts are: no producer existed
  before this unit, so there are no legacy items to tolerate, and the answer
  transactions locate the source stage through the claims' provenance; a
  producer that could create an item without that claim would create one the
  answer cannot route. The requirement makes that state unrepresentable.
- **The blocked terminal is outcome-backed, not export-backed.** The plan
  assumed the stage driver would record an `ExecutionExport` for a blocked
  result; `ExecutionExport` requires a head SHA because it is the record
  publication joins on, and a blocked result has no candidate by definition.
  Chose a new non-export outcome status, `blocked`, over relaxing the export
  record: it keeps the two authorities disjoint (completed work is
  export-backed, everything without a candidate is outcome-backed), every
  publication path stays closed because no export exists, and the run
  observation and signet authentication already key on the outcome record
  for non-completed terminals. The decisions themselves travel as evidence
  blobs and agent claims keyed by invocation, which the engine re-reads; the
  result's artifact list is informational.
- **The driver records the blocked outcome in the ordinary terminal commit,
  not inside `finish`.** An early attempt recorded it before the released
  directory was removed, for the export path's durable-first ordering; but
  every intent load then restores the outcome into a minimal result, so the
  committed result lost its evidence digests and usage. The ordinary commit
  is where failed and canceled results record theirs, the persisted evidence
  lands before either, and the remaining crash window (dir removed, no
  durable terminal, intent not yet committed) converges to lost, which is
  rerun-safe. Recovery adopts a driver-recorded blocked outcome like the
  other outcomes.
- **A driver-recorded blocked outcome does not make the engine skip the
  attempt.** `acceptProductionAttempt` returns early when an outcome record
  exists; that ordering is exactly what the real driver produces before the
  engine collects. The blocked outcome is exempted so the engine still
  collects the terminal and raises the question, then converges on the
  driver's record. The same early return for failed and canceled outcomes is
  left as found and filed as #1084.
- **The engine reads persisted evidence through the blob store the
  configured workflows already share.** Rejected a new composition option in
  `cmd/freesided` (outside the unit's declared scope) in favor of the engine
  taking the store the production-publication or specification workflow was
  configured with.
- **`answer_route` is a typed field on the command.** An implementation-stage
  answer has two possible consumers, so the human names one. Rejected
  inferring the route from the blocker kind (an owner decision can go either
  way), reusing `answer_without_retry` as the "revise" signal (its name says
  nothing is retried), and adding `request_changes` to the type's action set
  (the item is a question, not an approval). The route rides the write-once
  command record so the dispatcher, a replay, and the audit trail see the
  same value; the submit gate requires it exactly on an implementation-stage
  `answer_and_retry` and refuses it anywhere else.
- **`revise_specification` is refused at submit as pending, not accepted with
  no effect.** The route needs a fresh implementation identity for the
  revised specification, and the §5.12 campaign model binds one approved
  digest per campaign with every retry copying it, while the initial
  attempt's campaign and specification-run identities derive from the
  implementation run id. Whether a revision is a new campaign or a
  digest-changing attempt is an owner decision on campaign identity; the
  plan named this as the stop condition. Refusing at the boundary keeps an
  answer from being superseded with nothing enqueued. Decision and route
  work: #1083. The app therefore sends `retry_implementation` on an
  implementation-stage question and offers no picker yet; the presentation
  type names the seam.
- **`answer_without_retry` is not offered by these producers.** Nothing
  consumes an unretried answer on a blocked run; an action with no effect
  would be decorative. The signet policy still admits it for the type.
- **The blocked outcome travels in the evidence channel, launcher-declared.**
  Rejected a top-level marker file like the commit plan: the evidence channel
  already carries launcher-fixed labels, paths, media types, and provenance
  the agent cannot forge, is persisted by the same path as every other
  claim, and never enters the repo channel. The post-writer shell composes
  one descriptor from the fixed source fragments that exist rather than
  emitting a full descriptor per combination, which kept the `sh` argument
  under the Linux single-argument limit at the maximum prompt.
- **`commit_plan_collision` remains a compatibility value but is not an
  agent-produced blocked kind.** The exporter and importer reject a trusted
  base that occupies the reserved commit-plan namespace before the blocked
  no-changes path can accept an outcome, so the prompt's advertised stop was
  unreachable. Rejected a base-aware import exception as disproportionate;
  #1086 will detect the collision and raise a daemon-owned hold before launch.
- **A blocked import audits both channels and builds no commit.** The
  importer's `ExpectNoChanges` runs after the manifest, evidence, path, and
  secret checks and fails closed on any change or commit plan, so a stop can
  never surface as an empty candidate (the #991 review finding), and a
  malformed outcome is a definitive rejection like malformed evidence.
- **`application/json` joins the evidence media allow-set** as exactly one
  valid UTF-8 JSON value, bounded by the evidence per-blob cap.

## Verification Findings

Refute-first pass by an independent fresh-context reviewer over the
returned-object trust boundaries this unit adds (the blocked outcome bytes,
the persisted decisions the engine re-reads, the needs_decision terminal, the
answer route, and the new status members). Outcomes:

- **Confirmed and fixed: implementer decisions bypassed secret scanning.**
  The importer scans only `text/markdown` evidence, so a credential written
  into a decision's text would have been persisted and copied verbatim into
  the item facts and reason, while the specifier's decisions were scanned.
  The stage now refuses a credential-shaped blocked outcome as a definitive
  rejection before any byte persists, matching the specification side.
- **Confirmed and fixed (automated review): JSON escapes bypassed the blocked
  outcome's raw-byte secret scan.** A valid decision could encode a token
  delimiter with a Unicode escape; decoding restored the credential-shaped
  text, and canonical persistence copied it into the CAS and question card.
  The raw scan remains, followed by scans of every decoded decision text field
  and the exact canonical bytes before persistence.
- **Confirmed and fixed (refute-first): a canonical whole-document scan still
  hid quote-delimited secret patterns inside JSON strings.** Canonical encoding
  re-escapes a decision field's quotes, so the GCP private-key-id rule still
  could not see its delimiters. Scanning the decoded fields closes that admitted
  path; escaped GitHub-token and quoted-GCP regressions cover both forms.
- **Disproved by refute-first: the decoded scan omits a current decision text
  field.** It enumerates the question, blocking reason, recommendation, and
  every option label and tradeoff before any blocked evidence persists.
- **Allowed by decision: the blocked outcome is canonically encoded twice.**
  The second pass scans the exact durable bytes; the input is already bounded
  to 64 KiB, so avoiding that duplicate work is not worth another return shape.
- **Confirmed and fixed (automated review): the specification producer had the
  same quote-delimited secret gap.** It scanned only the JSON-encoded decisions
  artifact, so quotes inside a decision field were escaped before the GCP rule
  saw them. The class sweep had stopped at the blocked producer even though the
  shared decision shape has two persistence paths; both producers now scan all
  decoded decision fields before their encoded artifact scan.
- **Confirmed and fixed (automated review): an undeliverable specification
  answer had no durable outcome.** A valid large question plus its answer can
  exceed the next prompt's delivery limit after the question is superseded.
  The answer path now records the same idempotent specification-revision
  failure item used by other follow-up refusals, and creates no next invocation.
- **Confirmed and fixed: the blocked outcome was read before the evidence
  caps applied.** `releasedBlockedOutcome` runs ahead of the importer, so the
  whole file was read unbounded. The declared size is checked against the
  decoder's 64 KiB cap first and the file is read through a bounded reader.
- **Confirmed and fixed: the blocked summary escaped the 512-byte summary
  bound.** The leading question (up to 4 KiB) was copied into the outcome
  record, the terminal row, and the item reason. The truncation every other
  stage result uses is now shared (`exec.TruncateSummary`) so the driver and
  the engine's equality check agree; the item reason keeps the full question
  from the persisted decisions.
- **Confirmed and fixed: a build artifact (`daemon/freesided`) had been
  committed** by an `add -A` in the first commit; removed from history.
- **Confirmed and fixed (automated review): the specifier's canonical
  decisions example showed one option**, below the two-option minimum the
  same decoder enforces, so a model copying the shape would have failed
  validation instead of asking. The example now carries two options. The
  prompt sits against the 4 KiB prompt-package budget the envelope
  headroom test pins, so the added option was paid for by compressing the
  research, specification, and reply example placeholders; the prose that
  states the enforced limits is unchanged.
- **Confirmed and fixed (automated review): typed question facts were not
  bound to the `Question` claim's content address.** A caller or reconstructed
  row could replace valid decisions, and the item validator checked only the
  claim label and producer invocation. `AgentQuestionFacts.ComputeDigest`
  now re-encodes the producer-specific canonical payload (the decisions array
  for specification, the versioned blocked outcome for implementation), and
  item validation requires the claim digest to match. This keeps a card from
  presenting or answering altered question content under the original claim.
- **Confirmed and fixed: the real convergence seed lagged the required facts
  invariant.** Its generic policy-matrix route created `agent_question`
  items without facts or a `Question` claim, so the real daemon correctly
  rejected every matrix cell before action policy ran. The harness now mints
  the same valid specification-question shape it is testing through the
  domain gate; the production API remains unchanged.
- **Confirmed and fixed (automated review): blocked evidence retained the
  agent's JSON formatting after semantic validation.** The claim therefore
  addressed raw bytes while the card's binding re-encoded the same outcome,
  and valid pretty-printed JSON could fail the digest comparison. The stage
  now canonicalizes only the validated blocked outcome as it crosses into
  durable evidence; its stored claim, size, result digest, and blob all name
  those canonical bytes. Other evidence remains byte-preserving.
- **Confirmed and fixed (automated review): answer routing scanned every
  claim instead of the authenticated question facts.** A valid supporting
  claim from another stage attempt could make a submitted answer ambiguous
  after the item was already superseded. `agent_question` routing now uses
  the invocation in its digest-bound, immutable facts and verifies that
  invocation belongs to the expected run stage; other item types keep the
  existing claim scan.
- **Confirmed and fixed (automated review): canonical JSON escaping could
  enlarge a valid blocked outcome past its durable 64 KiB bound.** Canonical
  encoding now enforces the same byte limit as decoding before the outcome
  crosses the persistence boundary.
- **Confirmed and fixed (automated review): the API property description
  required `agent_question` on every item, but the schema did not.** Both
  mirrored schemas now require the nullable key, so question facts cannot be
  silently omitted while non-question items continue to send `null`.
- **Confirmed and fixed (automated review): repeated implementation answers
  discarded the prior retry inputs.** Each operator-feedback invocation now
  inherits the authenticated source invocation's complete input list, whether
  that source is the initial implementation, remediation, or earlier
  operator feedback, then appends the new answer. The source admission
  rechecks the stored input digest. The prospective cumulative input is also
  materialized before the retry intent is recorded; a permanent size refusal
  becomes the existing durable undeliverable item instead of wedging later at
  dispatch.
- **Confirmed and fixed (automated review): an `agent_question` row and its
  copied claim could authenticate each other without producer authority.**
  Store writes and reconstruction now bind specification questions to the
  admitted `needs_decision` terminal, and implementation questions to the
  admitted blocked outcome, terminal, and immutable `freeside.blocked` claim.
  A caller-supplied or tampered row can no longer authorize an answer merely
  by keeping its own facts and `Question` claim self-consistent.
- **Confirmed and fixed (automated review): blocked-outcome crash recovery
  dropped the persisted artifact digests.** The driver now reconstructs the
  recovered blocked result from the immutable claim set, requiring exactly
  one blocked claim from the invocation. The engine therefore receives the
  same evidence address after a crash between the outcome write and private
  intent commit, and the question gate can converge.
- **Confirmed and fixed (automated review): retry inputs preserved source
  artifacts but not the question each answer resolves.** Specification and
  implementation retries now pair the accepted answer with the exact
  authenticated `AgentQuestionFacts` in a daemon-authored input. Rejected
  relying on the source input chain alone: the question is producer output,
  so it cannot be reconstructed from those inputs.
- **Confirmed and fixed (automated review): outcome-backed crash recovery
  dropped terminal usage measurements.** Before recording a failed,
  canceled, or blocked outcome, the driver now saves usage in its private
  intent and rehydrates it if recovery finds the outcome without the final
  result. Rejected enlarging the public outcome record: status and summary
  remain its authority, while usage stays private replay data.
- **Confirmed and fixed (automated review): the immutable admission reader
  did not itself recheck invocation inputs.** Before inheriting a non-root
  retry chain, operator feedback now recomputes the source invocation's input
  digest and compares it with the recorded admission. The root still admits
  exactly one specification input, so it has no extra ordered input to
  substitute.
- **Confirmed and fixed (automated review): an invalid `answer_route` mapped
  to HTTP 500.** The enum error now joins the existing deterministic request
  errors and returns 400; malformed input remains rejected with no commit.
- **Allowed by decision: `application/json` evidence validation reads the
  blob into memory** rather than streaming like the markdown and JSON Lines
  validators. The blob has already passed the evidence per-blob cap and its
  declared size by then, and `json.Valid` needs the whole value; a streaming
  validator is not worth its surface for a bounded input.
- **Allowed by decision: the engine's artifact reader is set only by the
  specification and production-publication options.** The daemon composes
  both; a composition with neither would fail every blocked terminal loudly
  rather than silently, which is the intended failure mode.
- **Allowed by decision: the dispatcher routes an answer by run ownership
  while the submit gate keys on the item's stage facts.** The two agree for
  every item these producers create (a specification question is always on
  a specification run); unifying them on the facts is a refactor of #919's
  dispatcher outside this unit's scope.
- **Confirmed and fixed (automated review and refute-first): the terminal gate
  authenticated question facts but not the offered actions.** A caller or
  reconstructed row could retain valid producer-backed facts while offering
  `answer_without_retry`, which signet permits for the type but resolves
  without retrying either producer. Both current producers intentionally emit
  exactly `answer_and_retry` followed by `stop`, and there are no legacy
  question items, so the store now authenticates that exact action set on
  writes and reconstruction. The independent pass found no legitimate runtime
  state this rejects; generic signet fixtures were updated to use the real
  producer shape.
- **Disproved by review:** a tolerated commit-blocking finding cannot slip a
  change past `ExpectNoChanges` (every such kind is fatal under both
  profiles); the agent cannot forge the evidence descriptor (root-written
  after the writer exits into a root-owned sticky directory); the shell
  composition of the descriptor is exact (`version` precedes `sources` with
  no omission, fragments single-quoted, the joined list double-quoted); the
  `answer_route` field converges on stored commands by decode and re-encode;
  a decisions terminal cannot start an implementation (the gate reconciler
  skips a terminal without a specification); crash recovery cannot present
  an artifact-bearing blocked record that disagrees with the driver; the
  needs_decision transition replays idempotently through the inbox byte
  compare with the artifact and item in one transaction; and
  `answer_without_retry` cannot be smuggled onto a blocked question because
  the item does not offer it.
- **Disproved by review: inherited retry inputs need a second recursive
  provenance chain.** The immutable source invocation plus its admission
  bind the complete ordered input list through an explicit `InputDigest`
  recomputation; the producer-specific stage identity and root specification
  check close the remaining substitution paths without another marker format.

## Revisit When

- #989's eval baseline measures how often each role asks; a rising
  `exceptional` rate on `agent_question` is the signal the prompts' bounded
  assumption rule is miscalibrated.
- #1083 decides the revision identity; the submit refusal and the app's
  fixed route go away with it.
- A third producer needs a decision shape with different limits.
