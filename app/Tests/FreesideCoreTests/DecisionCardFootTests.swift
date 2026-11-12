import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The card's foot after #2607: Evidence above the control group, then one
/// hairline over Recorded Context and, on a card with no page to hand them
/// to, Identifiers.
@MainActor @Suite struct DecisionCardFootTests {
    /// Acceptance 1: on every type, a seated card draws Recorded Context
    /// alone under the hairline and a card with no page draws Recorded
    /// Context then Identifiers. Evidence sits above the control group
    /// exactly when the card has foot claims or attachments, and on an
    /// iPhone also when it has text claims a supporting module drew below
    /// the actions.
    @Test(arguments: AttentionFixtures.fixtureTypes)
    func theFootPlacesEachFoldByWhereTheCardSits(type: Components.Schemas.AttentionType) {
        let item = AttentionFixtures.fixture(type: type).item
        let composition = DecisionCardComposition.forType(type)
        let identifiers = DecisionDetailView.identifierRows(item, proposalFacts: nil, effectProposalFacts: nil)
        let recorded = Self.recordedContext(item)
        #expect(!recorded.parts.isEmpty)
        #expect(!identifiers.isEmpty)

        for platform in DecisionCardComposition.UnverifiedContext.Platform.allCases {
            let evidence = composition.evidenceClaims(
                from: item.agent_claims, prominentClaimIndex: nil, on: platform)
            func foot(onPage: Bool) -> DecisionCardFoot {
                DecisionCardFoot(
                    evidenceClaims: evidence, attachments: item.evidence_snapshot.count,
                    recordedContext: recorded, identifierRows: identifiers, onPage: onPage)
            }
            let seated = foot(onPage: true)
            let alone = foot(onPage: false)

            #expect(seated.underHairline == [.recordedContext], "\(platform)")
            #expect(alone.underHairline == [.recordedContext, .identifiers], "\(platform)")
            let hasEvidence = !evidence.isEmpty || !item.evidence_snapshot.isEmpty
            #expect(seated.aboveActions == (hasEvidence ? [.evidence] : []), "\(platform)")
            #expect(alone.aboveActions == seated.aboveActions, "\(platform)")
        }
    }

    /// An empty Recorded Context draws no fold, and a card with neither
    /// claims nor attachments draws no Evidence.
    @Test func anEmptyFoldIsNotDrawn() {
        let foot = DecisionCardFoot(
            evidenceClaims: [], attachments: 0, recordedContext: .init(),
            identifierRows: [.init(label: "Item Version", value: "1")], onPage: true)
        #expect(foot.aboveActions.isEmpty)
        #expect(foot.underHairline.isEmpty)
    }

    /// On an iPhone the Evidence fold takes the text claims a supporting
    /// claims module drew below the actions; the Mac lists them beside the
    /// actions instead, so its Evidence keeps attachments alone.
    @Test func anIPhoneEvidenceFoldTakesTheTextClaimsBelowTheActions() {
        var item = AttentionFixtures.fixture(type: .system_health).item
        item.agent_claims[0].text = .init(media_type: .text_sol_plain, content: "The daemon restarted cleanly.")
        let claim = item.agent_claims[0]
        let composition = DecisionCardComposition.forType(.system_health)

        #expect(
            composition.evidenceClaims(from: item.agent_claims, prominentClaimIndex: nil, on: .phone)
                .contains(claim))
        #expect(
            !composition.evidenceClaims(from: item.agent_claims, prominentClaimIndex: nil, on: .mac)
                .contains(claim))
    }

    /// Every claim a claims module or the foot would list renders once on an
    /// iPhone: in a claims module above the actions or in Evidence, never
    /// both and never neither.
    @Test(arguments: AttentionFixtures.fixtureTypes)
    func everyClaimRendersOnceOnAPhoneCard(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        let claims = AttentionFixtures.fixture(type: type).item.agent_claims
        let claimModules = composition.modules.indices.filter { composition.modules[$0] == .claims }
        let listed =
            claimModules.flatMap { composition.claims(from: claims, at: $0, prominentClaimIndex: nil) }
            + composition.footClaims(from: claims, prominentClaimIndex: nil)
        let lead = claimModules.filter(composition.claimsAreProminent(at:)).flatMap {
            composition.claimsInCard(from: claims, at: $0, prominentClaimIndex: nil, on: .phone)
        }
        let evidence = composition.evidenceClaims(from: claims, prominentClaimIndex: nil, on: .phone)

        for claim in listed {
            #expect((lead + evidence).filter { $0 == claim }.count == 1, "\(claim.label)")
        }
    }

    /// The closed Recorded Context names what it holds in drawing order.
    @Test func recordedContextNamesItsPartsInDrawingOrder() {
        let full = DecisionRecordedContext(
            hasReason: true, hasRunDetails: true, hasReviewYield: true,
            readingRows: [
                .init(label: "Requested, Not Available Here", value: "Convert to Policy"),
                .init(label: "Review Round 1", value: "2 blockers"),
                .init(label: "Waiver 1", value: "w"),
                .init(label: "Readiness", value: "Clean"),
                .init(label: "Posture", value: "Healthy"),
            ],
            hasFactBlock: true, hasAgentSummary: true, hasReport: true)

        #expect(
            full.summary
                == "reason · run details · review yield · posture · readiness · review rounds"
                + " · not offered here · facts · agent summary · report")
        #expect(DecisionRecordedContext().parts.isEmpty)
        #expect(DecisionRecordedContext(hasReason: true).summary == "reason")
    }

    /// Acceptance 4: the diminishing-returns card's reason, which its face
    /// leaves to the stop cause, and its "Requested, Not Available Here"
    /// row both read in Recorded Context, and neither is an identifier.
    @Test func diminishingReturnsReadsItsReasonAndUnofferedActionInRecordedContext() throws {
        let item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        let composition = DecisionCardComposition.forType(.review_diminishing_returns)
        let recorded = Self.recordedContext(item)
        let identifiers = DecisionDetailView.identifierRows(item, proposalFacts: nil, effectProposalFacts: nil)

        #expect(DecisionCardComposition.reason(for: item) != nil)
        #expect(!composition.drawsReason(for: item))
        #expect(
            recorded.readingRows.contains(
                .init(label: "Requested, Not Available Here", value: "Convert to Policy")))
        #expect(recorded.parts.starts(with: ["reason"]))
        #expect(recorded.parts.contains("not offered here"))
        #expect(recorded.parts.contains("review rounds"))
        #expect(!identifiers.contains { $0.label == "Requested, Not Available Here" })
    }

    /// Acceptance 3: the eyebrow's Copy menu offers the item ID, then the
    /// evidence digest: none without one, a single item with one, and a
    /// single item carrying each digest when there are two.
    @Test func eyebrowCopyItemsFollowTheEvidenceDigests() throws {
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        let first = try #require(item.evidence_snapshot.first)
        var second = first
        second.digest = "sha256:second"

        item.evidence_snapshot = []
        #expect(DecisionDetailView.eyebrowCopyItems(item) == [.init(title: "Copy item ID", values: [item.id])])

        item.evidence_snapshot = [first, first]
        #expect(
            DecisionDetailView.eyebrowCopyItems(item) == [
                .init(title: "Copy item ID", values: [item.id]),
                .init(title: "Copy evidence digest", values: [first.digest]),
            ])

        item.evidence_snapshot = [first, second]
        #expect(
            DecisionDetailView.eyebrowCopyItems(item) == [
                .init(title: "Copy item ID", values: [item.id]),
                .init(title: "Copy evidence digest", values: [first.digest, second.digest]),
            ])
    }

    /// The rows an effect proposal's fetched facts add (the closure's bound
    /// merge, a follow-up filing, an image policy adoption) are values a
    /// reader copies, so they join Identifiers and never Recorded Context.
    @Test func effectProposalFactRowsAreIdentifiers() throws {
        let snapshots = [
            AttentionFixtures.recommendedEffectProposal(),
            AttentionFixtures.followUpFilingEffectProposal(),
            AttentionFixtures.imagePolicyAdoptionEffectProposal(),
        ]
        for snapshot in snapshots {
            let item = snapshot.item
            let facts = try #require(AttentionFixtures.effectProposalFacts(for: snapshot))
            let bare = DecisionDetailView.identifierRows(item, proposalFacts: nil, effectProposalFacts: nil)
            let withFacts = DecisionDetailView.identifierRows(item, proposalFacts: nil, effectProposalFacts: facts)
            let reading = DecisionDetailView.readingRows(item, unavailable: [])
            let added = withFacts.filter { !bare.contains($0) }

            #expect(!added.isEmpty, "\(item.id)")
            #expect(Set(added.map(\.label)).isDisjoint(with: reading.map(\.label)), "\(item.id)")
        }
    }

    /// The Recorded Context the view builds for a fixture, from the parts
    /// that need no graphics: the reason and the reading rows. The reason
    /// alone makes it non-empty on every type that records one.
    private static func recordedContext(_ item: Components.Schemas.AttentionItem) -> DecisionRecordedContext {
        DecisionRecordedContext(
            hasReason: DecisionCardComposition.reason(for: item) != nil,
            readingRows: DecisionDetailView.readingRows(
                item, unavailable: DecisionActionRanking(requested: item.requested_decision).unavailable))
    }
}
