# Follow-Up Filing Credentials and Composition

Issue #1634 connects the approved-follow-up dispatcher to the running Claude
daemon. The [dispatcher note](2026-10-05-1805-follow-up-filing-dispatch.md)
records the filing and recovery rules; the
[identity note](2026-10-06-1205-follow-up-filing-dispatch-identity.md)
records why recovery must re-read the dispatching App's identity.

## Separate Filing Authority

Chose a filing token of `issues: write` and `metadata: read` over adding
`issues` to `PublishPermissions`. The registration requests the union, but
publication still requests its original grant. An installation that has not
accepted the new permission can therefore keep publishing while filing fails
before dispatch and raises `follow_up_filing_refused`.

The token source resolves current trust and the installation before every
cache lookup. Its key includes the registration, installation, and repository.
A changed binding cannot reuse a prior binding's token. Each successful mint
records both requested and granted `issues` values in the columns #1768 added;
no filing token can circulate without the audit write succeeding.

Chose a separate bot identity resolver over reusing `commitAuthors`. The
resolver authenticates its identity reads with its token source. Reusing the
publication resolver would put publication authority on the filing path and
could report a publication grant failure as missing filing permission.

## Composition Choices

The plan proposed the separate resolver and a one-minute sweep. The owner
assigned implementation without vetoing either; treating that as acceptance
is the implementer's interpretation, not an explicit owner statement.
The sweep bounds crash recovery and settle delay; an approve wakes the filer
immediately. The fake driver has no filer. The binding returns a nil interface
when composition is absent, avoiding a typed-nil wake target.

Chose a seventh return from the existing transport constructor over a new
transport result struct. The unit adds one companion to an existing chain;
a broader constructor refactor would add unrelated review surface.

## Refute-First Findings

A fresh-context credential review attempted to refute the narrow grant,
unchanged publication authority, current-binding cache, and lossless audit.

- **Disproved:** filing can accept a publication or unknown scope. Exact map
  comparison rejects both, as the negative mint tests show.
- **Disproved:** publication now requests `issues`. Its unchanged request
  golden passes; the audit test also requires an empty issues scope.
- **Disproved:** a cached token bypasses current authority. Resolution occurs
  first, and removal of the installation prevents a cache hit.
- **Disproved:** the filing audit drops authority. The real store recorder's
  round-trip checks every requested and granted permission field.
- **Disproved:** composition shares the publication identity credential.
  The constructor passes the same filing source to the filer and its own
  resolver; neither receives the publication source or `commitAuthors`.
- **Disproved:** a fake-driver approval wakes a typed nil, or startup loses
  an earlier approval. Binding tests cover both absent-composition cases;
  the startup sweep discovers approvals and the wake channel buffers one.

The live recovery test records cleanup identities before deliberately dropping
the response, then reopens the same database without reseeding. It cannot
recover a cleanup identity from a real response lost before the test transport
receives it. The operator must inspect the test repository after that failure.

## Revisit When

- Approve-command volume makes a minute's full scan too costly, or recovery
  delay needs a different bound.
- #1437 implements approved-specification comments. Decide then whether its
  identical permissions should share the filing set's name and cache.
