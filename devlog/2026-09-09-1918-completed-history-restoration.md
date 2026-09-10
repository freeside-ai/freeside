# Restore Completed History Without Reopening Work

Chose a distinct completed-history checkpoint over requiring an open pull
request during every retained-session restoration. Once a work unit completes,
that requirement prevents a supported runtime upgrade from restoring access to
its history. The owner authorized this narrow repair in #1272.

The checkpoint reuses the store's completion gate, which reconstructs authority
from the declaration, effective publication binding, and recorded merge and issue
fact timelines. It independently compares that binding with the run's published
ready resource. An internally consistent completion for another pull request
must not authenticate this run. The remote check then requires the same pull
request, repository, branch, base, accepted head, and recorded merge commit,
with the pull request closed and merged.

A later issue reopen does not erase the earlier facts that established
completion. Requiring the issue to remain closed would change the existing
historical completion contract. Completed history therefore does not depend on
the current ready-card status either. Incomplete retained history keeps its
existing restrictions, including refusal of stopped cards.

Restoration grants access to history, not execution authority. Ordinary
publication verification and the incomplete-publication action gate still
reject completed work. No schema, database row, pairing, approval, or runtime
lease change is part of this repair.

Original and successor publication fixtures cover the accepted path. Refutation
cases cover unsupported completion facts, a foreign published binding, and
remote state, coordinate, head, or merge mismatches. A read-only check against
the retained completed run also authenticated its actual accepted successor and
remote merge. That proves the verifier can read existing evidence; it does not
establish installation or client acceptance of a replacement runtime.

Revisit when completion semantics change, publication can move across
repositories, or retained restoration gains any authority to execute work.
