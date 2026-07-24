# Trusted Installation Grant Reconciliation

Issue: #263.

## Decision

Chose a publish-local `InstallationAuthoritySource` contract over deriving
authority from trust profiles, mint audits, or GitHub observations. One
registration snapshot carries the approved owner set, canonical
installation-to-repository bindings, the active epoch and durable intent
revision, and at most one pending install-or-expansion envelope. Trust profiles
identify repositories but not registrations or installations; mint audits are
historical effects rather than owner authority; GitHub is the untrusted state
being reconciled. A domain or store schema would be premature before the
onboarding flow owns persistence, but the source and recorder ports now fix the
minimum durable shape that flow must implement.

Chose aggregate bindings, one record per installation with its complete
repository-ID set, over one row-shaped binding per repository at the janitor
boundary. The remote object under review is an installation grant set, so the
aggregate makes exact-set comparison and duplicate-installation rejection
explicit. The source may persist normalized rows behind the port later.

Chose a pending envelope as a reconciliation exception only. It must match the
snapshot's active epoch and durable intent revision, be unexpired, require
`selected` mode, bind the expected numeric account and installation when known,
and carry both the current trusted set and exact post-change set. An expansion
accepts either the unchanged current set while GitHub has not applied the
selection update or the exact post-change set after it does. Both states
preserve mint authority only for the current trusted repositories; the added
repositories never enter the runtime allow-set. A new pending installation
enters no mint allow-set at all.

Chose a janitor-only token requesting only `metadata: read`, with no
`repository_ids`, followed by bounded complete reads of
`/installation/repositories`. The token response and every repository page are
untrusted. Permission or selection broadening, missing or changing total
counts, short pagination, excessive counts, and duplicate or non-positive IDs
all fail the pass. Once a token value exists, revocation runs under a separate
bounded cleanup context even when the parent operation is canceled or token
validation fails. A pass publishes no coverage unless revocation succeeds.

Chose terminal local quarantine before remote suspension and deletion. The
recorder contract atomically audits the safe numeric evidence and invalidates
every trusted binding and pending envelope for the installation before either
destructive call. This ordering is stricter than invalidating after deletion:
a suspension or deletion failure leaves no local authority capable of minting.
There is no unsuspend path. A later pass may retry deletion as an unbound
installation, but cannot restore the invalidated binding.

Chose the same active-janitor gate for public and private registrations. App
visibility changes who may install the App, not whether a trusted owner can
widen an existing grant. Runtime coverage is therefore keyed by exact
registration, installation, and repository IDs for every visibility.

## Rejected Alternatives

- **A trusted owner implies a trusted installation:** rejected because an
  unsolicited or replacement installation under that account has no canonical
  repository binding and must be deleted before a full-grant token is minted.
- **Trust the token-creation response's repository list:** rejected because it
  is another returned object and does not prove complete bounded pagination of
  the list-repositories endpoint.
- **Persist the contract in a new store schema now:** deferred because no
  onboarding consumer yet creates or promotes these records. Encoding a storage
  layout before that transaction exists would make the janitor choose the
  onboarding model. The source and atomic quarantine recorder are the stable
  ports the later store adapter must satisfy.
- **Suspend, narrow, and automatically unsuspend:** rejected because suspension
  prevents the enumeration needed to prove recovery and unsuspension would
  reopen the compromised authority. Recovery is a fresh native installation.
- **Publish registration-wide coverage for an exact pending envelope:** rejected
  because the resolver could otherwise turn the non-authoritative exception
  into a worker token. Coverage retains an explicit trusted repository allow-set.

## Refute-First Findings

- **Confirmed:** choosing the trusted candidate before the pending candidate
  would quarantine every legitimate expansion, because the remote post-change
  set necessarily differs from the current trusted set. Conversely, comparing
  only with the pending post-change set would quarantine the unchanged
  installation before GitHub applies the expansion. Pending matching now takes
  precedence and accepts either bounded state while publishing only the current
  trusted subset into the mint allow-set.
- **Confirmed:** using the parent context for revocation loses the cleanup call
  on cancellation. Revocation now uses a cancellation-detached ten-second
  context and remains part of the pass verdict.
- **Confirmed:** a token can be present in a syntactically decoded response whose
  permissions or selection are invalid. The janitor retains that value only
  long enough to revoke it; errors and records never render it.
- **Confirmed:** page length alone cannot prove completeness. Enumeration binds
  every page to one bounded `total_count`, rejects a short page before that
  total, and rejects duplicate, malformed, excessive, or count-changing results.
- **Confirmed:** exact pending coverage at registration scope could still let a
  private resolver select the installation. Minting now requires the exact
  trusted repository tuple in addition to active registration coverage.
- **Confirmed:** a known installation ID returned under different account
  coordinates must not fall through as an ordinary unknown-owner deletion.
  It enters atomic terminal quarantine as identity drift.
- **Rejected by verification:** unknown-owner cleanup did not need a grant token
  or a wider removal budget. Full installation pagination still completes
  before deletion, every removal crosses the audit barrier, and the existing
  per-cycle bound still applies across ordinary removals and quarantines.

## Revisit When

Revisit only the persistence representation when the onboarding flow implements
creation, promotion, expiry, and portable-frontier publication. It must preserve
this source snapshot and atomic quarantine behavior; a different representation
is acceptable if it proves the same epoch, revision, exact-set, and invalidation
contract.
