import Foundation

/// Deterministic usage-pool and routing-policy fixtures, mirroring the cases
/// the daemon's bootstrap golden pins (daemon/internal/signet/
/// golden_internal_test.go): a pool with readings and an exhausted window, a
/// pool whose collector cannot read usage, and a pool with no record yet.
public enum UsageFixtures {
    public static let readingPoolID = "claude-a"
    public static let unsupportedPoolID = "codex-b"
    public static let unrecordedPoolID = "spare"
    public static let readingCollectorID = "enrollment-claude-a-mac"
    public static let readingSecondEnrollmentID = "enrollment-claude-a-mini"
    public static let unsupportedCollectorID = "enrollment-codex-b-mac"

    /// The agents the mock's lineup declares, which set_routing_policy
    /// accepts, as the daemon checks a policy against its lineup tree.
    public static let lineupAgents = ["claude-primary", "claude-secondary", "codex-primary"]
    public static let lineupRevision = "sha256:" + String(repeating: "a", count: 64)

    public static func defaultUsagePools() -> [Components.Schemas.UsagePoolSnapshot] {
        [
            .init(
                as_of_revision: 11, entity_version: 3,
                pool: .init(
                    id: readingPoolID, provider: "anthropic", max_parallel_executions: 2,
                    plan: "max", collector: readingCollectorID,
                    last_attempt: .init(value1: .init(status: .succeeded, at: date(2_400), detail: "")),
                    enrollments: [
                        .init(
                            enrollment_id: readingCollectorID, auth_identity_id: "identity-claude-a",
                            harness_client: .claude_code,
                            availability: .init(value1: .init(answer: .supported, observed_at: date(2_400)))),
                        .init(
                            enrollment_id: readingSecondEnrollmentID,
                            auth_identity_id: "identity-claude-a", harness_client: .claude_code),
                    ],
                    model_windows: [.init(route_model_id: "claude-opus", window: "seven_day_opus")],
                    windows: [
                        .init(
                            window: "five_hour", enrollment_id: readingCollectorID, generation: 1,
                            status: .allowed, used_percent: 42, resets_at: date(9_000),
                            length_seconds: 18_000, observed_at: date(2_400), stale_at: date(4_200)),
                        .init(
                            window: "seven_day_opus", enrollment_id: readingCollectorID, generation: 1,
                            status: .exhausted, used_percent: 100, resets_at: date(86_400),
                            length_seconds: 604_800, observed_at: date(2_400), stale_at: date(4_200)),
                    ],
                    exhausted_marks: [
                        .init(
                            window: "seven_day_opus", source: .usage_reading,
                            reading_observed_at: date(2_400), resets_at: date(86_400), set_at: date(2_400))
                    ])),
            .init(
                as_of_revision: 9, entity_version: 1,
                pool: .init(
                    id: unsupportedPoolID, provider: "openai", max_parallel_executions: 1,
                    collector: unsupportedCollectorID,
                    last_attempt: .init(
                        value1: .init(
                            status: .unsupported, at: date(1_800),
                            detail: "the provider reports no usage for this plan")),
                    enrollments: [
                        .init(
                            enrollment_id: unsupportedCollectorID, auth_identity_id: "identity-codex-b",
                            harness_client: .codex_cli,
                            availability: .init(value1: .init(answer: .unsupported, observed_at: date(1_800))))
                    ],
                    model_windows: [], windows: [], exhausted_marks: [])),
            // No record yet: the daemon projects version 0 at the snapshot's
            // revision, and no collector because nothing is enrolled.
            .init(
                as_of_revision: 1, entity_version: 0,
                pool: .init(
                    id: unrecordedPoolID, provider: "anthropic", max_parallel_executions: 1,
                    enrollments: [], model_windows: [], windows: [], exhausted_marks: [])),
        ]
    }

    public static func defaultRoutingPolicy() -> Components.Schemas.RoutingPolicy {
        .init(
            revision: 2,
            roles: [.init(role: .implementer, agents: ["claude-primary", "codex-primary"])],
            source: .client_routing_set, set_by: "device:mock-device", set_at: date(1_200),
            lineup_revision: lineupRevision)
    }

    private static func date(_ offset: TimeInterval) -> Date {
        Date(timeIntervalSince1970: 1_786_502_400 + offset)
    }
}
