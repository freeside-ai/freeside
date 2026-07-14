// Package fake holds the permanent scripted fakes of the exec contract
// (plan §5.3 "permanent fakes of both"): first-class fixtures every lane
// tests against, not throwaway mocks. Names stutter deliberately
// (fake.StageDriver implements exec.StageDriver), the httptest naming shape.
//
// Determinism is structural: a scenario is scripted per invocation id before
// Start/RequestReview, and all progression is call-step-counted. "Delay" is a
// number of calls observing pending/running or an unready poll, never a
// clock; there are no goroutines and no randomness, so every scenario
// replays identically on any platform. The fakes ignore ctx for the same
// reason. A mutex makes them safe under concurrent callers (the Wave 2
// engine's tests), without affecting single-caller determinism.
//
// The crash scenarios model the §5.3 durability boundary as three durable
// facets against one transient one. Durable: the scripted scenarios (the
// external reality), the committed invocation intents (the outbox record, one
// per id), and the committed-result registry. Transient: the per-invocation
// session progress, which is the provider session a crash or restart loses. A
// result committed before the loss stays collectable (StatusGone from Inspect,
// the result from Collect/Poll); a loss before any result leaves ErrNoResult.
// Collect and Poll re-deliver the identical committed result on every call:
// duplicate delivery is inherent to the contract, and accepting at most once
// is the caller's job.
//
// The durability is real, not simulated: NewStageDriverAt(dir) and
// NewReviewSourceAt(dir) persist the three durable facets under dir (one
// atomic-rename JSON file per fake) and reload them on construction, so the
// committed intent and result outlive a genuine daemon-process kill, not just
// an in-process Outcome flag. Reconstructing from the same dir *is* the
// restart boundary: the durable facets reload, no live session does, so every
// intent that had not committed a result reads as a lost session while a
// committed result stays recoverable by id. Persistence is opt-in; the plain
// NewStageDriver/NewReviewSource constructors stay in-memory for the fast-lane
// tests. The on-disk state is clock-free (no timestamp, sorted map keys), so
// reconstruction is a pure function of the dir and equal runs write
// byte-identical files. Each mutator commits its in-memory change and the
// durable write as one atomic step: a persistence failure (unwritable dir,
// full disk) is a broken test environment, not a scripted scenario, so it
// panics rather than return an error, leaving no half-committed state for a
// caller to diverge on.
//
// This is the provider half of the effectively-once boundary. The daemon half
// (the store's inbox/outbox ledger, plan §5.9) and a harness that kills the
// actual daemon process land with the Phase 1A integration harness and the
// Wave 2 engine; NewStageDriverAt's dir is what that harness reuses, with the
// discard-and-reconstruct here becoming a real SIGKILL and restart.
//
// Layout, by concept:
//
//   - stagedriver.go    fake.StageDriver and its StageScript
//   - reviewsource.go   fake.ReviewSource and its ReviewScript
//   - persist.go        the shared durable-state load/atomic-write helpers
//   - runnerbackend.go  fake.RunnerBackend, a capability-declaring value
package fake
