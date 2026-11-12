import Foundation
import FreesideAPI
import Testing

/// Checks `ActionOutcome.of` against the daemon's own outcome table, which
/// the daemon's TestActionOutcomeVector writes to
/// daemon/internal/signet/testdata/action-outcomes.golden. CI selects the app
/// component when that file changes, so a daemon-only change to the table
/// runs this test.
@Suite struct ActionOutcomeTests {
    private struct VectorEntry: Decodable {
        let action: String
        let status: String
        let kind: String
        let byItemType: [ItemTypeOverride]
    }

    private struct ItemTypeOverride: Decodable {
        let itemType: String
        let status: String
        let kind: String
    }

    private static func loadVector() throws -> [VectorEntry] {
        let repositoryRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // FreesideAPITests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // app
            .deletingLastPathComponent()
        let url = repositoryRoot.appendingPathComponent(
            "daemon/internal/signet/testdata/action-outcomes.golden")
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return try decoder.decode([VectorEntry].self, from: Data(contentsOf: url))
    }

    /// The client outcome a daemon (status, kind) pair maps to, or nil for a
    /// pair this mapping has not reviewed. The daemon has more kinds than the
    /// client has cases: a kind whose whole client-visible effect is the
    /// status flip collapses into `.concludes(status)`. "" is the daemon's
    /// "no status change".
    private static func clientOutcome(status: String, kind: String) -> ActionOutcome? {
        let concluded = Components.Schemas.ItemStatus(rawValue: status)
        switch (kind, concluded) {
        case ("concludes", let status?):
            return .concludes(status)
        // Each concludes the item with the daemon's status; the rest of the
        // transaction (the trust reevaluation intent, the adjudication, the
        // proposal ledger, the engine's retry) is server-side, and the client
        // shows only the conclusion. approve on an effect_proposal lands here
        // as starts_proposal, the outcome start has.
        case ("reruns_trust_evaluation", let status?),
            ("accepts_recommended_route", let status?),
            ("chooses_alternative_route", let status?),
            ("adjudicates_findings", let status?),
            ("starts_proposal", let status?),
            ("declines_proposal", let status?),
            ("answers_and_retries", let status?),
            ("returns_to_agent", let status?),
            ("retries_with_capabilities", let status?),
            ("retries_with_alternate_agent", let status?):
            return .concludes(status)
        // The table says resolved, but the daemon's revise handler
        // supersedes the item with a revised replacement and starts it;
        // `.revisesProposal` stands for that whole transaction, which the
        // mock replays the same way.
        case ("revises_and_starts_proposal", .resolved?):
            return .revisesProposal
        // run_doctor makes the diagnostic due and leaves the item open, which
        // is what the client's record-only receipt says.
        case ("runs_doctor", nil):
            return .records
        case ("records", nil):
            return .records
        case ("discusses", nil):
            return .discusses
        case ("pending", nil):
            return .pending
        case ("snoozes_proposal", nil):
            return .snoozesProposal
        case ("stops_unattended", .resolved?):
            return .stopsUnattended
        case ("resumes_unattended", .resolved?):
            return .resumesUnattended
        case ("recovers_review", .resolved?):
            return .recoversReview
        case ("adopts_review_configuration", .resolved?):
            return .adoptsReviewConfiguration
        case ("resolves_reenrollment", .resolved?):
            return .resolvesReenrollment
        default:
            return nil
        }
    }

    @Test func outcomeTableMatchesDaemonVector() throws {
        let vector = try Self.loadVector()
        for entry in vector {
            let action = try #require(
                Components.Schemas.Action(rawValue: entry.action),
                "daemon action \(entry.action) is not a client Action")
            let expected = try #require(
                Self.clientOutcome(status: entry.status, kind: entry.kind),
                "no client mapping for \(entry.action): (\"\(entry.status)\", \(entry.kind))")
            #expect(ActionOutcome.of(action) == expected, "action \(entry.action)")
            // ActionOutcome.of sees only the action, so every item-type
            // override must map to the same client outcome.
            for override in entry.byItemType {
                let typed = try #require(
                    Self.clientOutcome(status: override.status, kind: override.kind),
                    """
                    no client mapping for \(entry.action) on \(override.itemType): \
                    ("\(override.status)", \(override.kind))
                    """)
                #expect(
                    ActionOutcome.of(action) == typed,
                    "action \(entry.action) on \(override.itemType)")
            }
        }
        let actions = vector.map(\.action)
        #expect(actions.count == Set(actions).count)
        // unknown is the client's decode-only member: the daemon never sends
        // it, so its table doesn't list it, and it stays pending.
        let wireActions = Components.Schemas.Action.allCases.filter { $0 != .unknown }
        #expect(Set(actions) == Set(wireActions.map(\.rawValue)))
        #expect(Set(vector.filter { $0.kind == "pending" }.map(\.action)) == ["convert_to_policy"])
        #expect(ActionOutcome.of(.unknown) == .pending)
    }
}
