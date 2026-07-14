// Package exec holds the daemon's execution contract: the StageDriver and
// ReviewSource interfaces every driver implements (plan §5.3). It is
// spine-owned and changes only
// through kind:contract work units. It is interfaces, contract types, and
// validation only: no provider code, no I/O, no persistence. Real drivers
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
//   - status.go      the shared invocation Status vocabulary
//   - driver.go      StageDriver and its StartSpec/StageResult contract (§5.3)
//   - review.go      ReviewSource and its ReviewRequest/ReviewResult contract (§5.3)
//   - errors.go      sentinel errors (wrapped with %w)
//
// See docs/plan.md §5.3 (execution and review operation sets, the
// at-most-one-accepted-result guarantee, session durability).
//
// # Conventions
//
// Patterns set here for later lanes to copy (recorded for spine review in the
// Wave 0 exec devlog entry), on top of the domain package's enum and
// validation conventions, which this package follows:
//
//   - Every implementation of a contract interface carries a compile-time
//     assertion (var _ exec.StageDriver = (*fake.StageDriver)(nil)) so a
//     signature drift fails the build, not a test.
package exec
