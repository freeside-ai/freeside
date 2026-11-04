# Assurance Tiers

Work unit: #2519, under tracker #2526. Mandatory note: it changes the
project's safety policy (which work gets refute-first, a fresh-context
review, and a mandatory note). The rules themselves live in AGENTS.md
(Assurance Tiers); this note records why.

## Decision: Tier the Bar by Authority and Input Trust

Chose three tiers, classified by path under one stated threat model, over
one assurance bar for every boundary:

- **Tier A, authority and untrusted input**, keeps today's bar:
  refute-first, a fresh-context review, a mandatory note, a re-check of
  stored authority against current policy on read, and tamper tests where
  the input is untrusted.
- **Tier B, ordinary state**, gets ordinary engineering: validate on
  write, test the behavior, decode rows with plain validation, and fail
  loudly on a bad row.
- **Tier C, advisory**, is best effort: skip and log a bad row rather than
  fail a whole listing.

The threat model trusts the daemon's own database, artifact store, and
binary, because anything that can write them can replace the daemon. It
distrusts agent output and workspaces, GitHub and provider API responses,
client requests, CI results, research fetches, and repository-supplied
config.

## Evidence

- **The bar had spread to ordinary state.** The refute-first trigger names
  fields returned by "an external call or deserializer". Read literally,
  every decode of the daemon's own SQLite rows is a returned-object trust
  boundary. 114 of the 225 notes dated 2026-10-03 to 2026-11-03 mention a
  refute-first pass, so about half of all recent work paid for refute-first,
  a note, and a fresh review.
- **The notes disagreed on the threat model.**
  `2026-10-03-1444-credential-integrity-mark.md` puts adversarial edits to
  the daemon's own database outside the boundary, while
  `2026-10-05-0921-follow-up-filing-kind.md` keeps a listing that refuses a
  tampered row on purpose. With no stated model, each unit chose its own,
  and the stricter reading won by default.

## Rejected Alternatives

- **Keep the uniform bar.** It spends the costliest process on rows the
  daemon wrote itself, where a bad row is a bug, not an attack. Review
  bandwidth is the project's constraint (Wave 6 tracker #835), so the
  spending is not free.
- **Classify each unit by judgment.** No reviewer can check a judgment
  call after the fact, and the safe-looking answer is always "boundary",
  so the practice would drift back to the uniform bar. A path list can be
  checked against a diff.

The path list alone would leave a hole: a credential or authority file it
misses would quietly get the Tier B bar. So the Tier A categories also
classify, and a unit that finds such a file adds it to the list in the
same PR. Judgment then only grows the list, and the list stays checkable.
A fresh-context review of this change found seventeen such files the
first draft missed (agent admission, the bearer check's HTTP entry,
image-policy adoption, publication authoring), which is the risk the rule
answers.

## Classification Rules Planning Added

- **Untrusted input that grants nothing is not Tier A.** Untrusted input
  is Tier A where it can grant authority, reach a credential, or change
  what gets published, imported, or verified. Input that only feeds
  scheduling or display (a provider usage reading, for example) is
  validated where it enters, under Tier B or C.
- **Client authority is Tier A.** Pairing, enrollment, and the
  paired-device bearer check grant a client authority over the daemon, so
  they sit with approvals and decision binding. Operator commands that
  arrive through that check stay Tier B.

## Ratchet, Not Sweep

Existing re-gates, cross-checks, and tamper tests stay in the code. New
Tier B code uses the lighter bar; an existing check goes only when a unit
touches its file anyway or a cleanup unit removes it. The re-gate of a
stored authority bit stays everywhere: it guards against a bit that was
valid when written but is stale under current policy, which the trusted
database does not rule out.

Upstreaming the tiers into the agent-setup skill's High-assurance profile
is deferred until they have run for a while. Until then the AGENTS.md
section narrows the managed text's "returned-object trust boundary" to
Tier A, so a sync of the managed blocks leaves the rule intact.

## Revisit When

After a wave runs under the tiers, check two things: whether a Tier A
defect escaped through a path missing from the list, and whether Tier B
units still write refute-first notes. The first means the path list needs
an entry or a broader rule; the second means the section is not being
read.
