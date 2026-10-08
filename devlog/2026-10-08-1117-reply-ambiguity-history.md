# Retain Reply Ambiguity Across Later Sends

Chose to disable listing-based recovery after an ambiguous reply in the same
comment collection because the earlier request may still commit. Serializing
outstanding intents, the assumption behind #1636 decision 7, does not isolate
a later intent after the earlier one becomes terminal. A late comment can
satisfy the later intent's App identity, timestamp, and pre-dispatch ID checks
without belonging to its request. This fixes #1832 using retained records;
it needs no migration or new payload field.

The original reasoning is in
[External Review Replies](2026-10-06-2204-external-review-replies.md).
[Wave 8 Exit Audit](2026-10-07-1159-wave-eight-exit-audit.md) supplies the
evidence that invalidated its recovery assumption.

## Collection and Proof

Authenticate each retained ambiguous outcome's intent against its own pinned
publication. Compare numeric repository ID, pull request number, and inline
root, with root zero representing the conversation. Review-body IDs do not
separate conversation comments. Repository spelling does not separate a
renamed repository. Outstanding intents serialize by numeric repository ID
and pull request across every collection, preserving the existing scope of
that gate. Terminal ambiguity restricts only adoption within its collection.

Later sends remain available. A validated response to their create request
still proves their own success. An unknown response ends ambiguous after the
existing settle interval rather than adopting a comment the earlier request
could explain. This sacrifices some successful recovery to preserve honest
outcomes. An unreadable or invalid history stops recovery with an error;
missing evidence cannot prove that the collection is safe.

Rejected shorter settle windows, comment text matching, observing the earlier
comment, and notice acknowledgement as clearance rules. None proves that the
earlier effect can no longer commit. An App account change also does not clear
the retained restriction. History is read without rewriting earlier outcomes.

## Refute-First Evidence

- Confirmed: the old code adopted delayed comment 777 as the later reply for
  different review-body IDs and for two findings in one inline root, both
  with and without store reopen. The regression requires ambiguous outcomes
  with no adopted comment and exactly one send per intent.
- Disproved by regression checks: reopening the store, a prior run, a former
  repository name, or a different App account can bypass retained ambiguity.
  A same-ID rename also cannot open an overlapping pending intent.
- Disproved by collection checks: restricting one inline root disables
  another root or the conversation. Other repositories and pull requests
  retain recovery, as do collections with only successful or refused history.
- Disproved by damaged-history checks: a missing intent or binding, mismatched
  publication coordinates, invalid thread, kind or key, malformed outcome,
  or unreadable history can authorize adoption.
- Disproved through the production pairing and command path: acknowledging
  the first ambiguous notice allows adoption after reopening the store.
- Independent review found no reachable material defect. It considered an
  intent dispatched without an outcome, but completion records the outcome
  and dispatches the intent atomically. No production replier path deletes
  the outcome or dispatches the intent separately.

## Revisit When

The forge supplies authenticated per-intent comment identity, or a designed
human settlement action proves that the earlier effect cannot contaminate
later recovery. Reassess retained scans if reply volume makes them costly;
current volume does not justify a new persistence contract or index.
