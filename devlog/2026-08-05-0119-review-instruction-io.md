# Keep Review-Instruction Artifact I/O Retryable

Chose a positive transient allowlist at the ward's instruction-materialization
boundary over the launch classifier's default-transient behavior because these
bytes authorize a credential-bearing review. Only errors carrying
`ErrCodexReviewOperational`, `context.Canceled`, or
`context.DeadlineExceeded` retain the invocation for retry; every unknown
shape and every authenticated content or binding failure remains a durable
contradiction.

Chose ward-local `Open` discrimination over changing the signet blob-store
contract because #515 is confined to the ward lane. A non-not-found
`*fs.PathError` or `syscall.Errno` is operational; `fs.ErrNotExist`, the bare
not-found sentinel used by the production blob store, and unknown errors fail
closed. Read and close failures are operational because a reader was obtained
for the authority digest but its bytes could not be observed reliably.

Chose to propagate a persisted-result re-read failure separately from byte
divergence. A read failure keeps its boundary classification; only a completed
read whose bytes differ from the reconstructed bundle reports an invalid
persisted result. Chose to replace a daemon-owned stale instruction snapshot
before rewriting it because restart retries reuse the same invocation and
snapshot path; private-root and ownership checks still precede every removal.

Chose to apply the same materialization classifier when the engine rechecks
persisted request authority before inspection and readiness. A positively
identified operational read remains transient there and does not enter request
rejection reconciliation; an authenticated content or binding contradiction
still reconciles any launched topology before terminalizing the invocation.

## Refute-First Dispositions

- **Confirmed contradiction:** a bare missing-artifact sentinel,
  `fs.ErrNotExist`, an unknown open error, oversized content, digest-mismatched
  bytes, an invalid binding, and composition divergence cannot enter the
  transient branch. Missing and unknown open errors remain untyped; size and
  digest violations carry `ErrConformance`; binding and composition failures
  carry no transient sentinel.
- **Confirmed and closed:** conclusive authority violations take precedence
  over coincident I/O. Content already observed beyond the size limit remains
  a contradiction even when the read also fails; a complete digest mismatch
  remains a contradiction even when close fails. An incomplete shorter read
  remains operational because its partial bytes cannot establish tampering.
- **Confirmed and closed:** unexpected snapshot topology, including an
  unowned, non-regular, or non-private `AGENTS.md` and extra directory entries,
  carries `ErrConformance`. It cannot become a permanent transient retry wedge;
  only genuine inspection or removal I/O carries the operational sentinel.
- **Confirmed transient:** non-not-found path and errno failures, reader and
  closer failures without a conclusive content violation, cancellation, and
  deadline expiry carry one of the three allowlisted signals. Operational
  cleanup joined with a launch contradiction remains transient intentionally,
  preserving the established operational-over-contradiction precedence.
- **Confirmed and closed:** every authority recheck uses that same allowlist.
  A repeated artifact-store outage remains retryable beyond the first backoff
  interval, while the existing tamper case still takes contradiction teardown.
- **Rejected by verification:** a successful persisted-result read cannot
  reach the byte-divergence branch with different bytes under the same digest
  without a SHA-256 collision. Ordinary tampering is rejected earlier by the
  digest check as a contradiction; the explicit byte comparison remains a
  defense-in-depth check.
- **Rejected by control flow:** cancellation cannot turn already-observed
  missing, oversized, tampered, or divergent content into a retry. Context is
  checked before opening an artifact, while content validation happens only
  after a complete read and close; these paths do not join cancellation into a
  content contradiction.
- **Accepted residual:** an artifact implementation that hides a real I/O
  failure behind an unknown error shape will fail closed as a contradiction.
  This favors authority safety over availability and is the intended default.

Revisit when `CodexReviewInstructionArtifacts.Open` gains a typed ward-visible
not-found/operational error contract; replace the filesystem-shape
discrimination rather than widening the transient fallback.
