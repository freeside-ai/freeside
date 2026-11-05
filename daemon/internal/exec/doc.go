// Package exec holds the daemon's execution contract: the StageDriver and
// ReviewSource interfaces every driver implements (plan §5.3) and the
// RunnerBackend capability model (§5.7). It is spine-owned and changes only
// through kind:contract work units. It is interfaces, contract types, and
// validation: no provider code, no I/O, no persistence. Three files are
// stated exceptions, each with the reason it lives here:
//
//   - review_instruction.go: ComposeCodexReviewInstructions builds Codex's
//     review-instruction bundle (provider code). The review contract carries
//     its ReviewInstructionBinding, and the engine, the ward, and freesided
//     preflight must produce the same bytes and digest.
//   - usage_claude.go: ExtractClaudeUsage parses Claude's transcript result
//     envelope (provider code), shared by exec/claude and the ward's review
//     sources.
//   - materialize.go: Materializer reads stage inputs through the injected
//     InputSource (I/O), because MaterializingStageDriver is the one
//     production start seam: it verifies input digests before a real driver
//     commits an invocation intent.
//
// Moving any of these out of exec is a kind:contract change. Real drivers
// (Claude, CodexGitHubReview) and runner backends land in their own lanes;
// the permanent scripted fakes live in exec/fake.
//
// Every operation is keyed by the daemon-generated domain.InvocationID handed
// to Start/RequestReview, so an external invocation is reconcilable across
// daemon restarts and provider crashes: one committed invocation intent and
// at most one accepted result (§5.3). Collect and Poll are idempotent
// re-deliveries of the committed result; accepting it at most once is the
// caller's job (the Wave 2 engine, durably), never the driver's.
//
// Layout, by concept:
//
//   - status.go             the shared invocation Status vocabulary
//   - driver.go             StageDriver and its StartSpec/StageResult contract (§5.3)
//   - materialize.go        digest-verified immutable stage inputs
//   - review.go             ReviewSource and its ReviewRequest/ReviewResult contract (§5.3)
//   - review_instruction.go the review-instruction binding and Codex bundle composition
//   - summary.go            the bounded StageResult summary (TruncateSummary)
//   - usage.go              UsageMeasurement, the numbers-only usage report
//   - usage_claude.go       Claude transcript usage extraction (ExtractClaudeUsage)
//   - usage_capture.go      ClaudeUsageCapture, the writer's post-turn provider windows
//     and rate-limit events, and its shape validation
//   - capability.go         Capability, RunnerBackend, CheckCapabilities (§5.7)
//   - errors.go             sentinel errors (wrapped with %w)
//
// See docs/plan.md §5.3 (execution and review operation sets, the
// at-most-one-accepted-result guarantee, session durability) and §5.7 (the
// ward's capability model: backends declare, policy states minimums, no
// silent downgrade).
//
// # Conventions
//
// Patterns set here for later lanes to copy (recorded for spine review in the
// Wave 0 exec devlog entry), on top of the domain package's enum and
// validation conventions, which this package follows:
//
//   - Every implementation of a contract interface carries a compile-time
//     assertion (var _ exec.StageDriver = (*fake.StageDriver)(nil)) so a
//     signature drift fails the build, not a test. StageDriver and
//     ReviewSource implementations also call the shared runners in
//     internal/exec/contract from their own tests, using an implementation
//     harness to realize lifecycle scenarios without moving assertions out
//     of the runner.
//   - A typed refusal is a struct error carrying the machine-readable facts
//     (which capabilities are missing) plus an Unwrap to a sentinel, so
//     errors.Is matches the class and errors.As reaches the details; see
//     CapabilityRefusal.
package exec
