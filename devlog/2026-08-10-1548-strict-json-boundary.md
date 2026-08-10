# Centralize Strict JSON Boundaries

Chose one standard-library-only `strictjson` leaf over per-package wrappers because the security contract is the same at every typed, single-value JSON boundary: an explicit UTF-8 posture, an explicit inclusive byte limit, unknown-field policy, and exactly one value. The posture parameters have invalid zero values, and `NoLimit` is an explicit sentinel, so adding a call requires a visible choice rather than inheriting a default.

Kept a separate allowing-unknown-fields entry point for forge responses because GitHub may add fields without changing the response meaning this daemon consumes. Reader variants buffer through a bounded reader because UTF-8 validation and exact single-value delimiting require the complete accepted bytes; over-limit input is rejected at `limit+1`, never truncated.

Made size the first input-content gate, as the issue's implementation plan specifies. Consequently an over-limit reader returns the size error even when its prefix is already malformed JSON; the HTTP boundaries continue to classify every over-cap request as 413 rather than exposing how much of the prefix parsed. Posture arguments are validated before any size check or reader I/O, so an invalid policy or limit remains a programmer error and cannot block on or allocate from the input.

Preserved all 32 strict decode sites across the issue's 25-file inventory plus the permissive forge site. The audit's file count was current, but its site count was not: seven files contain multiple strict boundaries. Existing UTF-8 and size postures remain site-specific, and package-level error categories stay intact where callers distinguish malformed, trailing, over-limit, or invalid-UTF-8 input.

The refute-first pass found four rejecting boundaries whose raw UTF-8 check had originally preceded structural or preallocation gates. Those checks remain in place before the package-specific gates, with `strictjson` independently enforcing the same posture at the typed decode, so compound-invalid payloads keep their established UTF-8 classification. It also found that a syntax-shape ratchet could miss an extracted trailing-decode helper; the ratchet now rejects every non-exempt `.Decode` method call in files importing `encoding/json`, with the sole non-JSON method exemption documented by receiver name.

Kept structural token walkers outside the helper because they enforce different contracts that a typed decode cannot express: case-folded duplicate-key rejection, authored-snapshot duplicate-key rejection, commit-plan preallocation bounds, and evidence-manifest entry preallocation bounds. A source ratchet permits only those named walkers and rejects new open-coded strict decoders, trailing decodes, or unregistered token walkers outside `strictjson`.

Rejected a universal invalid-UTF-8 gate because several durable daemon-authored formats deliberately retain `encoding/json`'s current tolerant behavior, and widening their rejection set is outside this posture-preserving unit. Rejected universal duplicate-key rejection for the same reason; the helper pins last-value-wins behavior while stronger boundaries keep their existing structural or canonical-byte gates.

The refute pass also confirmed the inclusive limits, the forge-only permissive unknown-field posture, the retained duplicate/canonical gates, and the separation of token walkers. No other error-priority or trust-boundary change was accepted.

Revisit when a boundary needs streaming decode without whole-input buffering, multiple top-level values, or a third unknown-field posture; those are different contracts and should not be hidden behind optional defaults.
