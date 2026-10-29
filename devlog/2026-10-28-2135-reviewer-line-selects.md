# A Task's Reviewer Line Selects The Review Agent

Mandatory note: a contract change (review admission's selection source), a
credential surface (another identity's setup token reaches a review
launch), and a reconstruction trust boundary (the store re-gates a cited
task line) (#1981).

## Decisions

Chose to resolve a `reviewer` task line the way writer admission does over
keeping the "the line must name the composed agent" refusal. Engine and
store now share one line-resolution path: `resolveLineAgent` serves both
`resolveAttemptRole` and `CheckReviewRole`. A review snapshot records
`task_line` and the line id. A card still cannot select a review agent,
because nothing writes a review card (#869). The store holds a cited line to
a recorded line of the run's task and to the snapshot's role, at write and
at read, so a writer's line never stands in for a reviewer's.

Chose to carry the selected agent in the snapshot and derive each launch
from it over recomposing a source per agent. The routed source reads model,
effort, and identity from the request's admission at both launch sites
(`RequestReview` and the relaunch from `Inspect`). A request recorded
before admission existed keeps the composed values.

Chose an in-memory credential for another identity over a second host
snapshot. A Claude identity enrolled through `freesided` has no host file;
its setup token lives on the enrollment generation volume.
`wardstore.CredentialReader` reads the generation the snapshot admitted,
under the identity's read hold, and refuses a store whose digest is not that
generation's. `ward.ReviewCredential` holds the body for one launch, formats
as redacted, and reaches the lifecycle only through `readReviewAuth`. The
same snapshot-volume digest check covers both kinds of credential, so a
volume that does not hold the read body cannot start. The lifecycle refuses
a launch carrying both or neither, and refuses a credential outside
setup-token mode. A Codex OAuth store refreshes in place and has no
in-memory form.

The lineup identity keeps its host snapshot. That path is the deputy's
configured `review.auth_snapshot`, and changing it was a non-goal. Reading
the lineup identity from its volume would also make every lineless review
depend on the enrollment read.

Kept the review configuration digest at the composed source's. The
snapshot records what the line selected (model, effort, identity, agent
digest), and `SharesSegment` separates two records with one configuration
digest and different agent digests. A digest per selected agent would
re-approve configuration nobody changed.

A Codex routed source refuses a request naming another identity, as a
configuration failure, before anything is journaled. The daemon composes no
reader for it.

The daemon's admit recheck holds a stored snapshot to the line's current
resolution on role, identity, model, and effort, as before; only the
identity it compares against moved from the composed one to the line's.
Holding the enrollment as well was considered and dropped: both minting
paths (`auth add`, `auth adopt`) name an enrollment `identity/client` and
the id is the primary key, so an identity has one enrollment per harness.

Chose to retry a credential read that can succeed later over parking the
run. A live mutation lease, an ended read hold, or a runtime or I/O failure
is a transient review failure. A store that is not the admitted one
(unsupported, generation changed, digest mismatch) refuses the review as a
configuration failure, as a refused snapshot does. The reader classifies;
the source keeps a class it is handed and otherwise reports configuration.

## Departures From The Plan

- The refusal for another identity without a reader lives in the ward
  source (`requestLaunch`), not in the cutover's `reviewAdmission`. The
  source is the only place that knows whether it can deliver the
  credential, so the composed identity needs no daemon-side branch.
- Six credential read sites route through `readReviewAuth`, not four: the
  two in `codex_review.go` (spec build and final verify) read the host
  snapshot too.
- The store gate also checks the line's role.

## Refute-First Findings

One fresh-context reviewer attacked the full diff before the credential and
trust-boundary work was committed.

- **Confirmed, fixed:** every credential read failure was a configuration
  failure, so a store mid-renewal parked the run for an operator. Now
  classified as above.
- **Confirmed, fixed:** `ReviewCredential` defined `String` and `GoString`
  only, so `%d` or `%x` on the value would print the body. It now has
  `Format` and `MarshalText`, as `SetupToken` does, and a test covers every
  verb.
- **Confirmed, fixed (tests):** the relaunch of a recorded request from
  `Inspect` and a full Claude launch from a read credential had no test.
  The launch test deletes the host snapshot first, so only the read body
  can reach the volume, and checks the binding digest and the wiped stage.
- **Disproved:** the composed identity keyed on identity rather than
  enrollment could run one enrollment's credential for another's snapshot.
  Unreachable: an identity has one enrollment per harness (see above).
- **Disproved:** a leak through the request, journal, binding, logs, or
  error text. The body is never on `exec.ReviewRequest`, the binding keeps
  only its digest, refusals are fixed sentences, and the host stage is
  wiped by defer and swept by recovery as on the host-snapshot path.
- **Disproved:** a decoded or caller-supplied field selecting a credential
  the daemon did not admit. Admit runs before the launch is derived on both
  paths; the reader refuses any generation that is not current; the store
  re-gates a cited line at write and at read.
- **Disproved:** the line's agent text in a refusal. `resolveReviewLine`
  checks the tree first and refuses by line id.
- **Allowed by decision:** token copies are left to the collector, as on
  the host-snapshot path; wiping them is not a regression this unit fixes.

Revisit when a Codex enrollment can be read from a volume, or when the
deputy's lineup reviewer moves off its host snapshot.
