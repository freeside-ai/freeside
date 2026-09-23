# Shared-App Janitor Interference

Work unit #1511. This finding revisits the prod-App exception in
`devlog/2026-09-23-0939-environment-tiers.md` (plan revision 69).

## Confirmed the Shared Authority Risk

The earlier decision allowed an attended `ephemeral` real-work daemon to
enroll the `prod` GitHub App while leaving cross-daemon GitHub reconciliation
unverified. Separate state roots and operator attendance do not prevent
installation interference. Each daemon reads its own
trusted installation bindings, but its janitor lists every installation of
the App. An installation absent from one daemon's trusted bindings and active
pending authority is deleted; a repository-grant set matching neither that
daemon's trusted binding nor its pending authority is suspended and deleted.
The other daemon may have correctly bound and still need that installation.
These are App-wide effects, even when the two daemons have
separate SQLite stores and work on the same repository.

The real-work harness starts the Claude daemon with publication credentials,
and that composition runs a janitor pass at startup and schedules later
passes. Its grant-read token is revoked by that same janitor after use; the
revocation does not target another daemon's token. Publication PR lookup,
active-resource reconciliation, label intake, and native review observation
do not match objects by the publishing App's identity. A separate concern
remains: an identical content-derived publication identity on the same target
repository can make two daemons converge one PR, regardless of which App
authored it. Different markers on one declared branch fail closed.

## Remedy to Decide

A distinct App for attended real-work instances separates the janitors'
installation sets. Target-repository separation alone cannot do that,
because the janitor enumerates all installations of its App. A GitHub-side
lease alone cannot make divergent local bindings safe when the later daemon
runs. If two instances can publish to one repository, their deterministic
publication identities also need a target or work-unit coordination rule.
The owner chooses the plan remedy in #1517; this note records the finding,
not that choice.

## Revisit When

Recheck this finding if the janitor stops reconciling App-wide installations
from local bindings, or if publication identity gains an instance boundary.
The planned App-authored comment and follow-up issue recovery paths need
their own cross-instance review when implemented.
