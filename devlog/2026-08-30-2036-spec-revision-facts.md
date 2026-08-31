# Specification Revision Facts

Chose a typed `SpecRevisionFacts` projection backed by durable artifacts over
reconstructing revision context in either client. The daemon now binds the
prior specification, every operator comment, the bounded unified diff, and the
agent's Addressals artifact into the approval item. The app reads only that
projection. This keeps presentation code from inferring trust from prose or
artifact position, and it lets store reconstruction fail closed when any
referenced digest changes.

Chose the request-changes command identity as the stable comment identity over
matching comments by text or array position. The daemon-created
`spec-feedback-<command-id>` artifact carries the same identity, so repeated or
similar comments remain distinct across iterations. Addressals name those
identities, and unknown or duplicate identities invalidate the elaborator
output.

Chose to permit an agent to omit an addressal for a known comment. The client
shows that omission as "No addressal claimed" and labels every present mapping
as an unchecked agent claim. This replaces the exact one-to-one addressal rule
recorded in `2026-08-11-1334-production-elaboration-boundaries.md`: the current
work contract requires an honest missing claim instead of forcing the agent to
assert a response it may not have made. Unknown and duplicate claims still
fail closed.

Chose an in-process deterministic line diff over a new dependency. The daemon
uses a bounded longest-common-subsequence pass for ordinary specifications and
ordered matching-line anchors for inputs whose product would make that pass too
large. Added and removed counts describe that deterministic edit script, even
when the rendered unified diff reaches the existing claim-text limit and is
truncated; minimum-edit optimality is not part of this bounded presentation
contract.

The refute-first pass initially treated direct database-row alteration of the
stored diff as outside the write-side contract. Automated review showed that
this also left insertion vulnerable to a caller-supplied rendering and let a
restored stored value bypass the gate. The store now authenticates both the
current and prior Specification claims against their immutable artifact rows,
re-derives the exact bounded diff from their digest-bound inline bodies, and
repeats the gate after update normalization. This closes insertion, update,
and reconstruction without a new table or a blob-aware store boundary.

Chose immutable request_changes commands as the authority for revision-comment
coordinates over trusting a matching Research artifact alone. The store now
requires each comment identity, item identity, iteration, and normalized body
to agree with both the command row and its feedback artifact. A suitably named
artifact can no longer invent operator-comment lineage. Each revised item also
re-authenticates its prior revision and must extend that exact comment history
by one command, so a later body cannot silently omit an earlier operator
comment. Research-only elaboration iterations may still separate those
commands because iteration identifies the producing invocation, not a dense
approval sequence.

Chose dedicated specification and unified-diff readers over rendering either
long-form body inline in the decision card. The revision lead is the
daemon-authenticated iteration, diff counts, and operator-comment lineage;
only the mapped agent responses carry an unverified label. The actions remain
next. Below them, the specification is named as the daemon-bound approval
object and the full bounded diff is a separate drill-down. This follows the
plan's presentation altitude, avoids treating the specification as a generic
unverified claim, and preserves unified rather than side-by-side scope. The
Mac opens these readers in its existing inspector; iPhone uses sheets.

Preserved reconciliation for an invocation admitted under the exact prompt
digest immediately preceding this change. That immutable contract asked the
agent to copy the feedback body into `comment`; the upgraded daemon translates
that body to the authenticated feedback artifact's command identity before
applying the current addressal gate. Current prompt admissions remain strict
on `comment_id`, and ambiguous repeated legacy bodies fail closed.

Revisit when a standard-library-quality diff implementation already present in
the dependency graph can replace the local bounded renderer without changing
its wire shape, or when a general artifact viewer can adopt these readers
without weakening the approval object's trust labeling.
