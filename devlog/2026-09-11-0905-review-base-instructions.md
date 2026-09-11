# Materialize The Trusted Base For Review Instructions

The publication transport deliberately fetches Git objects into an empty
working tree so the importer can reconstruct the candidate. Filesystem
instruction discovery on that tree silently omitted repository instructions.
The stopped Wave 7 campaign demonstrated this with an AGENTS.md present in the
pinned base and an empty repository_sources binding.

Use the existing sealed transport capability to materialize a separate scratch
tree at that same base commit. Compose instructions there before importing the
candidate. This preserves the importer precondition and prevents candidate
instruction edits from becoming reviewer authority. It needs neither another
fetch nor a new transport interface. A materialization error stops composition.

The regression exercises an empty import tree, a real base instruction file
and a conflicting candidate instruction edit through production publication.
Fixtures establish this source behavior; they do not complete live acceptance.

Independent refutation found no actionable defect. The concern that this adds
a broader base-tree rejection was disproved: production execution already
materializes the base with the same transport, and the importer rejects removal
of unsupported base symlinks. The sealed capability and fresh sibling preserve
the original import tree.

Automated review identified a separate reachable refusal: retaining the fetched
object database has a byte limit that execution's base-tree materialization
does not apply. A deterministic retention refusal now uses the existing durable
task hold, so one repository cannot stop the publication lane. The hold survives
restart and retries after repair; review and publication remain blocked while
the refusal persists. Other materialization errors keep their existing failure
behavior. This uses the publication hold rather than inventing a review round
before instruction composition has established its binding.

The same task-scoped hold covers deterministic instruction-budget refusal,
including a raw source set that fits discovery but exceeds the composed bundle
limit after framing. Filesystem and artifact-store failures remain hard errors;
the pure composition boundary and aggregate-size check identify refusals without
changing the execution interface or either limit.

Revisit when instruction discovery can read Git objects directly through an
existing supported boundary, without adding a second transport contract.
