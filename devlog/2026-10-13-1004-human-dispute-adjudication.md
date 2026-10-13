# Human Decisions for Routed Review Disputes

Issue: #1016. Implements the six decisions in its owner-authorized plan.

## Contract Decisions

Chose one complete `adjudicate_findings` command over partial decisions. Each
finding has exactly one offered route, bound to the displayed item version,
candidate head, and artifacts. Partial decisions need a separate lifecycle;
they remain in #1863.

Chose a `human` adjudication producer and a `Decision` reference to the durable
command, item, and item version. Human decisions have no confidence or
alternatives. Remediate records required/allowed, decline contradictory, and
defer adjacent. A human can create revision 1 when model adjudication parked
before producing an artifact, or a successor preserving the batch and every
unselected entry. A successor carries exactly one of feedback or decision.
Existing artifacts retain their canonical bytes and digests.

Chose decline and defer for every finding, with remediate only for a canonical
repository path matched by the run's declared paths. The engine and store use
the same route derivation. The store authenticates the displayed message,
location, severity, external identity, completion evidence, and candidate head
against persisted records instead of trusting the card's fields.

Admitted external findings use the existing review-cycle admission gate. A
clean review may bind only its admitted external findings. This does not
enable the external cycle's production ending; #1989 owns that transition.
Shadow findings and publication-continuation cards retain their controls.

Chose `item.DecidedAt` and canonical entry order for the human artifact's
bytes. Recovery consumes a decision before the parked-batch return and reuses
the artifact when its command identity already matches. A crash before or
after persistence cannot create a different revision. Decline, defer, and
remediation then use the existing execution and producer gates.

## Refute-First Findings

Independent review exposed real shared-path and boundary failures: a generic
dispute writer could offer the new action on unrelated endings, incomplete
rows could conclude a partial batch, traversal paths could match declared
scope, and human revision 1 could remain parked in downstream execution gates.
The implementation restricts the writer, requires complete rows and canonical
paths, and recognizes authenticated human decisions in both execution gates.

The persistence gate also needed an explicit comparison between the displayed
head and the review record's head. Command-to-card equality alone did not
authenticate the reviewed candidate. A regression now rejects another head
even when all finding and completion fields are authentic.

Duplicate choice and forged rationale concerns are covered at the canonical
command and command-derived entry boundaries. Another guard against reusing a
command in an invented successor was rejected: the actual engine reuses the
existing command-bound artifact before constructing a successor.

Revisit when partial decisions or external-cycle endings gain their own
execution contracts, or when a run can change its declared paths in place.
