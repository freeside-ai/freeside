# Keep Scope Decisions Bound To The Candidate

Issue: #1214.

A required documentation change outside the approved paths is an unresolved
obligation. Keeping the restriction must preserve that obligation in the
operator's decision and the published evidence. It cannot mint permission to
edit the path or waive a blocking review finding.

## Decisions

- Keep the completed candidate and report the conflict in a third fixed
  evidence channel, `scope-conflict.json`. Letting `blocked.json` carry changes
  would break its established no-candidate meaning.
- Hold publication before verification, review, and forge effects. The
  publication task already owns the imported candidate and its durable replay;
  a terminal-stage gate would split that ownership.
- Offer `answer_without_retry` to keep scope, and `stop` to end the attempt.
  Widening scope requires a new run with a new immutable policy. The current
  feedback export cannot become a candidate, so `answer_and_retry` would not
  implement the decision.
- Keep paths, declared scope, and candidate head as daemon facts. Keep the
  reason and proposed alternatives as an agent claim. The store authenticates
  the question against the producing invocation, execution export, resolved
  policy, and persisted claim. It does not import the engine's private task
  decoder; the export is the candidate authority committed with that task.
- Reject malformed claims, in-scope paths, changed conflict paths, conflicting
  outcome channels, and specification-stage use. Reuse the export path gate
  and the importer's underlying path matcher rather than introducing another
  interpretation of path scope.
- Reconstruct the accepted answer from the question and immutable command on
  each pass. No new outbox request or mutable task payload is needed. Both
  publisher authorization and ready-item persistence re-gate those facts.
- Render the decision in its own publisher-owned PR section after advisories
  and before disposition history. It has a 12 KiB ceiling reserved from the
  candidate prose budget and never enters publication identity. Candidate
  prose cannot supply the heading or markers.

## Boundary Findings

The refute-first review confirmed that pretty-printed JSON has a different
digest from its canonical encoding. The stage persists a canonical question
claim under a distinct artifact ID while retaining the original export bytes
and digests for replay and terminal identity. The engine also binds that claim
back to the released evidence; missing or mismatched claims cannot bypass the
hold. The shared decoder's existing last-value rule for duplicate JSON keys
remains unchanged; the final decoded fields must pass every scope gate.

The review also found that a remediation could replace the candidate and omit
the earlier conflict. Candidate authorization now checks the preceding export
history. A replacement without a current decision covering the known
obligation is quarantined with an explicit notice. Consent on another head
is not reusable. A fresh valid conflict on the replacement can receive a fresh
decision. Historical ready cards only inspect history through their candidate.

PR repair now recovers the run from the stored publication intent and its
producing admission before accepting optional scope facts. Publication identity
omits the run and body sections, so accepting a caller's empty run could erase
the decision during repair. The repair regression checks empty and foreign run
IDs against an actually published scoped candidate. Remediation compares only
persisted task fields; its reconstructed scope decision is not durable task
authority.

The forge renderer screens command IDs and operator answers with the existing
high-signal credential detector before truncating them. A detected credential
redacts the entire field in the PR section, while the local immutable command
and scope facts remain intact. Command length checks and Markdown escaping
alone did not prevent client-provided credentials from reaching a public PR.
Regression checks cover both fields, including credentials spanning or beyond
their rendering limits. Redaction preserves the required path obligation and
decision without blocking publication on private conversation text.

Publication identity intentionally remains shared across attempts, so scope
ownership must also be checked across publication intents. A new attempt or
repair re-authenticates matching pending, dispatched, and quarantined intents
and their producing run/export before comparing the accepted scope decision.
A conflicting statement, including omission, stops before a new intent or
forge effect. The guard uses durable commands even if someone removes the
section from the live PR. Scoped publication requires the execution-bound
entry point so the intent always retains its owner run; copying forge prose
or adding consent to the publication identity would weaken existing contracts.

The domain enum check now obtains module dependency export data through
`go list`, because the reused path gates reach existing module dependencies.
It caches that listing once per source importer and still checks the same
declarations and enum registrations.

## Operator Inputs

`ingestPromptPackage` reads and hashes the supplied file directly. The script
does not rebuild an operator's external prompt package from this repository.
For a production exercise, the operator must supply the updated implementer
prompt through `FREESIDE_REAL_RUN_PROMPT_PACKAGE` before admission; changing this
repository cannot change an already admitted run's prompt digest.

Revisit when feedback exports can become publication candidates, or when an
explicit contract allows consent to survive a replacement head. Until then,
scope expansion and replacement-candidate consent require their own approval.
