import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// #2607 split the card's one Technical Details list into identifier rows
/// (values a reader copies) and reading rows (values a reader reads). The
/// split moves rows; it adds, drops, and rewrites none.
@Suite struct DetailRowPartitionTests {
    static let fixtures: [Components.Schemas.AttentionItem] =
        AttentionFixtures.fixtureTypes.map { AttentionFixtures.fixture(type: $0).item }
        + [
            AttentionFixtures.degradedReady().item,
            AttentionFixtures.staleReady().item,
            AttentionFixtures.reviewDiminishing(cause: .drift_audit, verdict: .stuck).item,
            AttentionFixtures.reviewDiminishing(cause: .growth_without_blockers).item,
            AttentionFixtures.routedReviewDispute().item,
            AttentionFixtures.publishBlockedAfterPublication().item,
            AttentionFixtures.recommendedEffectProposal().item,
            AttentionFixtures.followUpFilingEffectProposal().item,
            AttentionFixtures.imagePolicyAdoptionEffectProposal().item,
            AttentionFixtures.issueCommentEffectProposal().item,
            heldPublication,
        ]

    /// A publication hold that names its code, the one row no stored
    /// fixture carries.
    private static var heldPublication: Components.Schemas.AttentionItem {
        var item = AttentionFixtures.fixture(type: .publish_blocked).item
        item.publish_block?.value1.hold_reason = .init(value1: Components.Schemas.RunHoldReason.allCases[0])
        return item
    }

    /// Acceptance 2: every row the old list drew appears exactly once,
    /// unchanged, on one side of the split.
    @Test(arguments: fixtures)
    func everyBindingRowLandsOnExactlyOneSide(_ item: Components.Schemas.AttentionItem) {
        let prior = "sha256:prior-proposal"
        let proposal = "sha256:proposal"
        let identifiers = AttentionDisplay.detailIdentifierRows(
            item, priorProposalDigest: prior, proposalDigest: proposal)
        let reading = AttentionDisplay.detailReadingRows(item)
        let before = Self.preSplitRows(item, priorProposalDigest: prior, proposalDigest: proposal)

        #expect(!before.isEmpty)
        #expect(Self.counts(identifiers + reading) == Self.counts(before))
        #expect(Set(identifiers.map(\.label)).isDisjoint(with: reading.map(\.label)))
    }

    /// The fixture set reaches every kind of row the split sorts, so the
    /// partition test above is not vacuous for a side.
    @Test func theFixturesReachBothSidesOfEveryBoundary() {
        let identifierLabels = Set(
            Self.fixtures.flatMap { AttentionDisplay.detailIdentifierRows($0) }.map(\.label))
        let readingLabels = Set(Self.fixtures.flatMap { AttentionDisplay.detailReadingRows($0) }.map(\.label))

        for label in ["Evaluation Set", "Bound Head", "Bound Base", "Handoff Tip", "Drift Audit", "Hold Code"] {
            #expect(identifierLabels.contains(label), "\(label)")
        }
        for label in ["Posture", "Readiness", "Terminal Review"] {
            #expect(readingLabels.contains(label), "\(label)")
        }
        #expect(readingLabels.contains { $0.hasPrefix("Requirement ") })
        #expect(readingLabels.contains { $0.hasPrefix("Waiver ") })
        #expect(readingLabels.contains { $0.hasPrefix("Review Round ") })
    }

    /// Acceptance 4: the diminishing-returns card's review rounds read in the
    /// body, not among the identifiers.
    @Test func diminishingReturnsReviewRoundsAreReadingRows() {
        let item = AttentionFixtures.fixture(type: .review_diminishing_returns).item

        let reading = AttentionDisplay.detailReadingRows(item)
        let identifiers = AttentionDisplay.detailIdentifierRows(item)

        #expect(reading.contains { $0.label.hasPrefix("Review Round ") })
        #expect(!identifiers.contains { $0.label.hasPrefix("Review Round ") })
        #expect(!identifiers.contains { $0.label == "Terminal Review" })
    }

    private static func counts(_ rows: [AttentionDisplay.BindingRow]) -> [String: Int] {
        rows.reduce(into: [:]) { $0["\($1.label)\u{1F}\($1.value)", default: 0] += 1 }
    }

    /// A frozen copy of `detailBindingRows` as it stood before #2607, kept as
    /// the partition's reference. It deliberately duplicates the old order
    /// and readiness rows rather than calling the split functions.
    private static func preSplitRows(
        _ item: Components.Schemas.AttentionItem,
        priorProposalDigest: String?,
        proposalDigest: String?
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = []
        if let posture = item.posture?.value1 {
            rows.append(.init(label: "Posture", value: AttentionDisplay.label(posture)))
        }
        if let created = item.created_at {
            rows.append(.init(label: "Created", value: created.formatted(.iso8601)))
        }
        if let due = item.expires_when {
            rows.append(.init(label: "Due", value: due.formatted(.iso8601)))
        }
        switch item.subject {
        case .project(let unscoped), .system(let unscoped):
            rows.append(.init(label: "Subject", value: unscoped.subject_id))
        case .run, .proposal_batch, .task:
            break
        }
        if let wait = item.blocked_on?.value1 {
            rows.append(.init(label: "Waiting Since", value: wait.since.formatted(.iso8601)))
            if let blockingItem = wait.item_id {
                rows.append(.init(label: "Blocking Item", value: blockingItem))
            }
        }
        if let hold = item.publish_block?.value1.hold_reason?.value1 {
            rows.append(.init(label: "Hold Code", value: hold.rawValue))
        }
        // #3026 moved a finding's raw diagnostic code here from its facts.
        if let diagnostic = item.health_diagnostic?.value1 {
            rows.append(.init(label: "Diagnostic Code", value: diagnostic.code))
        }
        rows.append(.init(label: "Item Version", value: "\(item.item_version)"))
        if !item.pr_head_sha.isEmpty {
            rows.append(.init(label: "PR Head", value: item.pr_head_sha))
        }
        if let diff = item.diff_stats?.value1 {
            rows.append(.init(label: "Diff Base", value: diff.base_sha))
            rows.append(.init(label: "Diff Head", value: diff.head_sha))
        }
        rows.append(contentsOf: AttentionDisplay.attachmentDigestRows(item))
        if let priorProposalDigest {
            rows.append(.init(label: "Prior Proposal", value: priorProposalDigest))
        }
        if let proposalDigest {
            rows.append(.init(label: "Proposal", value: proposalDigest))
        }
        rows.append(contentsOf: AttentionDisplay.reviewRecoveryBindingRows(item))
        rows.append(contentsOf: AttentionDisplay.reviewConfigurationRecoveryRows(item))
        rows.append(contentsOf: AttentionDisplay.codexReenrollmentRecoveryRows(item))
        rows.append(contentsOf: AttentionDisplay.findingAdjudicationRows(item))
        if let readiness = item.readiness?.value1 {
            let verdict =
                switch readiness._class {
                case .ready_clean: "Clean"
                case .ready_degraded: "Degraded"
                case .unknown: "Unknown"
                }
            rows.append(.init(label: "Readiness", value: verdict))
            rows.append(.init(label: "Evaluation Set", value: readiness.evaluation_set_digest))
            if let detail = item.readiness_detail?.value1 {
                rows.append(.init(label: "Bound Head", value: detail.candidate_head))
                rows.append(
                    .init(label: "Bound Base", value: "\(detail.base.base_ref)@\(detail.base.base_sha)"))
                if let tip = detail.handoff_tip {
                    rows.append(.init(label: "Handoff Tip", value: tip))
                }
                if let merge = detail.handoff_merge {
                    rows.append(.init(label: "Handoff Merge", value: merge.merge_sha))
                }
                // The requirement and waiver rows were the tail of the old
                // readiness rows; the reading half still builds them alone.
                rows.append(contentsOf: AttentionDisplay.readinessReadingRows(item).dropFirst())
            }
        }
        rows.append(contentsOf: AttentionDisplay.reviewYieldRows(item))
        if let drift = item.review_diminishing?.value1.drift_audit?.value1 {
            rows.append(.init(label: "Drift Audit", value: drift.audit_digest))
        }
        return rows
    }
}
