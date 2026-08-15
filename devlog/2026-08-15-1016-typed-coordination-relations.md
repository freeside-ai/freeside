# Typed Coordination Relationships

Work unit: #791. This note is mandatory because the change revises the
project's coordination contract.

## Decision

Chose four issue-authoritative relationship types over an untyped Dependencies
field because start order, integration order, intentional branch ancestry, and
mutual exclusion have different operational checks:

- `starts-after` requires the prerequisite PR to merge before the dependent
  unit starts;
- `merges-after` permits independent starts but orders integration;
- `stacked-on` records intentional ancestry on an open PR branch; and
- `exclusive-with` forbids concurrent activity without inventing an order.

Unknown or materially ambiguous relationships fail closed as `starts-after`
until the spine resolves them. Trackers expose **Startable now** and
**Mergeable next** as separate derived projections, while the unit issue stays
authoritative. The startable projection contains stable relationship state,
not volatile claim or exclusivity occupancy; sessions query those live. A
one-sided `exclusive-with` declaration has symmetric effect because Session
Start searches open unit issues for reverse declarations. Simultaneous
conflicting claims converge through a post-write recheck ordered by the
forge-issued comment time and numeric ID; a new declaration mid-claim remains
a serialized relationship edit that cannot land while both endpoints have
active claims. The editor waits for one verified release before changing the
relationship. A stack is satisfied while its base is open and usable or after
that base merges; an existing child must then be retargeted to the default
branch before it is mergeable. Tracker projections refresh on relationship
edits, relevant PR lifecycle changes, and merges. Contract units retain their
existing spine-owned,
merge-before-dependent policy as a standing `exclusive-with` regime.

## Rejected Alternatives

- **Keep untyped prose.** It leaves readers to guess whether "depends on"
  blocks start, only orders merge, declares a stack, or merely prevents
  overlap.
- **Make trackers authoritative.** A derived digest would compete with the
  issue contract and drift whenever only one surface changed.
- **Add a local status database.** GitHub issues, claims, PRs, and git already
  hold the durable facts; another store would add reconciliation without new
  authority.
- **Add four structured issue-template fields.** Most units would gain four
  required fields containing "none". One typed Dependencies field keeps the
  contract compact while making every non-empty relationship explicit.
- **Require mirrored exclusivity declarations.** Two issue-body writes cannot
  be atomic, so a partial update would make the safety rule depend on which
  unit starts. A reverse lookup gives either declaration symmetric effect.
- **Use a pre-claim exclusivity snapshot.** Two sessions can both pass a
  snapshot before either claim exists. Re-arbitrating fresh candidates across
  every directly related issue after posting closes that race.
- **Add a bespoke relationship lock or script.** The forge-issued claim order
  already provides deterministic arbitration. A second lock would duplicate
  future Freeside control-plane enforcement while adding lock-location,
  liveness, and fail-closed failure modes.

Revisit when repeated real units expose a relationship that these four cannot
represent without ambiguity, or when machine validation of typed entries is
valuable enough to justify structured template fields.
