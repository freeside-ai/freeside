# Fence Claude Execution Instruction Bodies

Chose `claude_explicit_bundle_v3`, with each trusted-base repository body and
the operator-host body inside a backtick fence one character longer than its
longest backtick run (minimum three), because it keeps every later path scope
and the operator-host block outside any Markdown construct an earlier trusted
CLAUDE.md opened. This is the sibling of the review-bundle fix
(`2026-08-25-2131-review-bundle-fences.md`, `codex_explicit_bundle_v2`); the
Wave 6 exit audit classed the surviving Claude-side flaw a plan §12
control-plane-trust failure, so it landed as blocking rather than a dormant
deferral. The per-source `RepositoryManifestDigest` and `HostDigest` still bind
the raw bytes, upstream of the fence framing; only the composed `BundleDigest`
changes.

Kept legacy `v2` readable at the journal reconstruction re-gate so an in-flight
run journalled before the upgrade survives a binary upgrade. This is the whole
compatibility mechanism: unlike the review engine, ward's recovery never
recomposes an instruction bundle. It loads the persisted binding into
`preparedInstructions` and dispositions the run against the runtime world, so a
`v2` binding only has to stay *validatable* to recover. The pre-fencing bytes
are therefore never re-emitted, and no supersede-and-retry path is needed.

Rejected refusing `v2` at the re-gate because it would turn a recoverable
in-flight run into an `ErrInvalidJournalRecord` at exactly the daemon-upgrade
boundary this record exists to survive: a persisted contradiction, not a safe
teardown. Rejected sharing one fence primitive with `exec` because reaching the
unexported review helper means editing the review file (#946 Non-goal) and this
is only the second use of the shape; duplicated the ~20-line helper with a
mirror comment instead, leaving a rule-of-three promotion for later. Rejected
content escaping and synthesized closing constructs for the same reasons the
review fix did: an encoded dialect, and Markdown constructs are not only
fences.

Revisit when the bundle format stops being Markdown, when a recovery consumer
needs to recompose a historical composition version rather than disposition it
against the runtime world, or when a third fenced-literal consumer makes the
shared primitive worth extracting.
