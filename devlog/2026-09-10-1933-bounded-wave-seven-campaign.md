# Bound Wave 7 Closure And Align Review Locations

The owner approved one bounded campaign after the ordinary gh-imgup #79 exit
succeeded and a separate controlled challenge failed before accepted review.
The real reviewer found an introduced defect in an unchanged function body;
the candidate changed the signature and documented contract. The engine
rejected that unchanged-line location, while the producer instructions omitted
the changed-line overlap requirement. This is evidence of mismatched guidance,
not an accepted review or proof that remediation ran. Refs #1291 and #1275.

## Location Decision

Keep the existing acceptance rule and teach the reviewer to identify the causal
changed line when a new change makes unchanged code incorrect. Keep ranges
tight; do not stretch them, choose unrelated lines, or substitute whole-file
locations when a relevant changed candidate-side line exists. A pure-deletion
hunk has no such line. Use the existing whole-file marker on its touched path
and identify the removed code and its causal failure precisely in the
explanation. Deleted files and genuine whole-file findings on changed files
retain their existing representation.

The shared production prompt and JSON-schema descriptions now state this rule.
Both providers' prompt protocol identities change so approval binds the revised
guidance. The schema's accepted object shape, consumer validation and finding
adjudication semantics do not change. Review exposed the removed-guard case;
the existing domain and diff gate already admit its file-level location, so
extending the location representation or admission rule was unnecessary.
Actual-git regressions distinguish signature changes, which require a concrete
changed anchor, from pure deletions, which have only a file-level anchor.
Weakening overlap or repeatedly sampling
the old prompt was rejected because either would conceal the mismatch instead
of fixing the producer's contract.

## Campaign Decision

The owner authorized one additional submission after the old two-submission
budget was consumed. Use fresh isolated state and corrected, repository-specific
instruction bindings. Keep the existing attempts, pairing and approvals as
evidence. The unused same-candidate recovery approval remains separate.

Require an actual accepted finding, adjudication, remediation, trusted checks,
independent oracle, corrected-head re-review and publication. A clean first
review or rejected finding cannot satisfy that chain. Bound the campaign to
three engine review rounds, at most two remediation passes, two hours through
publication and 30 minutes for client/lifecycle closeout. Unexpected gating
defects or exhausted budgets stop the campaign; they do not authorize another
repair. Source merges and the final closure decision remain with the owner.

The owner explicitly amended only the parked #1002 recommendation subclause of
the Wave 7 evaluation to accept separately labeled deterministic engine,
command and client evidence. Live parked recommendation evidence remains not
demonstrated. A material in-scope finding naturally takes deterministic
adjudication to remediation and does not promise that separate card. Forcing a
model route or injecting a finding would not establish live acceptance.

One direct-route sample supports a concrete dispatch-calibration observation,
not a frequency claim. Actual client inspection and lifecycle restoration stay
live requirements. The fresh run restores the original supervised deployment;
it does not require an unfinished-run restart or a retained-runtime upgrade.

Revisit when an introduced defect cannot be represented by a truthful changed
location, a new campaign needs a different evidence standard or budget, or live
parked recommendation evidence becomes available. None of those conditions
silently revises this decision or the recorded earlier failures.
