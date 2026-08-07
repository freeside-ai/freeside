import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct AttentionDisplayTests {
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
}
