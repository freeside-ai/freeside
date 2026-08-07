# Make Terminal Eviction Win In-Flight Cache Fills

Chose a reconciler-wide cache epoch over holding the mutex across network I/O
or retaining per-key generations. A fresh-context refutation found that a
request started before terminal eviction could otherwise complete afterward
and refill the entry that eviction had just removed. Capturing the epoch before
I/O and accepting a cache write only when it is unchanged makes eviction win
that race while preserving the reconciler's deliberate unlocked network calls.
The global epoch may discard an unrelated in-flight cache fill, but cache state
is only a bandwidth optimization and the next poll safely refetches it.

Also chose to re-check terminal state immediately after a successful
active-resource commit. An item can conclude inside that commit, so waiting for
the next 15-minute enumeration pass would leave its cache entries resident
after the lifecycle transition.

Rejected holding the cache mutex across network I/O because the reconciler's
documented concurrency design forbids that coupling. Rejected per-key epochs
because they would retain another permanent key per concluded resource, the
same lifetime shape this work removes.

Revisit when cache writes need key-local ordering for correctness rather than
request-rate optimization.
