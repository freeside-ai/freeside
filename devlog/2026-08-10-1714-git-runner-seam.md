# Shared Hardened Git Runner Seam (#566)

## Decisions

**Chose one `internal/gitrun` package for the stable common contract.** The
package owns a fresh-copy five-key baseline, the shared replacement process
environment, checkout pinning, bounded run/capture, and `GitError`. Verify and
importer retain their package-local sentinels and sha1 gates by supplying the
error class and validating the object format themselves. Their line-ending,
attribute, author, date, encoding, and signing pins remain caller extras. This
keeps the shared seam at the shape proven by repeated use instead of turning it
into a policy authority for every git lane.

**Chose a shared transport preset without a shared transport runner.** Publish
and projectimage now consume `TransportBaseline`, because the complete network
config is a two-consumer invariant rather than a per-package extra. Publish
keeps its own process code, `TransportGitError`, refusal classification,
symlink-resolved repository gate, and authenticated stream discipline; raw
remote output still never enters its errors. Projectimage keeps its
`commandRunner` seam and authenticated-error opacity. Rejected: routing either
through the generic capture runner, which would flatten deliberate credential
and remote-output trust boundaries.

**Normalized transport argv to the baseline-first order.** The scheme-specific
allow now follows all five common keys, making `Baseline()` a literal prefix.
Every config key is distinct, so command semantics are unchanged; the ordering
change makes omission mechanically testable at the shared boundary.

**Moved verifier git execution onto `procbound`.** The previous ten-second
`WaitDelay` remains, now with the process-group cancellation and reap already
used by importer and publish. This closes the verifier git runner's recorded
partial-bound exemption without changing a hardening value.

## Refute-First Verification

A one-off executable harness reconstructed all four pre-refactor config
literals from `origin/main` with `git show`, parsed them as Go syntax, and
compared key/value decisions against the new presets. It covered verify,
importer, projectimage, and 200 deterministic scheme strings for publish. No
key, value, duplicate-key rule, or caller extra changed; only the documented
transport ordering changed.

Focused trust-boundary tests then re-proved baseline contents and fresh-copy
behavior, runner argv construction, caller-sentinel matching, authenticated
token placement, unauthenticated credential absence, transport-error stream
redaction, and projectimage's exact clone argv. Full build, test, vet, and lint
also passed. A complete diff read confirmed that publish's token environment,
stream handling, refusal classification, and config allowlist did not move.

This refute pass was mechanical and same-session. The automated PR reviewer is
the first independent lens.

## Revisit When

Revisit the separate publish/projectimage process seams only when another
consumer repeats one of their authenticated stream models and a shared API can
preserve its output-retention boundary explicitly.
