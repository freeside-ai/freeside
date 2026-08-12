import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct AttentionDisplayTests {
    @Test func existingPullRequestActionUsesViewLanguage() {
        #expect(AttentionDisplay.label(Components.Schemas.Action.open_pr) == "View PR")
    }

    @Test func healthPostureLabelsAreExplicit() {
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.blocking) == "Blocking")
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.advisory) == "Advisory")
    }

    @Test func attachmentDigestsKeepTheirEvidenceAndClaimContext() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item

        let rows = AttentionDisplay.attachmentDigestRows(item)

        #expect(rows.count == item.artifact_digests.count)
        #expect(rows.first?.label == "Evidence digest")
        #expect(rows.dropFirst().allSatisfy { $0.label == "Claim digest" })
        #expect(Set(rows.map(\.value)) == Set(item.artifact_digests))
    }

    @Test func sharedAttachmentDigestKeepsBothTrustChannelLabels() {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let digest = item.evidence_snapshot[0].digest
        item.agent_claims[0].digest = digest
        item.artifact_digests = Array(Set(item.agent_claims.map(\.digest) + [digest])).sorted()

        let rows = AttentionDisplay.attachmentDigestRows(item)

        #expect(rows.contains(.init(label: "Evidence digest", value: digest)))
        #expect(rows.contains(.init(label: "Claim digest", value: digest)))
    }

    @Test func detailBindingsKeepDistinctLabelsThatShareAValue() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        let rows = AttentionDisplay.detailBindingRows(item)

        #expect(rows.contains(.init(label: "PR head", value: "cafebabe")))
        #expect(rows.contains(.init(label: "Head", value: "cafebabe")))
    }

    @Test func proposalBindingSurvivesMatchingAttachmentDigest() {
        let item = AttentionFixtures.fixture(type: .run_proposal).item
        let digest = item.artifact_digests[0]

        let rows = AttentionDisplay.detailBindingRows(item, proposalDigest: digest)

        #expect(rows.contains { $0.value == digest && $0.label.hasSuffix("digest") })
        #expect(rows.contains(.init(label: "Proposal", value: digest)))
    }

    @Test func runSubjectLeadsWithProjectAndDemotesTheRunID() {
        let item = AttentionFixtures.fixture(type: .execution_failure).item

        #expect(
            AttentionDisplay.subject(item)
                == .init(lead: "proj-1", identifier: "run-execution_failure"))
    }

    @Test func readableUnscopedSubjectRendersOnlyOnce() {
        let item = AttentionFixtures.fixture(type: .system_health).item

        #expect(AttentionDisplay.subject(item) == .init(lead: "system", identifier: nil))
    }

    @Test func reviewRecoveryBindingRowsExposeEveryAuthorityCoordinate() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        #expect(
            AttentionDisplay.reviewRecoveryBindingRows(item) == [
                .init(label: "Recovery run", value: "run-review_contradiction"),
                .init(label: "Invocation", value: "review-run-review_contradiction-1"),
                .init(label: "Round", value: "1"),
                .init(label: "Base", value: "beefcafe"),
                .init(label: "Head", value: "cafebabe"),
                .init(
                    label: "Failure digest",
                    value: "sha256:failure-review_contradiction"
                ),
            ])
    }

    @Test func ordinaryItemsHaveNoReviewRecoveryBindingRows() {
        let item = AttentionFixtures.fixture(type: .review_dispute).item

        #expect(AttentionDisplay.reviewRecoveryBindingRows(item).isEmpty)
    }

    @Test func reviewConfigurationRecoveryRowsExposeEveryAuthorityCoordinate() {
        let item = AttentionFixtures.fixture(type: .review_configuration).item

        #expect(
            AttentionDisplay.reviewConfigurationRecoveryRows(item) == [
                .init(label: "Recovery run", value: "run-review_configuration"),
                .init(label: "Invocation", value: "review-run-review_configuration-2"),
                .init(label: "Round", value: "2"),
                .init(label: "Base", value: "beefcafe"),
                .init(label: "Head", value: "cafebabe"),
                .init(
                    label: "Failure digest",
                    value: "sha256:failure-review_configuration"
                ),
                .init(label: "Repository", value: "owner/repo"),
                .init(
                    label: "Superseded profile",
                    value: "sha256:profile-review_configuration"
                ),
            ])
    }

    @Test func ordinaryItemsHaveNoReviewConfigurationRecoveryRows() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        #expect(AttentionDisplay.reviewConfigurationRecoveryRows(item).isEmpty)
    }

    @Test func codexReenrollmentRecoveryRowsExposeEveryAuthorityCoordinate() {
        let item = AttentionFixtures.fixture(type: .system_health).item

        #expect(
            AttentionDisplay.codexReenrollmentRecoveryRows(item) == [
                .init(label: "Auth identity", value: "codex-primary"),
                .init(label: "Lease fence", value: "4"),
                .init(label: "Auth store digest", value: "sha256:replacement-store"),
                .init(label: "Token expires", value: "2026-08-12T02:44:05Z"),
            ])
    }

    @Test func ordinaryItemsHaveNoCodexReenrollmentRecoveryRows() {
        let item = AttentionFixtures.fixture(type: .review_contradiction).item

        #expect(AttentionDisplay.codexReenrollmentRecoveryRows(item).isEmpty)
    }
}
