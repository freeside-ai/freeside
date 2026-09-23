# Source Issue Text for Publication Authoring (Issue #1493)

## Same-Repository Read

Chose to read the source issue's title and body through the daemon's target
repository installation token when the bound issue subject or client source URL
names that repository. The publication author needs the issue's requirements
to explain the change and propose whether the pull request resolves them. A
cross-repository URL contributes only its reference. Reading another
repository would require separate credentials and a visibility classification;
neither belongs to this work unit. The same-repository text takes the target
repository's sensitivity class, and the inference builder's cross-visibility
guard remains in place.

## Failure Decision

Chose the durable v1 authoring fallback and `daemon_fallback` closure origin
when the issue read fails. Publication still proceeds, but no `resolves`
answer based on an unread issue gets policy approval. Rejected returning a
reference-only input after a failed read: it would repeat the defect for the
closure propose site. A transient read error costs this candidate its authored
text and does not retry at the same head and base, matching the existing
authoring checkpoint rule.

Both author sites share one engine-private source observation keyed by run,
head, and base, persisted before either consumes it. The row also binds the
repository identity and issue number. It records a read failure as well as
successful prose, so recovery or an issue edit between explain and propose,
including across a daemon restart, cannot change the candidate's source input.
Rejected independent reads: review demonstrated that a failed explain read
could otherwise be followed by a successful propose read and close approval.
This observation belongs to the candidate's authoring input; a new head or
base gets a new observation. It is not a repository-wide issue cache.

## Refute-First Findings

The shared forge response decoder replaces invalid UTF-8 before a caller can
inspect decoded strings. This would have hidden malformed title or body bytes,
so the issue observation now validates raw response UTF-8 under the existing
16 MiB bound before decoding. The issue reconciler shares this read and will
also refuse an issue response with invalid UTF-8, even if it needs only the
number and state. That fail-closed behavior is accepted.

The authoring read also rejects missing, null, empty, or whitespace-only
titles. GitHub requires a title, so accepting those responses could leave
the author with no requirements when the body is null. This check lives at
the authoring read boundary; lifecycle-only reconciliation keeps its existing
title-independent behavior.

The same nonblank-title invariant is checked when reconstructing a successful
source observation. A fresh refutation confirmed the store accepts opaque
inbox payloads, so identity checks alone cannot prove restored prose is valid.
Malformed saved text fails closed without another forge read; a null text
pointer remains the valid, durable read-failure state.

The fake forge checks a different returned issue number, a pull-request
number, missing issue, unsolicited 304, invalid UTF-8 in both title and body,
and a response above the bound. Each refuses to supply author text. The engine
selects no read for a cross-repository client URL; an issue subject must match
both the target repository name and numeric identity. The integration harness
confirms both author sites receive same-repository prose. When the issue read
fails, the candidate still publishes with a non-closing reference, neither
site is called, and a later pass at the same head and base does not retry
authoring. The production token source also binds the repository's numeric
identity, preventing a reused name from redirecting the read.

Revisit when: cross-repository source issue text becomes an authorized input,
or the issue reconciler needs to tolerate malformed prose while still
observing number and state.
