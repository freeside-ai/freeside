# Open-source Freeside under AGPL-3.0-or-later

Chose AGPL-3.0-or-later for the entire monorepo over MPL-2.0 and
GPL-3.0-or-later (owner decision). Freeside's dominant architecture remains a
network-accessed daemon, and the product is a standalone control plane rather
than an embeddable library. Network reciprocity therefore protects changes to
Freeside's source at a smaller adoption cost than it would impose on a library.

The owner accepts that some organizations ban or specially review AGPL
software. MPL-2.0 would reduce that friction but allow proprietary service
forks; GPL-3.0-or-later would preserve strong distribution copyleft but leave
the network-use gap that matters to the daemon. LGPL-3.0-or-later and CC BY-SA
4.0 do not match the repository's primary software form.

The changed assumption is timing, not architecture: exhausted private-repo
Actions capacity makes public-repository CI immediately useful, so licensing
and visibility move forward from Phase 4 while Phase 4 product features stay
put. Repository visibility follows the license landing so the public history
does not begin with an unlicensed default branch.

The owner applies AGPL-3.0-or-later to every historical revision for which he
holds copyright, except material that states a different license or copyright
holder. Chose an explicit retrospective grant over rewriting or discarding the
commit graph; the authorship and notice sweep found no conflicting repository
material at this decision point.

The originating 2026-07-08 decision entry remains frozen under the current
decision-note protocol. Issue #18 and ADR 0001 carry the live linkage instead
of appending a legacy queue marker to that historical note.

Revisit when observed target adopters are unable to deploy Freeside because
of AGPL policy, when App Store distribution is being prepared and its
then-current terms need a compatibility decision, or when a concrete
distribution model makes dual licensing materially valuable.

Follow-up: #101 carries the repository-publication state change.
