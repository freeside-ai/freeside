# Codex Review Resource Name Compatibility

Issue #587 changes a destructive Ward recovery boundary and therefore requires
a durable record of how existing launch intents remain safe across the naming
upgrade.

## Decision

Chose compact, role-specific suffixes that retain the complete admitted review
invocation ID over truncation or hashing because the resulting names fit Apple
container's 64-byte limit while preserving direct, collision-free derivation
from invocation and role. The Apple limit is also enforced at every runtime
create boundary, so a future naming regression fails locally instead of
deterministically churning runtime retries.

Chose exact-generation legacy compatibility over accepting legacy names
individually. Recovery accepts either the complete current resource set or the
complete pre-#587 set, re-derived from the caller's invocation ID. It still
requires the durable unpredictable owner and live ownership evidence before
deletion. A mixed, foreign, or ambiguous topology remains open and fails
closed. New launches emit only current names.

Rejected shortening durable invocation IDs because they are workflow identity,
not a runtime implementation detail. Rejected deleting or rewriting legacy
intent rows because the pre-create journal is the authority that makes an
ambiguous runtime survivor safe to authenticate and reap.

## Refute-First Findings

- **Confirmed and fixed:** simply renaming the workspace observer made a
  pre-upgrade `preparing` intent fail identity validation forever. Recovery now
  re-derives the exact legacy generation and uses its authenticated container
  targets during cleanup before opening a current intent.
- **Confirmed and fixed:** accepting each old name independently allowed a
  rewritten row to compose current and legacy teardown targets. Validation now
  admits only one complete generation.
- **Rejected by verification:** a same-name legacy observer with a foreign
  owner is not deleted and does not close the intent.
- **Rejected by verification:** maximum-length invocation IDs do not collide
  across roles or invocations, and the complete current topology reaches the
  review container start under a runtime boundary enforcing 64 bytes.
- **Rejected by verification:** restart recovery does not rewrite the persisted
  review request; it closes the authenticated legacy intent, removes its
  partial topology, and relaunches the same invocation with current names.

Revisit when Apple container changes its identifier contract or when the last
pre-#587 launch intent can be proven absent from every supported state store.
