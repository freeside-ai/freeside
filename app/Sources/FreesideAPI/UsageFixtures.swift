import Foundation

/// Deterministic usage-pool, routing-policy, and agent-catalog fixtures,
/// mirroring the cases the daemon's bootstrap golden pins (daemon/internal/
/// signet/golden_internal_test.go): a pool with readings and a window that
/// holds both an older refusal and a newer exhausted reading resetting first,
/// a pool whose collector cannot read usage, a pool with no record yet, and a
/// catalog whose agents run as those pools' enrollments.
public enum UsageFixtures {
    public static let readingPoolID = "claude-a"
    public static let unsupportedPoolID = "codex-b"
    public static let unrecordedPoolID = "spare"
    public static let readingCollectorID = "enrollment-claude-a-mac"
    public static let readingSecondEnrollmentID = "enrollment-claude-a-mini"
    public static let unsupportedCollectorID = "enrollment-codex-b-mac"

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
                    recorded_exhausted_marks: [
                        .init(
                            window: "seven_day_opus", source: .provider_refusal,
                            invocation_id: "invocation-refused", resets_at: date(172_800), set_at: date(1_800)),
                        .init(
                            window: "seven_day_opus", source: .usage_reading,
                            reading_observed_at: date(2_400), resets_at: date(86_400), set_at: date(2_400)),
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
                    model_windows: [], windows: [], recorded_exhausted_marks: [])),
            // No record yet: the daemon projects version 0 at the snapshot's
            // revision, and no collector because nothing is enrolled.
            .init(
                as_of_revision: 1, entity_version: 0,
                pool: .init(
                    id: unrecordedPoolID, provider: "anthropic", max_parallel_executions: 1,
                    enrollments: [], model_windows: [], windows: [], recorded_exhausted_marks: [])),
        ]
    }

    /// Set from the other paired device fixture, so a client names it.
    public static func defaultRoutingPolicy() -> Components.Schemas.RoutingPolicy {
        .init(
            revision: 2,
            roles: [.init(role: .implementer, agents: ["claude-primary", "codex-primary"])],
            source: .client_routing_set, set_by: "device:\(DeviceFixtures.otherDeviceID)", set_at: date(1_200),
            lineup_revision: lineupRevision, drain_first_account: nil)
    }

    /// The mock's agent catalog. Its tree half holds the agents
    /// set_routing_policy accepts, as the daemon checks a policy against its
    /// tree: each agent's enrollment joins a pool fixture's enrollment, one
    /// offer declares no lineage group, and the lineup binds every task-line
    /// role. Its enrollments are those pools' enrollments, two with a
    /// discovery record and one the daemon hasn't discovered yet, so a
    /// selection on it passes on shape alone.
    public static func defaultAgentCatalog() -> Components.Schemas.AgentCatalog {
        let claudePrimary = selection(readingCollectorID, "claude-opus", "high")
        let claudeSecondary = selection(readingSecondEnrollmentID, "claude-opus", "max")
        let codexPrimary = selection(unsupportedCollectorID, "gpt-codex", "harness_default")
        return .init(
            lineup_revision: .init(value1: lineupRevision),
            enrollments: [
                .init(
                    enrollment_id: readingCollectorID, harness_client: .claude_code,
                    discovery: .init(
                        value1: .init(
                            harness_build: "claude-code 2.1.0", observed_at: date(600),
                            models: [
                                .init(
                                    id: "claude-opus", resolved_model: "claude-opus-5-5", display_name: "Opus",
                                    effort_levels: ["low", "medium", "high", "xhigh", "max"]),
                                .init(
                                    id: "claude-sonnet", resolved_model: "claude-sonnet-5-5",
                                    display_name: "Sonnet", effort_levels: ["low", "medium", "high"]),
                            ]))),
                .init(enrollment_id: readingSecondEnrollmentID, harness_client: .claude_code, discovery: nil),
                .init(
                    enrollment_id: unsupportedCollectorID, harness_client: .codex_cli,
                    discovery: .init(
                        value1: .init(
                            harness_build: "codex-cli 0.50.0", observed_at: date(600),
                            models: [
                                .init(
                                    id: "gpt-codex", resolved_model: nil, display_name: "GPT Codex",
                                    effort_levels: ["low", "medium", "high"])
                            ]))),
            ],
            agents: [
                .init(
                    name: "claude-primary", enrollment_id: readingCollectorID, model: "claude-opus",
                    effort: .high, lineage_group: "anthropic", selection: claudePrimary),
                .init(
                    name: "claude-secondary", enrollment_id: readingSecondEnrollmentID, model: "claude-opus",
                    effort: .max, lineage_group: "anthropic", selection: claudeSecondary),
                .init(
                    name: "codex-primary", enrollment_id: unsupportedCollectorID, model: "gpt-codex",
                    effort: .harness_default, lineage_group: nil, selection: codexPrimary),
            ],
            lineup: [
                .init(role: .specifier, agent: "claude-primary", selection: claudePrimary),
                .init(role: .implementer, agent: "claude-primary", selection: claudePrimary),
                .init(role: .remediator, agent: "claude-secondary", selection: claudeSecondary),
                .init(role: .reviewer, agent: "codex-primary", selection: codexPrimary),
            ])
    }

    /// The catalog of a daemon that holds no committed tree: the enrollments
    /// still, and no agents, lineup, or revision.
    public static func treelessAgentCatalog() -> Components.Schemas.AgentCatalog {
        .init(lineup_revision: nil, enrollments: defaultAgentCatalog().enrollments, agents: [], lineup: [])
    }

    public static func selection(
        _ enrollment: String, _ model: String, _ effort: String
    ) -> Components.Schemas.TaskLineSelection {
        .init(enrollment_id: enrollment, model_id: model, effort: effort)
    }

    /// The selection the default catalog's tree runs `agent` with, as a
    /// client's choice of that agent sends it.
    public static func agentSelection(_ agent: String) -> Components.Schemas.TaskLineSelection {
        guard let entry = defaultAgentCatalog().agents.first(where: { $0.name == agent }) else {
            preconditionFailure("no default catalog agent \(agent)")
        }
        return entry.selection
    }

    private static func date(_ offset: TimeInterval) -> Date {
        Date(timeIntervalSince1970: 1_786_502_400 + offset)
    }
}
