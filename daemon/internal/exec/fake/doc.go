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
// The crash scenarios model the §5.3 durability boundary with two stores: a
// per-session state that a crash destroys, and a committed-results registry
// keyed by invocation id that survives it. A result committed before the
// crash stays collectable (StatusGone from Inspect, the result from
// Collect/Poll); a crash before any result leaves ErrNoResult. Collect and
// Poll re-deliver the identical committed result on every call: duplicate
// delivery is inherent to the contract, and accepting at most once is the
// caller's job.
//
// Layout, by concept:
//
//   - stagedriver.go    fake.StageDriver and its StageScript
//   - reviewsource.go   fake.ReviewSource and its ReviewScript
//   - runnerbackend.go  fake.RunnerBackend, a capability-declaring value
package fake
