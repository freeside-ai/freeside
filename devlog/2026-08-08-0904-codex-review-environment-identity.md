# Canonicalize Codex Review Environment Identity

Chose a sorted exact-entry digest for the Codex review launcher environment
over ordered hashing or key-based normalization. Apple container can reorder
the same entries after start, while sorting preserves multiset identity: an
added, removed, duplicated, or value-changed entry still changes the digest.
Command and command-template digests remain ordered because argument order is
semantically significant.

Chose to require exactly one runtime-injected fixed PATH entry before removing
it from the realized environment. The implementation plan expected an absent
PATH to fail through digest mismatch if the unstripped list were hashed, but
the absent-PATH list is exactly the launcher environment and would therefore
authenticate. Explicit cardinality closes that gap; a missing or duplicated
fixed PATH fails before the canonical digest comparison.

Bindings written by a pre-fix daemon use the old ordered digest and fail
post-start authentication under a post-fix daemon. Codex review bindings live
for one run, and no production review is in flight past `started`, so a
migration would add durable compatibility authority without preserving useful
work. The incompatible ephemeral binding is accepted as the safer outcome.

Deterministic fake-runtime coverage authenticates both the pre-start report
shape and a post-start permutation with PATH in the middle, then rejects added,
removed, duplicated, changed, missing-PATH, and duplicated-PATH reports. The
Apple-container lifecycle proof now uses the ordinary abort path and retains
its zero-residue assertions.

The refute-first pass confirmed the implementation-plan gap for an absent
PATH and fixed it with explicit cardinality. It rejected by execution the
concerns that canonicalization might admit duplicate entries or that moving
PATH away from the first position might strip the wrong entry: the adversarial
fake-runtime cases fail closed and the reordered live report authenticates.
The pre-fix binding incompatibility is accepted by decision for the single-run
lifetime described above.

Revisit when the runtime exposes a stable structured environment identity, or
when review bindings outlive a single run and require a versioned migration.

Follow-up: #606
