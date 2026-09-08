# Declare Publication Branches Without Renaming Identity

Work unit: #1216. The issue's owner-approved policy lets the operator provide
an exact head branch in `publication.json`. An absent field keeps
`freeside/publish/<identity-hex16>`. Repository files never supply naming
authority, and using the default never implies an exception to repository
conventions.

## Stable Identity, Frozen Branch

The `freeside-publication/v1` identity encoding stays unchanged. The branch,
like the title and body, is excluded. A version 3 intent freezes the resolved
branch before dispatch. Versions 1 and 2 continue to mean the identity-derived
branch; their persisted bytes and published refs stay unchanged. The outcome,
recovery resolver, live PR checks, and ready-item re-gate must agree with that
binding. Adding a branch to submission metadata changes its digest and therefore
the default run ID, as other publication metadata changes already do.

Chose exact operator names over automatic digest suffixes because a suffix
can violate the repository's slug convention. Rejected reading AGENTS.md
because managed-repository prose is untrusted. Rejected an identity encoding
bump because an upgrade retry could otherwise mint another PR; the reasoning
in `2026-07-16-1622-publication-identities.md` still holds. Renaming existing
branches is outside this policy.

This changes operator naming authority and durable retry policy, so plan
revision 49 records it as a material contract change. The original
implementation plan's wording-only classification was incorrect; revision 48
is archived without changing its decisions.

## Necessary Durable-Store Changes

Migration 0039 restricted outbox versions and new-publication triggers to
format 2. Migration 0068 rebuilds that table for format 3 while copying every
old row unchanged. It reinstates current-format insertion and promotion guards
and the no-downgrade guard; restore reinstates the same canonical insertion
guard. A payload's format still must equal the store-owned version marker.

Invocation-scoped intent keys alone cannot enforce one branch per identity.
A new invocation can arrive before the prior attempt has recorded an outcome.
Both store-backed publication paths therefore compare the proposed binding
with committed publication intents in their existing write transaction.
Pending, dispatched, and decodable quarantined intents retain their bindings.
Undecodable quarantined rows cannot dispatch or supply authenticated identity.
No additional identity table is needed for the current local workload.

## Refute-First Findings

- Confirmed and fixed: admitted URL-significant branch names could target
  another ref or evade PR rediscovery. Forge lookups and live-test cleanup
  now escape path and query components. A publication/retry regression
  reproduced failures for `#`, `&`, `+`, `%`, and literal percent escapes.
- Confirmed and fixed: a v3 payload paired with the old version stamp would
  fail reconstruction. The migration, writer, decoder, and restore guard move
  together. Migration tests compare the complete v2 row before and after.
- Confirmed and fixed: a new invocation could rename the same identity before
  an outcome existed. The transaction-level comparison rejects that attempt
  before a forge effect; restart tests cover both sides of the first effect.
- Disproved by checks: moving the grammar changed accepted names. The old and
  new implementations returned identical decisions for 100,000 generated
  refnames, and the existing git differential corpus remains in place.
- Disproved by tests: a declared branch can bypass the reserved-prefix or base
  check, a foreign SHA or PR marker can be adopted, or an outcome's branch can
  disagree with its candidate or ready-item intent. Each path fails closed.
- Checked consumers: transport uses the sealed capability's branch;
  reconciliation takes the supplied ref; recovery and engine result assembly
  use the recorded outcome branch. Installation janitor work derives no
  publication branch. The head-branch choice never changes `BaseRef`.
- Independent review found no actionable defect after checking transaction
  coverage, legacy formats, migration preservation, restore guards, forge
  coordinates, and targeted reconstruction tests.

Follow-up: #1224 shares the duplicate fake-publication refname grammar.

Revisit when the Task namer supplies suggested slugs, or publication history
becomes large enough that scanning committed intents needs an identity index.
