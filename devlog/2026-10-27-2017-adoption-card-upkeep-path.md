# The Adoption Card Is Raised by Upkeep and Builds Under the Adopted Policy

Mandatory note: a trust-boundary and safety-policy change (#2335). The
policy choice is in `devlog/2026-10-27-1236-adoption-card-daemon-policy.md`,
and the store's gates are in
`devlog/2026-10-27-1348-adoption-store-trust-gates.md`. This note records
what wiring the card end to end added to them.

## Decisions

- **Chose the card's proposal batch as its subject over its project.** The
  plan named the project, but the store's adoption write requires the
  deciding item to sit on the card's batch. The batch ID is derived from the
  refusal ID, so the upkeep job finds a refusal's card without a new store
  query.
- **Chose a configured project list over a new store query.** A card needs a
  project in the refused repository, and no store read lists projects by
  repository. Upkeep takes the daemon's intake and manual projects, and the
  first one registered for the repository takes the card. Adding the query
  would be a contract change, outside this unit.
- **Chose to refuse a stale approve at the decision, not only to withdraw
  the card.** Approving re-checks, in the accepting transaction, that the
  refusal was measured from the admitted image at the observed tip. A stale
  approve rolls back and records nothing. Upkeep also withdraws open cards a
  settled pass has moved past, so the queue stays clear, but the decision
  gate is what makes a stale approve harmless.
- **Chose to read the repository's effective recipe set in the publication
  lane and in borrowed commands.** An adopted recipe produces evidence under
  its digest. A lane or command that kept the operator's set would refuse
  that evidence, and held work would never release. Sites that build items
  carrying no recipe evidence keep the operator's set. The daemon serves one
  repository, so its ID comes from configuration. A daemon with no Claude
  driver has no repository and approves only the operator's set.
- **Chose a fresh image at the tip after a recipe adoption over a delta
  rebuild.** The rebuild gate's unchanged-recipe clause stays as is. When the
  only finding is a changed recipe and the tip's recipe digest is adopted,
  upkeep builds from the tip's own recipe, reread and digest-checked at the
  tip, and gates the result against the adopted digest. A host adoption needs
  no new path: the gate reads the effective registry set, so the next pass
  admits the delta as an ordinary rebuild.
- **Chose to treat an approved card as pending until upkeep settles past its
  refusal.** Between the approve and the build, held work waits, not blocks.
  A declined or withdrawn card resolves nothing, so held work then takes the
  blocked outcome.

## Refute-First Findings

A fresh-context reviewer tried to prove the branch wrong.

- **Confirmed and fixed:** the schedule's backstop pass reused the policy
  refusal an approved card answers. After an adopted build that host-faulted,
  no later scheduled pass built again; after one that failed its proof, held
  work still showed the approved card instead of the repair. A refusal an
  adoption answers is no longer a settled verdict, so the tip is measured
  again.
- **Confirmed and fixed:** facts the domain rejects (no recipe at the tip, a
  lockfile host with a port, too many hosts) failed every pass instead of
  raising no card. They now raise none and leave the policy refusal settled.
- **Checked and held:** no path applies an adoption outside the card's
  repository or without an approve through the card's own item; the adopted
  build reads its recipe from the tip's immutable tree and the result is
  re-gated against the adopted digest.
- **Allowed by decision:** the stale gate compares the refusal's tip with the
  repository's tip observation. While the observation lags a forge read, a
  current card's approve is refused; the card stays open and a later approve
  succeeds, so the gate fails closed. With no observation the gate checks the
  admitted image only, and the operator approves the exact hosts the card
  shows and the digest it shortens, which Details shows in full.

## Not Done Here

- **Writer egress for adopted hosts.** The writer's network allowance still
  reads the declared policy only, so a run in an image built under an
  adopted host can't reach that host. Follow-up: #2406.
- **A repository on the card.** The adoption facts name no repository, so
  the app card's sentence says "the repository's image policy", and the
  item's project names the repository. Adding it is an API change.
  Follow-up: #2407.
