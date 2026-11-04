#if os(macOS)
    import Foundation
    import FreesideAPI

    @testable import FreesideCore

    /// The account usage view's states for the screenshot suite, each reached
    /// through the mock daemon the way the app reaches it. Each state pins the
    /// client clock it is drawn at, against the usage fixtures' instants.
    @MainActor enum AccountUsageScreenshots {
        /// The usage fixtures' epoch (`UsageFixtures`).
        nonisolated private static let base = Date(timeIntervalSince1970: 1_786_502_400)

        enum State: CaseIterable {
            /// The default pools: one with readings and refusal marks and two
            /// enrollments to collect, one whose collector can't read usage,
            /// and one with no reading yet.
            case usage
            /// The same pools once the client's clock passes the readings'
            /// `stale_at`.
            case stale
            /// The #2389 shape for Claude under a setup token: a weekly share
            /// with its reset, and a five-hour window with a status alone.
            case claudeVerdict
            /// A refresh the daemon never answered, waiting for Retry.
            case unconfirmed
            /// A reassignment the daemon refused.
            case failed
            /// A reassignment sync confirmed: the new collector has no reading
            /// yet, and the pool's refusal mark outlives the old readings.
            case reassigned

            var surface: String {
                switch self {
                case .usage: "usage"
                case .stale: "usage-stale"
                case .claudeVerdict: "usage-claude-verdict"
                case .unconfirmed: "usage-unconfirmed"
                case .failed: "usage-failed"
                case .reassigned: "usage-reassigned"
                }
            }

            /// After the readings were observed and before they go stale,
            /// except in the stale state.
            var now: Date {
                base.addingTimeInterval(self == .stale ? 5_000 : 3_000)
            }
        }

        static func model(_ state: State) async -> AccountUsageModel {
            let server =
                state == .claudeVerdict ? MockServer(usagePools: [claudeVerdictPool()]) : MockServer()
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            await coordinator.refresh()
            let model = AccountUsageModel(coordinator: coordinator)
            switch state {
            case .usage, .stale, .claudeVerdict:
                break
            case .unconfirmed:
                await server.setBeforeRespond { operation in
                    if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
                }
                await model.refresh(pool: UsageFixtures.readingPoolID)
            case .failed:
                await server.setBeforeRespond { operation in
                    if operation == "submitCommand" { throw MockServer.ForcedStatus(400) }
                }
                await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
            case .reassigned:
                await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
            }
            return model
        }

        /// Built here rather than in `UsageFixtures`, which mirrors the
        /// daemon's bootstrap golden.
        private static func claudeVerdictPool() -> Components.Schemas.UsagePoolSnapshot {
            let enrollment = "enrollment-claude-setup-token"
            return .init(
                as_of_revision: 4, entity_version: 2,
                pool: .init(
                    id: "claude-setup", provider: "anthropic", max_parallel_executions: 1, plan: "max",
                    collector: enrollment,
                    last_attempt: .init(
                        value1: .init(status: .succeeded, at: base.addingTimeInterval(2_400), detail: "")),
                    enrollments: [
                        .init(
                            enrollment_id: enrollment, auth_identity_id: "identity-claude-setup",
                            harness_client: .claude_code,
                            availability: .init(
                                value1: .init(answer: .supported, observed_at: base.addingTimeInterval(2_400))))
                    ],
                    model_windows: [],
                    windows: [
                        .init(
                            window: "five_hour", enrollment_id: enrollment, generation: 1, status: .allowed,
                            resets_at: base.addingTimeInterval(12_600), observed_at: base.addingTimeInterval(1_200),
                            stale_at: base.addingTimeInterval(4_800)),
                        .init(
                            window: "seven_day", enrollment_id: enrollment, generation: 1, status: .allowed,
                            used_percent: 63, resets_at: base.addingTimeInterval(270_000),
                            length_seconds: 604_800, observed_at: base.addingTimeInterval(2_400),
                            stale_at: base.addingTimeInterval(4_200)),
                    ],
                    recorded_exhausted_marks: []))
        }
    }
#endif
