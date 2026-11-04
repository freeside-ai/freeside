# The Publication Core Moves To A Tier A File Of Its Own

#559 moves the production publication core out of
`daemon/internal/engine/fake_publication.go` and `fake_workflow.go` into
`publication_core.go`, and drops "fake" from the four package-private
names production calls. This note records why that refactor is Tier A, why
the new file joins AGENTS.md's Tier A Paths list, and what the refute-first
pass found.

## Decision

- **Classified the unit Tier A** under AGENTS.md Assurance Tiers. The
  rename edits listed Tier A paths (`production_publication.go`,
  `production_reentry.go`, `publication_closure.go`), and the path rule
  makes a change that touches any of them Tier A as a whole. Rejected:
  treating a pure move and rename as Tier B because it changes no
  behavior. The tier follows what the code does, not how much of it
  changed.
- **Added `internal/engine/publication_core.go` to the Tier A Paths list**,
  under "Publication decisions in the engine". The file holds the
  production git publish transport (`GitPublicationTransport`, which
  `cmd/freesided/claude_driver.go` builds), the publication lock, and
  candidate identity, which are what the "GitHub publish" and "Publication
  decisions in the engine" categories name. Its old homes were never on the
  list, so before this move the transport sat in an unlisted file that
  a reader would take for fake-lane code.
- **Named the blob helpers for the artifact store, not publication**
  (`loadArtifactBlob`, `verifyArtifactBlob`): drift audit, adjudication
  inference, operator feedback, and remediation read through them too, so
  "publication" would mislead as "fake" did. The owner accepted the four
  names in the plan for #559.
- **Also moved `digestBytes`**, which the issue's list leaves out. The
  moved `digestJSON` calls it and production calls `digestJSON`, so left
  behind it would keep production reaching into a fake-named file, which
  the issue's rule (a declaration moves when non-fake engine code calls
  it) covers once `publication_core.go` is that caller. The pre-push
  reviewer found it. Rejected: keeping it in place with a recorded reason,
  since none applies; unlike `FakePublicationReadyItemID` and
  `FakePublicationBlockedItemID`, it owns no fake-lane vocabulary. No other
  fake-file declaration is reachable from the moved code.
- **Left `fake_workflow.go`'s run-item helpers in place** (`runSubject`,
  `initialItem`, `feedbackItem`, and their ID helpers). `invocation.go` and
  `task_abandonment.go` reach them only on the 1A.0 fake run's
  conversation-feedback path, so they belong to the fake lane. The owner
  accepted this in the plan.

## Refute-First Pass

The moved code includes the publish transport and the publication lock, so
the unit took the refute-first pass (`docs/agent-workflow.md`
§refute-first).

- **Disproved by a check: a moved declaration changed.** A scratch
  `go/ast` program cut each declaration by its AST span, doc comment
  included, and then compared every moved declaration's source text at
  `0267e409` with its text in `publication_core.go`. All 37 match byte for
  byte, and each is declared exactly once in the package. The transport
  block (`fake_publication.go:57-237` at the base) left no line behind.
  The first comparison failed on two of the four one-line
  `gitPublicationCheckout` methods: blank lines between them had made
  gofmt drop their column alignment. Restoring the original grouping made
  the texts equal.
- **Not run: the old-versus-new input comparison.** The section's
  generated-input comparison measures equivalence a diff can only suggest.
  Here the source of each declaration is identical and stays in the same
  package, so it resolves the same identifiers and compiles to the same
  code; the byte comparison already measures that, and a generated-input run
  would add nothing.
- **Disproved by a check: the rename touched more than four names.** After
  the rename, every changed line pairs with its old line once the eight
  names are masked, so no other text changed. No file under `daemon/cmd/`
  or `daemon/internal/exec/` changed, and the old names appear nowhere
  under `daemon/`.
- **Confirmed and fixed: a production dependency stayed in a fake file.**
  A fresh-context reviewer, asked to prove the change wrong, found that
  `digestBytes` stayed behind (see Decision). It found no changed text, no
  lost or duplicated declaration, no import that resolves differently, and
  no shadowing from the new names.
