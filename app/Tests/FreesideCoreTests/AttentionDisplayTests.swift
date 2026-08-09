import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct AttentionDisplayTests {
    @Test func healthPostureLabelsAreExplicit() {
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.blocking) == "Blocking")
        #expect(AttentionDisplay.label(Components.Schemas.HealthPosture.advisory) == "Advisory")
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
}
