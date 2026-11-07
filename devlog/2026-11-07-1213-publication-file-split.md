# The Production Publication File Splits Into Seven Concerns

#2761 moves seven concerns out of
`daemon/internal/engine/production_publication.go`, byte for byte, into
files of their own in the same package. The file goes from 6,160 to 3,235
lines. This note records where the boundaries fell, the tier of each new
file, and what the refute-first pass found. It is the first unit of a
proposed chain that makes publication states explicit (the plan comment
on #2761); the later units are not filed by this one.

## Decision

- **Chose concern boundaries over a size target.** Each new file holds one
  thing a reviewer can name: task keys and row reconstruction, completion
  authentication, export recording, outcome recovery, verification, the
  remediation source tree, and run holds. What stays is the workflow type,
  its constructor, the reconcile loop, the binding re-gate,
  `reconcileTask`, readiness, published and blocked completion, PR
  bindings, the milestone writer, and the attention items. Rejected:
  splitting `reconcileTask` itself or naming its states here. That changes
  code, not only its home, so it belongs to the later units.
- **Listed six files as Tier A** under "Publication decisions in the
  engine", each with its reason:
  - `production_publication_task.go`: the restart reconstruction gate on a
    decoded task.
  - `production_publication_completion.go`: authenticates terminal and
    ready records.
  - `production_export_record.go`: commits the trusted export and the task
    that replays it.
  - `production_publication_outcome.go`: authorizes a committed
    publication outcome.
  - `production_verification.go`: binds the approved recipe, runs
    verification, and stores its verdict.
  - `production_remediation_source.go`: authenticates the remediation
    source tree.

  `production_publication.go` keeps its listing and its reason: the
  binding re-gate before publish stays there.
- **Left `production_publication_holds.go` off the Tier A list.** It holds
  `pruneHeldTaskRetries`, `recordAttendedPublicationHolds`,
  `deferHeldTask`, the environment, retryable, image-upkeep, and
  attended-mode hold writes and clears, and the pace state they share.
  These tell the operator why a task waits and pace its retries.
  `holdRetryAfter` only delays the next attempt, and
  `successorObservationPending` only counts attempts toward a blocking
  card that `production_publication.go` raises. No gate reads a run hold
  these functions write to decide whether a candidate may publish. So an
  edit here gets Tier B review.
  `recordAttendedPublicationHolds` calls the Tier A
  reconstruction gate, but only to find which run to hold; a refusal
  joins as an error and grants nothing.
- **Kept `appendPublicationMilestone` in `production_publication.go`**,
  against the issue's table, which put it in the holds file. The
  milestones it writes are authority:
  `AuthenticateProductionPublicationReady` requires a `publication_ready`
  milestone with the matching invocation, and
  `AuthenticatedProductionRunConclusion` reads `publication_blocked` to
  decide whether a run's conclusion is final, which admits a new attempt.
  The plan named this fallback: leave the function behind rather than
  list the holds file. Rejected: listing the holds file as Tier A, which
  would put the whole file under the full bar for one writer.
- **Left the blocked-task holds behind too.** `holdBlockedTask` and
  `supersedeBlockedHold` carry the requested decision a blocked item
  offers, and `holdLaggingSuccessorObservation` sits with the PR
  bindings. Both are Tier A concerns.

## Refute-First Pass

The moved code includes the reconstruction gate, completion
authentication, and the verification verdict, so the unit took the
refute-first pass (`docs/agent-workflow.md` §refute-first).

- **Disproved by a check: a moved declaration changed.** A scratch
  `go/ast` program cut each top-level declaration by its AST span, doc
  comment included. All 156 declarations at `7a79d9ec1` appear exactly
  once among the package's non-test files, byte for byte: 72 in
  `production_publication.go` and 84 across the seven new files. Every
  declaration in a new file is one of the base's. The base file has no
  floating top-level comment that a move could strand. A fresh-context
  reviewer found the same with a line diff: the new
  `production_publication.go` is a pure subsequence of the old one, and
  the removed lines equal the new files' bodies apart from imports.
- **Disproved by a check: initialization order changed.** No `init`
  moved. The moved package-level values are two constants and
  `errTaskMarkerMismatch`, an `errors.New` with no dependency, so file
  order cannot change them.
- **Disproved by a check: a file-keyed reference went stale.** The
  hold-exit enumeration is the only code that keys a moved function by
  file. Its keys for `RecordProductionExecutionExport` and
  `recordPublicationEnvironmentHold` follow the functions with their
  reason text unchanged. Its `reconcileTask` and `completePublishedTask`
  keys stay correct, since neither moved. No script, workflow, or lint
  config names a moved function.
- **Confirmed and fixed: a Tier A writer was headed for the Tier B
  file.** The reviewer, asked to refute the Tier B claim, traced the
  milestones `appendPublicationMilestone` writes to the two gates above.
  The function stays in `production_publication.go` (see Decision).
- **Confirmed and fixed: a listed reason left out recipe approval.**
  `production_verification.go` binds the recipe digest and passes the
  approved recipe set into verification, so its listed reason names the
  recipe.
- **Not run: the old-versus-new input comparison.** Each declaration's
  source is identical and stays in the same package, so it resolves the
  same identifiers and compiles to the same code. The byte comparison
  already measures that.

The reviewer also found two gates outside this change that are not on the
Tier A list: `signet/publication_reevaluation.go`, which authenticates
block resolution, and `engine/production_attempt.go`, which admits a new
attempt only on a final conclusion. They predate this unit.
Follow-up: #2811.

Revisit when a later unit of the chain moves code between these files:
the tier of each file follows what it decides, so a gate moving into the
holds file puts that file on the list.
