# The Adoption Store Re-Gates Every Adoption and Its Approve

Mandatory note: a contract change to a returned-object trust boundary
(#2379). The plan and the planning choices are in
`devlog/2026-10-27-1236-adoption-card-daemon-policy.md` and the issue body;
this note records what the implementation added to them and the refute-first
findings.

## Decisions

- **Chose to re-gate every stored adoption on read over trusting the row.**
  An adoption widens the approved-recipe set, which is a trust bit, so each
  read reruns the card's gate, requires its approve decision, and checks the
  record against the card's parameters and its project's repository. The
  result is memoized per transaction: every row it reads is immutable and
  only the adoption write changes the answer, and that write clears the memo.
  One adoption that fails its gate fails every evidence read in the store.
  That is the intended fail-closed direction, and no update or delete path
  exists for the rows the gate reads, so a legitimately recorded adoption
  cannot go stale.
- **Chose a dedicated constructor (`NewImagePolicyAdoptionProposal`) over
  extending `NewEffectProposal`.** The shared constructor takes a resolved
  policy, and this kind binds none. It now refuses the kind, so no caller can
  bind a policy to a card by accident.
- **Chose to tie the admission key to the card.** An adoption card is
  admitted only under the `upkeep_refusal` key naming the refusal it answers,
  and that source admits no other kind. Without it the same card could be
  admitted once per arbitrary key, which breaks "one refusal admits one card".
- **Chose to require the approve to come through the card's own item.** A
  binding does not check its item against its card, so the adoption write
  and read check that the deciding command's item is in the card's project
  and batch, renders the card's digest, and selected that digest. The item
  is read at the record tier because the evidence gate reads adoptions, and
  the item read would otherwise recurse.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong.

- **Confirmed and fixed:** the admission key was not tied to the card's
  refusal or kind; an approve on a mis-bound item recorded an adoption.
- **Disproved:** "an adoption flips the stored eligibility bit of older
  ineligible artifacts under the adopted digest and breaks the backup
  closure." Such an artifact needs verification under a recipe no policy
  approves, and the engine verifies only under an approved recipe
  (`production_reentry.go`, `production_publication.go`). Approving the same
  digest through the operator flag has the same effect today.
- **Allowed by decision:** a store-wide outage from one bad adoption row
  (fail closed, above); no read-time check of the admission source, since
  the stored key is uniqueness, not authority.

Revisit when the engine gains a path that verifies under an unapproved
recipe, or when adoptions gain a withdraw path (#2335).
