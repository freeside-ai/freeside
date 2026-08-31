import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct SpecApprovalReaderTests {
    @Test func unifiedDiffSeparatesKindsAndHunks() {
        let unified =
            "@@ -1,2 +1,2 @@\n-old\n+new\n same\n@@ -8,1 +8,2 @@\n keep\n+added"
        let hunks = UnifiedDiffView.parse(unified)

        #expect(hunks.count == 2)
        #expect(hunks.map(\.header) == ["@@ -1,2 +1,2 @@", "@@ -8,1 +8,2 @@"])
        #expect(
            hunks[0].lines.map(\.kind) == [.hunk, .removal, .addition, .context])
        #expect(hunks[1].lines.map(\.kind) == [.hunk, .context, .addition])
        #expect(
            UnifiedDiffView.truncationMessage
                == "This unified diff is truncated. The line counts cover the full revision.")
    }

    @Test func specificationIsReservedApprovalMaterial() throws {
        let item = AttentionFixtures.revisedSpecification().item
        let composition = DecisionCardComposition.forType(.spec_approval)
        let claimsIndex = try #require(composition.modules.firstIndex(of: .claims))

        #expect(DecisionDetailView.specificationClaim(in: item)?.text != nil)
        #expect(
            composition.claims(
                from: item.agent_claims,
                at: claimsIndex,
                prominentClaimIndex: nil
            ).allSatisfy { !AgentClaimLabels.isApprovalMaterial($0.label) })
    }

    @Test func predecessorLabelUsesTheAuthenticatedPriorApprovalIteration() throws {
        var item = AttentionFixtures.revisedSpecification().item
        var revision = try #require(item.spec_revision?.value1)
        revision.iteration = 3
        item.spec_revision = .init(value1: revision)

        #expect(DecisionDetailView.priorSpecRevisionIteration(in: item) == 1)

        let initial = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(DecisionDetailView.priorSpecRevisionIteration(in: initial) == nil)
    }

    @Test func initialSpecificationIterationUsesAuthenticatedArtifactCoordinate() throws {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let specificationIndex = try #require(
            item.agent_claims.firstIndex { $0.label == AgentClaimLabels.specification })
        item.agent_claims[specificationIndex].artifact_id = "spec-implementation-run-2"

        #expect(DecisionDetailView.specificationRevisionIteration(in: item) == 2)

        item.agent_claims[specificationIndex].artifact_id = "legacy-specification"
        #expect(DecisionDetailView.specificationRevisionIteration(in: item) == nil)
    }

    @Test func legacyAttachmentBackedSpecificationRemainsApprovalMaterial() throws {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let specificationIndex = try #require(
            item.agent_claims.firstIndex { $0.label == AgentClaimLabels.specification })
        item.agent_claims[specificationIndex].text = nil

        let specification = try #require(DecisionDetailView.specificationClaim(in: item))
        #expect(specification.text == nil)

        let composition = DecisionCardComposition.forType(.spec_approval)
        let claimsIndex = try #require(composition.modules.firstIndex(of: .claims))
        #expect(
            composition.claims(
                from: item.agent_claims,
                at: claimsIndex,
                prominentClaimIndex: nil
            ).allSatisfy { $0.label != AgentClaimLabels.specification })
    }
}
