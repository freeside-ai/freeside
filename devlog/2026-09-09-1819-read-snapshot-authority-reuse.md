# Reuse Authority Within One Read Snapshot

Chose to memoize successful initial-attempt and publication-successor authority
checks inside `Store.Read`. Feedback and publication reconstruction
revisit the same initial approval many times in one sync request. Revalidating it
against the same SQLite snapshot adds cost without adding evidence. Both caches
materially help: with representative retained history, successor caching alone took
about 800 ms per sync read; adding initial authority reuse reduced it to about
190 ms. Initial authority reuse alone still took about four seconds per read.

The cache key contains every attempt coordinate consumed by this predicate:
campaign, specification run, implementation run, source digest, publication digest,
and approved specification digest. Attempt number is always one before lookup.
The remaining evidence comes from the read transaction. Failures are not retained,
and a hit still checks context cancellation.

Successor proofs are keyed by the entire active publication ancestry, including
ready-resource links. Length-prefixed keys prevent separator collisions. The cycle
gate runs before lookup; a proof under one ancestry cannot suppress an error under
another. The returned successor has only scalar fields, so copying it does not
share mutable state with callers. A canceled hit returns the same zero value and
error as the original first-query failure.

Only `Store.Read` initializes the cache. `Write`, `WriteInternal`, and their shared
read helpers must observe their own mutations; they never reuse this proof. No
cache survives its transaction or shares a mutable returned object. A new request
reconstructs current authority normally.

Independent refutation found no reachable omitted-key, policy, publication-context,
or write-visibility bypass. Generated comparisons against the predicate reconstructed
from the base agreed for all 128 combinations of changed authority coordinates,
covering source-only and completed specification authority. Regression checks also
reject forged inputs, honor cancellation, and observe changed authority both within
write transactions and after a new read begins. Successful human approval and
publication-history validation remain covered by the existing engine and integration
fixtures. Comparison with the original successor reader agreed on values and errors
across 198 generated ancestry cases, each repeated through the cached route.

Revisit when either predicate gains another caller-supplied coordinate, depends on
mutable nontransactional policy, or read transactions support concurrent use. Such a
change must reconsider the key and cache lifetime before reusing this proof.

Follow-up: [#1270](https://github.com/freeside-ai/freeside/issues/1270)
