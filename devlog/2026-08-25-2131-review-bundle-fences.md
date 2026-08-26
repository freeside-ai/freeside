# Fence Codex Review Instruction Bodies

Chose `codex_explicit_bundle_v2`, with each raw instruction body inside a
backtick fence one character longer than its longest backtick run, because it
keeps every later repository scope and the operator-host boundary outside any
Markdown construct opened by an earlier trusted source. Source digests still
bind the raw bytes; the result digest binds the new deterministic bundle.

Persisted v1 bindings remain structurally valid for decoding, backup closure,
and supersession comparison, but ward refuses to reconstruct them for launch.
The engine's ordinary fresh composition emits v2, so a pending same-authority
v1 request is torn down through the existing supersession path and retried
with the current binding.

Rejected content escaping because it would introduce an encoded instruction
dialect, and rejected synthesizing guessed closing constructs because
Markdown constructs are not limited to fences. Rejected a hard-cut migration
that invalidated v1 rows during decoding because it would turn a safely
supersedable pending request into a persisted contradiction and would omit its
artifacts from backup closure.

The sibling Claude execution bundle has the same raw-concatenation flaw but a
separate persisted contract, so it remains a follow-up rather than sharing
this version transition.

Follow-up: #946

Revisit when the bundle format stops being Markdown or a launch consumer needs
to execute historical composition versions rather than supersede them.
