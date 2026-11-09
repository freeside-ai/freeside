import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// The System Health pane's lists (#2920): blocking findings lead the open
/// ones, each part newest first; closed findings run newest first by when
/// they closed; and only system_health items list at all.
@Suite struct SystemHealthFindingsTests {
    private typealias Item = Components.Schemas.AttentionItemSnapshot

    private func finding(
        _ id: String, posture: Components.Schemas.HealthPosture? = .advisory, created: TimeInterval = 0
    ) -> Item {
        var snapshot = AttentionFixtures.fixture(type: .system_health)
        snapshot.item.id = id
        snapshot.item.posture = posture.map { .init(value1: $0) }
        snapshot.item.created_at = Date(timeIntervalSince1970: created)
        return snapshot
    }

    private func closed(_ id: String, cause: Components.Schemas.ConclusionCause, after offset: TimeInterval) -> Item {
        AttentionFixtures.closedByDaemon(
            AttentionFixtures.fixture(type: .system_health), id: id, cause: cause, after: offset,
            reason: "Closed.")
    }

    @Test func blockingFindingsLeadAndEachPartRunsNewestFirst() {
        let findings = SystemHealthFindings(
            open: [
                finding("advisory-old", created: 10), finding("blocking-old", posture: .blocking, created: 20),
                finding("advisory-new", created: 40), finding("blocking-new", posture: .blocking, created: 30),
                finding("no-posture", posture: nil, created: 50),
            ],
            closed: [])

        #expect(
            findings.open.map(\.id) == [
                "blocking-new", "blocking-old", "no-posture", "advisory-new", "advisory-old",
            ])
        #expect(findings.openCount == 5)
        #expect(findings.blockingCount == 2)
    }

    @Test func closedFindingsRunNewestFirstByWhenTheyClosed() {
        let findings = SystemHealthFindings(
            open: [],
            closed: [
                closed("first", cause: .resolved_by_daemon, after: 100),
                closed("last", cause: .expired, after: 300),
                closed("middle", cause: .superseded, after: 200),
            ])

        #expect(findings.closed.map(\.id) == ["last", "middle", "first"])
    }

    @Test func onlySystemHealthItemsList() {
        var question = AttentionFixtures.fixture(type: .agent_question)
        question.item.id = "question"
        let findings = SystemHealthFindings(
            open: [question, finding("finding")],
            closed: [AttentionFixtures.decidedReviewDispute(), closed("closed", cause: .resolved_by_daemon, after: 0)])

        #expect(findings.open.map(\.id) == ["finding"])
        #expect(findings.closed.map(\.id) == ["closed"])
    }

    @Test func theKeywordIsThePostureWhileOpenAndTheOutcomeOnceClosed() {
        #expect(SystemHealthFindings.keyword(finding("advisory").item) == "Advisory")
        #expect(SystemHealthFindings.keyword(finding("blocking", posture: .blocking).item) == "Blocking")
        #expect(SystemHealthFindings.keyword(finding("unset", posture: nil).item) == "Advisory")
        #expect(
            SystemHealthFindings.keyword(closed("resolved", cause: .resolved_by_daemon, after: 0).item) == "Resolved")
        #expect(SystemHealthFindings.keyword(closed("expired", cause: .expired, after: 0).item) == "Expired")
        #expect(SystemHealthFindings.keyword(closed("superseded", cause: .superseded, after: 0).item) == "Superseded")
    }
}
