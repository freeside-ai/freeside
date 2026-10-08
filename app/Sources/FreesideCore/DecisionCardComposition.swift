import FreesideAPI
import SwiftUI

enum DecisionCardModule: String, CaseIterable {
    case facts
    /// The routine facts a card folds (`DecisionFactPlacement.folded`), as
    /// a module of their own so a composition can place the fold under its
    /// actions while `.facts` stays ahead of them.
    case foldedFacts
    case agentQuestion
    case specRevision
    case specification
    case factBlock
    case findingFacts
    case recommendation
    case stopCause
    case checklist
    case stageRail
    case comparison
    case yieldChart
    case summary
    case claims
    case evidence
    case details
}

// Keep in sync with export.SummaryEvidenceLabel in
// daemon/internal/export/evidence_source.go.
enum AgentClaimLabels {
    static let summary = "freeside.summary"
    static let addressals = "Addressals"
    static let specification = "Specification"

    static func isApprovalMaterial(_ label: String) -> Bool {
        label == addressals || label == specification
    }
}

struct DecisionCardComposition: Equatable {
    let modules: [DecisionCardModule]
    let actionInsertionIndex: Int
    let reviewingActionInsertionIndex: Int?
    /// Whether the agent's claim is this card's own lead content rather
    /// than support for another module. Such a card draws its leading claims
    /// module in the card on every platform, and leads with its claims even
    /// when none carries inline text, so the lead is never empty while a
    /// claim exists. Elsewhere a claim without text stays supporting
    /// context.
    var leadsWithItsClaim = false
    /// Whether the card's frame places the agent's readable claims inside
    /// its own order, ahead of the actions, without making them its lead
    /// (frame 5.3: the diagnostic between the failure's facts and its
    /// stages). Such a card draws the claims an operator can read in the
    /// card on every platform; a claim without text stays supporting
    /// context below the actions.
    var placesReadableClaimsInCard = false
    /// Whether the leading claims module draws in the card itself on every
    /// platform, so no other place a platform lists claims repeats it.
    var drawsLeadClaimsInCard: Bool { leadsWithItsClaim || placesReadableClaimsInCard }
    /// How many modules directly after the action region are closed folds.
    /// They draw with Recorded Context under the card's one hairline rather
    /// than as sections of their own.
    var foldedModuleCount = 0

    /// A claim module leads when it renders above the action region: that is
    /// the whole meaning of prominence here, so it is read from
    /// `actionInsertionIndex` rather than from another module's position.
    func claimsAreProminent(at moduleIndex: Int) -> Bool {
        guard let claimsIndex = modules.firstIndex(of: .claims) else { return false }
        return moduleIndex == claimsIndex && claimsIndex < actionInsertionIndex
    }

    func claims(
        from claims: [Components.Schemas.AgentClaim],
        at moduleIndex: Int,
        prominentClaimIndex: Int?
    ) -> [Components.Schemas.AgentClaim] {
        let claims = claims.filter {
            !($0.label == AgentClaimLabels.summary && $0.text != nil)
                && !AgentClaimLabels.isApprovalMaterial($0.label)
        }
        let claimModuleIndices = modules.indices.filter { modules[$0] == .claims }
        guard claimModuleIndices.count > 1 else { return claims }
        let leads = moduleIndex == claimModuleIndices.first
        guard let prominentClaimIndex, claims.indices.contains(prominentClaimIndex) else {
            // Without a caller-chosen prominent claim, the claim the operator
            // can read leads and an attachment stays supporting context
            // (plan §9). This is the split the macOS action region already
            // applies, where text claims sit above the actions and attachment
            // claims move to the inspector.
            if leadsWithItsClaim, !claims.contains(where: { $0.text != nil }) {
                return leads ? claims : []
            }
            return claims.filter { ($0.text != nil) == leads }
        }
        return claims.enumerated().compactMap { index, claim in
            leads == (index == prominentClaimIndex) ? claim : nil
        }
    }

    /// The claims a card that leads with its claim draws in the card itself,
    /// which every other place a platform lists claims has to leave out so
    /// the claim renders once. Empty for a card that does not lead with one.
    func cardLeadClaims(
        from claims: [Components.Schemas.AgentClaim],
        prominentClaimIndex: Int?
    ) -> [Components.Schemas.AgentClaim] {
        guard drawsLeadClaimsInCard, let lead = modules.firstIndex(of: .claims),
            claimsAreProminent(at: lead)
        else { return [] }
        return self.claims(from: claims, at: lead, prominentClaimIndex: prominentClaimIndex)
    }

    func summaries(
        from claims: [Components.Schemas.AgentClaim]
    ) -> [Components.Schemas.AgentClaim] {
        claims.filter { $0.label == AgentClaimLabels.summary && $0.text != nil }
    }

    /// Whether this type's `reason` carries the agent's own summary rather
    /// than a daemon-authored context fact. `acceptSpecification` sets a
    /// specification approval's `reason` from the agent's summary and builds
    /// its `freeside.summary` claim from the same bytes, so drawing the
    /// reason too would repeat the labeled claim stripped of its unverified
    /// register (#1098). The rule is per type, never a comparison of the two strings.
    /// The switch is exhaustive so a new type has to answer the question.
    static func reasonIsAgentSummary(_ type: Components.Schemas.AttentionType) -> Bool {
        switch type {
        case .spec_approval:
            return true
        case .execution_failure, .agent_question, .review_diminishing_returns, .review_dispute,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .ready_for_final_review, .publish_blocked, .task_proposal, .effect_proposal,
            .system_health, .blocked:
            return false
        }
    }

    /// Whether this type's `reason` is the agent's own prose rather than a
    /// sentence the daemon composed. A specification approval's reason is
    /// the agent's summary (`reasonIsAgentSummary`), and a question's reason
    /// is the asking invocation's statement of why it stopped. Plan §9's
    /// Summary Provenance has agent prose labeled wherever it draws, so the
    /// shell quotes such a reason under its producer label. The switch is
    /// exhaustive so a new type has to answer the question.
    static func reasonIsAgentWritten(_ type: Components.Schemas.AttentionType) -> Bool {
        switch type {
        case .spec_approval, .agent_question:
            return true
        case .execution_failure, .review_diminishing_returns, .review_dispute,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .ready_for_final_review, .publish_blocked, .task_proposal, .effect_proposal,
            .system_health, .blocked:
            return false
        }
    }

    /// An item's `reason` as the card draws it, wherever it draws it.
    struct Reason: Equatable {
        let text: String
        /// An agent wrote it, so it draws quoted under the unverified label.
        let isAgentWritten: Bool

        var label: String { isAgentWritten ? "Agent reason" : "Reason" }
    }

    /// The reason the card's Details carry in full on every type (plan §9
    /// revision 82), including one the card draws nowhere else, such as the
    /// diminishing-returns `Binding:` line. Nil only when the daemon recorded
    /// no reason.
    static func reason(for item: Components.Schemas.AttentionItem) -> Reason? {
        guard !item.reason.isEmpty else { return nil }
        return Reason(text: item.reason, isAgentWritten: reasonIsAgentWritten(item._type))
    }

    /// Whether the card draws `item`'s reason outside Details. The reason is
    /// dropped only when the type's `reason` is the agent's summary *and*
    /// that summary has a claim to render under. A specification approval
    /// persisted before summary claims carries its `Specification` claim
    /// alone, a shape `verifySpecificationApprovalClaims` in
    /// daemon/internal/engine/specification.go still accepts, and dropping
    /// the reason there would take it off the card's face entirely.
    ///
    /// A diminishing-returns item that carries typed stop-cause facts also
    /// drops it: the daemon writes that `reason` as a summary line followed by
    /// a `Binding: {…}` JSON line (`ReviewDiminishingReason` in
    /// daemon/internal/store/review_diminishing.go), and the `.stopCause`
    /// module states the cause from the typed field instead. An item without
    /// the facts, stored before they existed or raised by a review
    /// escalation, has no other statement of its cause and keeps its reason.
    ///
    /// Details carries the reason in full on every type either way (plan §9
    /// revision 82), so a reason this drops is still one disclosure away.
    func drawsReason(for item: Components.Schemas.AttentionItem) -> Bool {
        if item._type == .review_diminishing_returns, item.review_diminishing != nil {
            return false
        }
        return !Self.reasonIsAgentSummary(item._type) || summaries(from: item.agent_claims).isEmpty
    }

    /// How a card frames its agent-written sections. The unverified label
    /// names the register in both; the frame is only how the section is set
    /// apart from its neighbors.
    enum AgentSectionFrame: Equatable {
        /// A dashed card around the section, with every claim's source
        /// identifiers printed beside its text.
        case dashedCard
        /// The label and then the agent's prose in a `QuoteBlock` (R5), with
        /// the source identifiers one disclosure away.
        case quoted
    }

    /// The visual audit keeps a bounded card for an independent item or
    /// option and separates ordinary sections by spacing, on the surfaces it
    /// approved only. The question card (D06) draws its options as the
    /// bounded panels, the final review (D07) keeps its one card for the
    /// daemon's checklist, and the dispute (D08) reads its claim as prose
    /// beside the actions, so their agent sections drop their own card and
    /// quote the agent instead. Every other type keeps the dashed card until
    /// its own sweep. The switch is exhaustive so a new type has to answer
    /// the question.
    static func agentSectionFrame(
        for type: Components.Schemas.AttentionType
    ) -> AgentSectionFrame {
        switch type {
        case .agent_question, .ready_for_final_review, .review_dispute, .system_health, .blocked,
            .execution_failure, .task_proposal, .effect_proposal, .review_diminishing_returns:
            return .quoted
        case .spec_approval,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .publish_blocked:
            return .dashedCard
        }
    }

    /// A card's gap ladder, padding, and corner (R10). The refined ladder is
    /// the one survey card 4b settled: 22 between sections, 11 inside a
    /// module, 10 within a control group, and 18 above the folds, which sit
    /// under the card's one hairline.
    enum Scale: Equatable {
        case legacy
        case refined

        var sectionGap: CGFloat { self == .refined ? 22 : 16 }
        /// Between the eyebrow and the ask.
        var headGap: CGFloat { self == .refined ? 12 : 16 }
        var moduleGap: CGFloat { self == .refined ? 11 : 8 }
        var controlGap: CGFloat { self == .refined ? 10 : 8 }
        var foldGap: CGFloat { self == .refined ? 12 : 16 }
        /// The space between the hairline and the first fold.
        var foldLead: CGFloat { 18 }
        var drawsFoldHairline: Bool { self == .refined }
        /// Returning the work is a plain outlined command on the refined
        /// card (R6).
        var drawsReturnGlyph: Bool { self == .legacy }
        /// The overflow trigger's words, in Title Case on the refined card
        /// (R30).
        var overflowLabel: String { self == .refined ? "More Actions" : "More actions" }
        var cornerRadius: CGFloat { self == .refined ? 12 : 8 }
        /// The widest a one-column card grows with the detail's 16pt margin
        /// around it: the refined card itself is 560 wide.
        var columnWidth: CGFloat { self == .refined ? 592 : 560 }

        /// A phone's card is 20 from each side and 18 from the top and
        /// bottom; a Mac's sits 28 in, with 24 under its last line.
        func padding(compact: Bool) -> EdgeInsets {
            switch self {
            case .legacy:
                EdgeInsets(top: 14, leading: 14, bottom: 14, trailing: 14)
            case .refined:
                compact
                    ? EdgeInsets(top: 18, leading: 20, bottom: 18, trailing: 20)
                    : EdgeInsets(top: 28, leading: 28, bottom: 24, trailing: 28)
            }
        }
    }

    /// The final review is the card the refined ladder was proved on, and
    /// the agent question the second composed on it; every other type keeps
    /// the earlier one until its own sweep composes it. The switch is
    /// exhaustive so a new type has to answer the question.
    static func scale(for type: Components.Schemas.AttentionType) -> Scale {
        switch type {
        case .ready_for_final_review, .agent_question, .system_health, .blocked,
            .execution_failure, .task_proposal, .effect_proposal, .review_diminishing_returns:
            return .refined
        case .review_dispute, .spec_approval,
            .review_contradiction, .review_configuration,
            .finding_adjudication, .publish_blocked:
            return .legacy
        }
    }

    /// The keyword over a card's typed fact rows (R1). A card whose rows
    /// all describe one thing names that thing; the rest say `Facts`. The
    /// switch is exhaustive so a new type has to answer the question.
    static func factsKeyword(for type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .execution_failure:
            return "Failure"
        case .spec_approval, .agent_question, .review_diminishing_returns, .review_dispute,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .ready_for_final_review, .publish_blocked, .task_proposal, .effect_proposal,
            .system_health, .blocked:
            return "Facts"
        }
    }

    /// The keyword over the claims a card leads with, where the frame names
    /// what the agent is claiming rather than that it is a claim (R5). The
    /// unverified register is drawn beside it, never spelled in it. The
    /// switch is exhaustive so a new type has to answer the question.
    static func leadClaimsKeyword(for type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .execution_failure:
            return "Diagnostic"
        case .task_proposal:
            return "Proposal"
        case .spec_approval, .agent_question, .review_diminishing_returns, .review_dispute,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .ready_for_final_review, .publish_blocked, .effect_proposal,
            .system_health, .blocked:
            return "Agent claims"
        }
    }

    /// Whether the shell draws its generic ask for `item`. A question card
    /// leads with the agent's own question (D06), so the ask would only
    /// delay it; an item that carries no typed decision draws no lead and
    /// keeps the ask, so a card is never left without one.
    static func rendersAsk(for item: Components.Schemas.AttentionItem) -> Bool {
        guard let question = AgentQuestionPresentation(item) else { return true }
        return question.decisions.isEmpty
    }

    /// The type eyebrow every card opens with (R27).
    struct Eyebrow: Equatable {
        /// The type's name, which the inbox row carries too.
        let keyword: String
        /// Whether the eyebrow carries the unverified register and the
        /// card's one explanation control, which it does when the card's
        /// lead is the agent's own prose.
        let carriesInfo: Bool
    }

    /// A question that leads with its typed decisions leads with the asking
    /// agent's words, so its eyebrow says so. Every other card leads with a
    /// daemon-written ask.
    static func eyebrow(for item: Components.Schemas.AttentionItem) -> Eyebrow {
        Eyebrow(
            keyword: AttentionDisplay.title(item._type),
            carriesInfo: item._type == .agent_question && !rendersAsk(for: item))
    }

    /// Where the card shell draws the `reason` ahead of Details. The reason
    /// is drawn by the shell, not by a module, so its place is a rule of the
    /// type rather than a position in `modules`.
    enum ReasonPlacement: Equatable {
        /// Directly under the ask, ahead of the actions.
        case underAsk
        /// A closed "Recorded Context" disclosure below the actions.
        case recordedContext
        /// Off the card's face: Details alone carry it, as they do on every
        /// type.
        case detailsOnly
    }

    /// How the shell sets a reason it draws under the ask.
    enum ReasonFace: Equatable {
        /// The ask's dim second line (R0).
        case secondLine
        /// The card's own statement, in the serif face and ink.
        case statement
    }

    /// A system-health item's reason is the daemon's finding: the sentence
    /// that says what is wrong, which its diagnostic code and impaired
    /// capability only classify. It is what the card is about, so it reads
    /// as the card's statement (survey frame 5.5) and never folds. Every
    /// other reason the shell draws is context for the ask above it. The
    /// switch is exhaustive so a new type has to answer the question.
    static func reasonFace(for type: Components.Schemas.AttentionType) -> ReasonFace {
        switch type {
        case .system_health:
            return .statement
        case .spec_approval, .execution_failure, .agent_question, .review_diminishing_returns,
            .review_dispute, .review_contradiction, .review_configuration,
            .finding_adjudication, .ready_for_final_review, .publish_blocked, .task_proposal,
            .effect_proposal, .blocked:
            return .secondLine
        }
    }

    /// Plan §9 (revision 82) places the reason by a per-item test: it folds
    /// only where the card's lead already states it. The question, the final
    /// review, and the finding cards lead with their own module and have that
    /// test (`reasonPlacement(for item:)`), so they keep the recorded
    /// sentence one disclosure away (D06, D07, D09). A proposal's reason
    /// restates its ask at the planned gate and folds there. A blocked card leads
    /// with the wait its typed facts name, so its reason leaves the face
    /// under the same kind of test. Every other type draws it under the ask
    /// (R0): no type has a boxed Context section, and no type folds its
    /// reason without a test that says its lead covers it. The switch is
    /// exhaustive so a new type has to answer the question.
    static func reasonPlacement(
        for type: Components.Schemas.AttentionType
    ) -> ReasonPlacement {
        switch type {
        case .agent_question, .ready_for_final_review, .finding_adjudication, .task_proposal,
            .effect_proposal:
            return .recordedContext
        case .blocked:
            return .detailsOnly
        case .review_dispute, .spec_approval, .execution_failure, .review_diminishing_returns,
            .review_contradiction, .review_configuration, .publish_blocked, .system_health:
            return .underAsk
        }
    }

    /// Where the shell draws the reason of this `item`. A question that
    /// carries no typed decision draws no lead and keeps the generic ask
    /// (`rendersAsk(for:)`), so its reason stays under that ask rather than
    /// folding away from a card with nothing else to read first.
    ///
    /// A finding adjudication's reason says what accepting does
    /// (`findingAdjudicationReason` in
    /// daemon/internal/engine/finding_adjudication.go). The recommendation
    /// states the same outcome finding by finding beside the batch action,
    /// so the reason may fold while that is on the card. An item whose
    /// recommendation did not revalidate has no other statement of it, and
    /// an action's consequence never folds (plan §9), so its reason stays
    /// under the ask.
    ///
    /// A blocked item's reason says what the run waits on and since when
    /// (`ensureSpecificationBlockedItem` in
    /// daemon/internal/engine/specification.go, the one writer). The card's
    /// lead names that wait and its Waiting fact gives the duration, with
    /// the exact start in Details, so the reason leaves the face. That holds
    /// only for the wait the daemon writes today: an item with no typed
    /// wait keeps the generic ask, and no writer yet says what a wait on PR
    /// checks or an external review records, so both keep the reason under
    /// the ask.
    ///
    /// A proposal opened at its planned gate carries a reason that only
    /// restates the ask: "Start the daemon-enumerated work subject"
    /// (daemon/internal/engine/proposal_admission.go), "Decide the proposed
    /// effect on the source issue" and "Decide whether to file the follow-up
    /// issue" (daemon/internal/signet/effect_proposal.go), and their revised
    /// forms (daemon/internal/signet/proposal.go), whose revision the card's
    /// own rows state. The one proposal reason that says more is the
    /// closure notice, that the source issue could not be closed
    /// automatically and that the notice does not hold the pull request; the
    /// daemon opens that item as exceptional, and only that one. So a
    /// proposal's reason folds at the planned gate and stays ahead of the
    /// actions on any other class.
    static func reasonPlacement(
        for item: Components.Schemas.AttentionItem
    ) -> ReasonPlacement {
        if item._type == .task_proposal || item._type == .effect_proposal,
            item.interruption_class != .planned_gate
        {
            return .underAsk
        }
        if item._type == .agent_question, rendersAsk(for: item) { return .underAsk }
        if item._type == .blocked, item.blocked_on?.value1.kind != .spec_approval {
            return .underAsk
        }
        if item._type == .finding_adjudication,
            DecisionRecommendationPresentation.of(item) == nil
        {
            return .underAsk
        }
        return reasonPlacement(for: item._type)
    }

    /// A place a card draws an unverified keyword the operator can always
    /// see: one that is not folded away and is not a disclosure's own label.
    enum UnverifiedSlot: Hashable {
        /// The type eyebrow.
        case eyebrow
        /// The agent-written reason under the ask.
        case reason
        /// A module, by its index in `modules`.
        case module(Int)
        /// The macOS action region: the recommendation, then the claims.
        case actionRegion
    }

    /// What the view knows about a card that its item does not say. The
    /// slots are computed from this rather than read back from the view, so
    /// the rule is testable without rendering.
    struct UnverifiedContext: Equatable {
        enum Platform: CaseIterable {
            /// Claims and the recommendation draw in the action region and
            /// the inspector.
            case mac
            /// Every module draws in the card.
            case phone
        }

        static var currentPlatform: Platform {
            #if os(macOS)
                .mac
            #else
                .phone
            #endif
        }

        var platform: Platform = currentPlatform
        /// At an accessibility size a supporting section is a disclosure,
        /// so its keyword is that disclosure's label.
        var accessibilityLayout = false
        /// The recommendation block draws, in the agent-claim register.
        var drawsUnverifiedRecommendation = false
        /// The card has a change summary to draw in its fact block.
        var hasChangeSummary = false
        /// The card draws a follow-up filing's proposed title and body.
        var hasProposedIssueText = false
        var prominentClaimIndex: Int? = nil
    }

    /// Every slot that draws an unverified keyword for `item`, in reading
    /// order. The first carries the card's one explanation control (R25);
    /// the rest draw the keyword and its register alone. An empty list with
    /// unverified content on the card means every such keyword is a
    /// disclosure label, and the section explains itself when opened.
    func unverifiedSlots(
        for item: Components.Schemas.AttentionItem,
        in context: UnverifiedContext
    ) -> [UnverifiedSlot] {
        var slots: [UnverifiedSlot] = []
        if Self.eyebrow(for: item).carriesInfo { slots.append(.eyebrow) }
        if drawsReason(for: item), Self.reasonPlacement(for: item) == .underAsk,
            Self.reasonIsAgentWritten(item._type), !item.reason.isEmpty
        {
            slots.append(.reason)
        }
        for (index, module) in modules.enumerated() {
            if index == actionInsertionIndex, drawsUnverifiedInActionRegion(item, in: context) {
                slots.append(.actionRegion)
            }
            if drawsUnverifiedKeyword(module, at: index, for: item, in: context) {
                slots.append(.module(index))
            }
        }
        if actionInsertionIndex >= modules.count, drawsUnverifiedInActionRegion(item, in: context) {
            slots.append(.actionRegion)
        }
        return slots
    }

    /// The slot whose keyword carries the card's explanation control.
    func infoSlot(
        for item: Components.Schemas.AttentionItem,
        in context: UnverifiedContext
    ) -> UnverifiedSlot? {
        unverifiedSlots(for: item, in: context).first
    }

    private func drawsUnverifiedInActionRegion(
        _ item: Components.Schemas.AttentionItem,
        in context: UnverifiedContext
    ) -> Bool {
        guard context.platform == .mac else { return false }
        if context.drawsUnverifiedRecommendation { return true }
        return !drawsLeadClaimsInCard && !Self.actionRegionClaims(item.agent_claims).isEmpty
    }

    /// The claims the macOS action region lists beside the actions: the ones
    /// an operator can read there, less the summary and the approval
    /// material, which have their own modules.
    static func actionRegionClaims(
        _ claims: [Components.Schemas.AgentClaim]
    ) -> [Components.Schemas.AgentClaim] {
        claims.filter {
            $0.text != nil && $0.label != AgentClaimLabels.summary
                && !AgentClaimLabels.isApprovalMaterial($0.label)
        }
    }

    /// Whether a claims module draws in the card at all. macOS lists claims
    /// in the action region and the inspector, so a claims module draws in
    /// the card there only where the claim is the card's own lead (D08).
    func drawsClaimsInCard(at moduleIndex: Int, on platform: UnverifiedContext.Platform) -> Bool {
        switch platform {
        case .mac: drawsLeadClaimsInCard && claimsAreProminent(at: moduleIndex)
        case .phone: true
        }
    }

    private func drawsUnverifiedKeyword(
        _ module: DecisionCardModule,
        at index: Int,
        for item: Components.Schemas.AttentionItem,
        in context: UnverifiedContext
    ) -> Bool {
        switch module {
        case .facts:
            return context.hasProposedIssueText
        case .agentQuestion:
            // The eyebrow labels the first question; a later one repeats
            // the label.
            return (AgentQuestionPresentation(item)?.decisions.count ?? 0) > 1
        case .recommendation:
            return context.platform == .phone && context.drawsUnverifiedRecommendation
        case .findingFacts:
            guard let binding = item.finding_adjudication?.value1 else { return false }
            return FindingCardPresentation.cards(binding)
                .contains { $0.producerUnverifiedKeyword != nil }
        case .factBlock:
            return context.hasChangeSummary
        case .summary:
            // The final review draws its summary section even with no inline
            // text, to say the summary is unavailable.
            return item._type == .ready_for_final_review
                || !summaries(from: item.agent_claims).isEmpty
        case .claims:
            guard drawsClaimsInCard(at: index, on: context.platform) else { return false }
            let drawn = claims(
                from: item.agent_claims, at: index,
                prominentClaimIndex: context.prominentClaimIndex)
            // A supporting claims section is a disclosure at an accessibility
            // size, and its keyword is that disclosure's label.
            return !drawn.isEmpty && (claimsAreProminent(at: index) || !context.accessibilityLayout)
        case .details:
            // A phone draws Details open in the card at an ordinary size, so
            // an agent-written reason there shows its keyword.
            return context.platform == .phone && !context.accessibilityLayout
                && Self.reason(for: item)?.isAgentWritten == true
        case .stopCause:
            // The drift audit's explanation and fixes are the audit model's
            // words, drawn under an unverified keyword (frame 5.2).
            return DecisionStopCausePresentation(item)?.audit != nil
        case .foldedFacts, .specRevision, .specification, .checklist, .stageRail,
            .comparison, .yieldChart, .evidence:
            return false
        }
    }

    /// The one action a card's row draws filled, or none (R6). A
    /// recommendation the card draws holds the fill in its own block, so the
    /// row fills nothing. Without one the fill goes to the type's forward
    /// action, where the row offers it and it needs no confirmation: a
    /// destructive action is never filled. `requested_decision` may repeat
    /// an action, so the row fills only the first and no card draws two.
    static func filledAction(
        for item: Components.Schemas.AttentionItem, ranking: DecisionActionRanking
    ) -> Components.Schemas.Action? {
        guard ranking.recommended == nil, let forward = forwardAction(for: item._type),
            ranking.principal.contains(forward) || ranking.reviewing == forward,
            AttentionDisplay.confirmationConsequence(forward, for: item) == nil
        else { return nil }
        return forward
    }

    /// The type's one forward action, as R6 lists them: the supported next
    /// step a card with no recommendation still points at. A type whose
    /// choices are peers (approve or request changes, one route or another)
    /// has none, and neither do the three types with no frame. The switch is
    /// exhaustive so a new type has to answer the question.
    static func forwardAction(
        for type: Components.Schemas.AttentionType
    ) -> Components.Schemas.Action? {
        switch type {
        case .agent_question: return .answer_and_retry
        case .ready_for_final_review: return .open_pr
        case .execution_failure: return .retry
        case .task_proposal: return .start
        case .effect_proposal: return .approve
        case .system_health: return .acknowledge
        case .spec_approval, .review_dispute, .review_diminishing_returns,
            .finding_adjudication, .review_contradiction, .review_configuration,
            .publish_blocked, .blocked:
            return nil
        }
    }

    /// The actions a type moves under More Actions beyond the set every
    /// card shares. Spec approval's conversation is where a reply starts
    /// (frame 5.1), so Discuss leaves its row.
    static func overflowActions(
        for type: Components.Schemas.AttentionType
    ) -> Set<Components.Schemas.Action> {
        type == .spec_approval ? [.discuss] : []
    }

    /// Whether the type's `.facts` rows are routine run and binding
    /// coordinates that fold into a closed disclosure (D06, D08). The final
    /// review's only row is its diff, which plan §9 lists with the verdicts,
    /// so it stays visible. The switch is exhaustive so a new type has to
    /// answer the question.
    static func foldsRoutineFacts(_ type: Components.Schemas.AttentionType) -> Bool {
        switch type {
        case .agent_question, .review_dispute:
            return true
        case .spec_approval, .execution_failure, .review_diminishing_returns,
            .review_contradiction, .review_configuration, .finding_adjudication,
            .ready_for_final_review, .publish_blocked, .task_proposal, .effect_proposal,
            .system_health, .blocked:
            return false
        }
    }

    static let sharedModuleSet = DecisionCardModule.allCases

    /// Every composition places `.facts` ahead of `actionInsertionIndex`: the
    /// daemon's typed facts inform the decision, so they can never render
    /// below the actions. Where each type puts them among its own modules is
    /// that type's judgement, not a shared rule, so a card leads with the
    /// module its §9 row leads with (the readiness checklist, the stage rail,
    /// the disputed positions) and keeps the identifier-shaped facts last.
    static func forType(_ type: Components.Schemas.AttentionType) -> Self {
        switch type {
        case .ready_for_final_review:
            // Plan §9 (revision 78, audit D07) and survey card 4b: what the
            // agent says changed, then the diff and the daemon's verdict on
            // it, then View PR, so the supported next step follows what it
            // rests on. Returning the work sits below any fact block; the
            // review's round-by-round yield is history, so it folds under
            // the actions.
            return .init(
                modules: [
                    .recommendation, .summary, .facts, .checklist, .factBlock, .yieldChart,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 5,
                reviewingActionInsertionIndex: 4,
                foldedModuleCount: 1)
        case .execution_failure:
            // Frame 5.3: what failed, then the agent's account of why, then
            // the stages the run reached, so the diagnostic is read before
            // the history it explains.
            return .init(
                modules: [
                    .recommendation, .facts, .claims, .stageRail, .factBlock, .summary, .claims,
                    .evidence, .details,
                ],
                actionInsertionIndex: 4,
                reviewingActionInsertionIndex: nil,
                placesReadableClaimsInCard: true)
        case .review_dispute:
            // Plan §9 (revision 78, audit D08): both positions lead when the
            // snapshot carries both; when it carries one claim, that claim
            // leads in their place. Either way the dissent sits beside the
            // actions, ahead of the daemon's facts about the run. Supporting
            // claims stay below with the summary.
            return .init(
                modules: [
                    .comparison, .claims, .factBlock, .facts, .summary, .claims, .evidence,
                    .details,
                ],
                actionInsertionIndex: 4,
                reviewingActionInsertionIndex: nil,
                leadsWithItsClaim: true)
        case .review_diminishing_returns:
            // Plan §7 "Routing": the card leads with the verdict and the
            // reversal list, so the stop cause sits ahead of the yield chart
            // and the cost and diff-growth facts, all above the actions.
            return .init(
                modules: [
                    .recommendation, .stopCause, .yieldChart, .facts, .factBlock, .summary,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 4,
                reviewingActionInsertionIndex: nil)
        case .finding_adjudication:
            // Plan §9 (revision 78, audit D09): one card per finding leads,
            // each holding everything about its finding, so no finding
            // content renders from another module and there is no fact
            // block. The item's recommendation sits with the batch action
            // below the cards; a commit-plan notice, the only row .facts can
            // carry here, still precedes both.
            return .init(
                modules: [
                    .findingFacts, .facts, .recommendation, .summary, .claims, .evidence,
                    .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil)
        case .agent_question:
            // Section 9: the typed decisions lead (what is blocked and the
            // enumerated options, with the recommendation labeled as an agent
            // claim), so the card is answerable without the transcript. The
            // claims module below the actions carries the decisions artifact
            // and any supporting context.
            return .init(
                modules: [
                    .recommendation, .agentQuestion, .facts, .foldedFacts, .factBlock, .summary,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil,
                // The run and stage the question came from are where to look
                // next, not what to decide on, so they fold under the
                // actions with Recorded Context (R26).
                foldedModuleCount: 1)
        case .spec_approval:
            // Plan §9 has this card lead with the ask and a plan-altitude
            // summary and put the full specification below, so `.summary`
            // renders ahead of the action region while `.specification` opens
            // the region below it. A revision still leads: `.specRevision`
            // carries the diff-from-last-reviewed facts and stays first.
            return .init(
                modules: [
                    .recommendation, .specRevision, .summary, .facts, .specification, .factBlock,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 4,
                reviewingActionInsertionIndex: nil)
        case .task_proposal:
            // Frame 5.4: the proposal in the agent's words, then the facts
            // the daemon authenticated about it.
            return .init(
                modules: [
                    .recommendation, .claims, .facts, .factBlock, .summary, .claims, .evidence,
                    .details,
                ],
                actionInsertionIndex: 3,
                reviewingActionInsertionIndex: nil,
                placesReadableClaimsInCard: true)
        case .review_contradiction, .review_configuration,
            .publish_blocked, .effect_proposal:
            return .init(
                modules: [
                    .recommendation, .facts, .factBlock, .summary, .claims, .evidence, .details,
                ],
                actionInsertionIndex: 2,
                reviewingActionInsertionIndex: nil)
        case .system_health, .blocked:
            return .init(
                modules: [.recommendation, .facts, .factBlock, .claims, .evidence, .details],
                actionInsertionIndex: 2,
                reviewingActionInsertionIndex: nil)
        }
    }
}

/// The card's closed-by-default disclosures. The view keeps the open set as
/// local state; a caller names the ones that start open, which is how a
/// screenshot shows what a folded section holds.
enum DecisionDisclosure: Hashable {
    case runDetails
    case recordedContext
    case reviewYield
    /// One claim's source identifiers. Neither a label nor a digest is
    /// unique on its own, and two claims that share both are the same bytes
    /// under the same name, so opening them together loses nothing.
    case claimSource(label: String, digest: String)

    static func claimSource(_ claim: Components.Schemas.AgentClaim) -> Self {
        .claimSource(label: claim.label, digest: claim.digest)
    }
}

/// Where each row of a card's `.facts` module renders: beside the decision,
/// or inside the closed "Run and Binding Details" disclosure. Kept apart
/// from the view so the split is testable without rendering.
///
/// `AttentionDisplay.cardFacts` carries coordinates only (a stage, a run, a
/// round, an identifier), so on a type that folds them every one of its rows
/// folds. A notice is not a coordinate: the commit-plan notice says something
/// about the candidate the operator is deciding on, so it stays visible on
/// every type.
struct DecisionFactPlacement: Equatable {
    static let foldedTitle = "Run and Binding Details"

    let visible: [AttentionDisplay.FactRow]
    let folded: [AttentionDisplay.FactRow]

    /// What the closed fold says it holds: the folded rows' values in row
    /// order, so the stage and what it waits on read without opening it.
    var foldedSummary: String? {
        folded.isEmpty ? nil : folded.map(\.value).joined(separator: " \u{00B7} ")
    }

    init(
        _ item: Components.Schemas.AttentionItem,
        includesCommitPlan: Bool,
        now: Date
    ) {
        let facts = AttentionDisplay.cardFacts(item, now: now)
        let notices: [AttentionDisplay.FactRow] =
            includesCommitPlan
            ? [item.commit_plan_notice?.value1].compactMap { $0 }.map {
                .init("Commit plan", AttentionDisplay.label($0))
            } : []
        if DecisionCardComposition.foldsRoutineFacts(item._type) {
            visible = notices
            folded = facts
        } else {
            visible = facts + notices
            folded = []
        }
    }
}

/// One finding's card on `finding_adjudication` (plan §9 revision 78, visual
/// audit D09): what the card shows before anything is opened, and what its
/// "Reason and alternatives" disclosure holds, in render order. Kept apart
/// from the view so the destination of each proposal and binding field is
/// testable without rendering.
struct FindingCardPresentation: Equatable, Identifiable {
    static let disclosureTitle = "Reason and alternatives"

    struct Alternative: Equatable {
        let route: Components.Schemas.AdjudicationRoute
        let label: String
        let consequence: String
    }

    /// The daemon's finding id. An open disclosure and an alternative
    /// selection are both held under it, so neither follows a card's
    /// position when the proposals reorder.
    let id: String

    // The card face.
    let heading: String
    /// The daemon-authenticated finding text, exactly as bound.
    let message: String
    let producerLabel: String
    /// The producer label without its "(unverified)" word, where the label
    /// carries one; `UnverifiedLabel` draws the word with its explanation.
    let producerUnverifiedKeyword: String?
    let route: String

    // The disclosure, in render order: the proposal in its producer's
    // register, the daemon's coordinates in theirs, then what the proposal
    // rests on and what else the operator may choose.
    let rationale: String
    let proposalRows: [AttentionDisplay.FactRow]
    let evidenceTitle: String
    let evidence: [String]
    let daemonFacts: [AttentionDisplay.FactRow]
    let assumptions: [String]
    let citedRules: [String]
    let alternatives: [Alternative]
    let gatingQuestions: [String]

    static func cards(
        _ binding: Components.Schemas.FindingAdjudicationBinding
    ) -> [FindingCardPresentation] {
        binding.proposals.enumerated().map { index, proposal in
            FindingCardPresentation(proposal, number: index + 1, binding: binding)
        }
    }

    init(
        _ proposal: Components.Schemas.FindingAdjudicationProposal,
        number: Int,
        binding: Components.Schemas.FindingAdjudicationBinding
    ) {
        let producer = AttentionDisplay.adjudicationProducerPresentation(proposal.producer)
        id = proposal.finding_id
        heading = "Finding \(number)"
        message = proposal.finding_message
        producerLabel = producer.label
        producerUnverifiedKeyword = producer.unverifiedKeyword
        route = AttentionDisplay.label(proposal.route)

        rationale = proposal.rationale
        var proposalRows: [AttentionDisplay.FactRow] = [
            .init("Goal relationship", AttentionDisplay.label(proposal.goal_relationship)),
            .init(
                "Work-unit compatibility",
                AttentionDisplay.label(proposal.compatibility?.value1)),
        ]
        // An adjudicator that recorded no confidence gets no row, rather
        // than a row with nothing in it.
        if let confidence = proposal.confidence?.value1 {
            proposalRows.append(.init("Confidence", AttentionDisplay.label(confidence)))
        }
        self.proposalRows = proposalRows
        // The engine fast path also populates evidence (the finding's own
        // containment location, a daemon fact), so the title follows the
        // producer instead of always reading "model-derived" (#892, #984).
        evidenceTitle =
            producer.modelBacked ? "Evidence (model-derived)" : "Evidence (daemon-derived)"
        evidence = proposal.evidence

        var daemonFacts: [AttentionDisplay.FactRow] = [
            .init("Finding", proposal.finding_id, monospaced: true)
        ]
        if let location = proposal.finding_location?.value1 {
            daemonFacts.append(
                .init("Location", AttentionDisplay.findingLocation(location), monospaced: true))
        }
        daemonFacts += [
            .init("Binding digest", binding.adjudication_digest, monospaced: true),
            .init("Run", binding.run_id, monospaced: true),
            .init("Round", "\(binding.round)", monospaced: true),
        ]
        self.daemonFacts = daemonFacts

        assumptions = proposal.assumptions
        citedRules = proposal.cited_rules
        alternatives = proposal.offered_alternatives.map {
            .init(
                route: $0.route, label: AttentionDisplay.label($0.route),
                consequence: $0.consequence)
        }
        gatingQuestions = proposal.open_questions
    }

    /// The heading and message as one spoken element.
    var messageAccessibilityLabel: String {
        message.isEmpty ? heading : "\(heading). \(message)"
    }

    /// The route spoken with the finding it belongs to and who proposed it,
    /// so a model-proposed route is never heard as a fact the daemon
    /// established, wherever VoiceOver lands first.
    var routeAccessibilityLabel: String {
        "\(heading) proposed route, \(producerLabel): \(route)"
    }

    /// Every card's disclosure reads "Reason and alternatives", so the
    /// spoken control says whose it is.
    var disclosureAccessibilityLabel: String {
        "\(Self.disclosureTitle), \(heading)"
    }

    /// A held alternative, said on the card's face. The picker sits inside
    /// the disclosure, so without this a closed card would hide a choice
    /// that "Choose Another Route" still sends, and accepting sends
    /// no choice at all.
    static func selectionNotice(_ route: Components.Schemas.AdjudicationRoute) -> String {
        "Selected alternative: \(AttentionDisplay.label(route)). Accepting does not send it."
    }

    /// The notice spoken with the finding it belongs to.
    func selectionAccessibilityLabel(_ route: Components.Schemas.AdjudicationRoute) -> String {
        "\(heading). \(Self.selectionNotice(route))"
    }

    /// What `accept_recommended_route` covers, said beside the action:
    /// every proposed route the item binds, whichever cards are open and
    /// whatever alternatives are selected. It states coverage only. What
    /// accepting does to each finding is the daemon's to say (the
    /// recommendation and the reason): a disputed finding parks the run
    /// with no disposition recorded, and a parked route beside a fix gets
    /// none either, so "applies every disposition" would be false there.
    static func acceptanceScope(findingCount: Int) -> String {
        findingCount == 1
            ? "Accepting covers the proposed route for the one finding above."
            : "Accepting covers every proposed route above: all \(findingCount) findings."
    }

    /// The same coverage as the object of the recommendation's sentence
    /// (R20), which says it in place of `acceptanceScope` where the card
    /// draws a recommendation.
    static func acceptancePhrase(findingCount: Int) -> String {
        findingCount == 1
            ? "accepting the proposed route for the one finding above"
            : "accepting the proposed route for each of the \(findingCount) findings above"
    }
}

struct DecisionGraphicPresentations: Equatable {
    var stageRail: DecisionStageRailPresentation?
    var comparison: DecisionComparisonPresentation?
    var changeSummary: DecisionChangeSummaryPresentation?
    var attemptTimings: DecisionFactPresentation?
    var diminishingYield: DecisionYieldPresentation?
    var prominentClaimIndex: Int?

    init(
        stageRail: DecisionStageRailPresentation? = nil,
        comparison: DecisionComparisonPresentation? = nil,
        changeSummary: DecisionChangeSummaryPresentation? = nil,
        attemptTimings: DecisionFactPresentation? = nil,
        diminishingYield: DecisionYieldPresentation? = nil,
        prominentClaimIndex: Int? = nil
    ) {
        self.stageRail = stageRail
        self.comparison = comparison
        self.changeSummary = changeSummary
        self.attemptTimings = attemptTimings
        self.diminishingYield = diminishingYield
        self.prominentClaimIndex = prominentClaimIndex
    }
}

struct DecisionChecklistPresentation: Equatable {
    /// A row's severity class. The declaration order is the sort order the
    /// checklist renders in, most severe first, so `allCases` is both the
    /// grouping key and the order the verdict line counts in.
    enum Result: Equatable, CaseIterable {
        case failed
        case waived
        case advisory
        case note
        case passed

        var accessibilityState: String {
            switch self {
            case .failed: "needs attention"
            case .waived: "waived"
            case .advisory: "advisory"
            case .note: "informational"
            case .passed: "passed"
            }
        }

        /// The verdict line's token for `count` rows of this class. Only
        /// `note` is a noun and pluralizes; the other words are states that
        /// read the same at any count.
        func countToken(_ count: Int) -> String {
            switch self {
            case .failed: "\(count) failed"
            case .waived: "\(count) waived"
            case .advisory: "\(count) advisory"
            case .note: count == 1 ? "1 note" : "\(count) notes"
            case .passed: "\(count) passed"
            }
        }
    }

    struct Row: Equatable, Identifiable {
        let label: String
        let value: String
        let result: Result

        var id: String { label }
    }

    /// The daemon's verdict row, drawn as the module's leading line rather
    /// than as a row. Optional because the initializer is type-agnostic,
    /// though only `ready_for_final_review` composes a checklist.
    let verdict: Row?
    /// Every row but the verdict, grouped by severity class and keeping the
    /// daemon's order inside each class.
    let rows: [Row]
    /// The count tokens alone, without the verdict word.
    let countSummary: String
    let verdictLine: String
    let summary: String
    let accessibilitySummary: String

    /// The rows drawn above the passed disclosure.
    var leadingRows: [Row] { rows.filter { $0.result != .passed } }
    var passedRows: [Row] { rows.filter { $0.result == .passed } }

    init?(_ item: Components.Schemas.AttentionItem) {
        var rows: [Row] = []
        var verdict: Row?
        let detail = item.readiness_detail?.value1
        let invalidation = item.readiness_invalidation?.value1
        let freshness = item.base_freshness?.value1
        // Staleness is a daemon fact on either axis: the superseding
        // invalidation, or the base-advance watch observing a moved base. Both
        // demote the verdict and its bound coordinates; the client compares
        // nothing itself and derives no reason from the verdict class.
        let stale = invalidation != nil || freshness?.advanced == true
        if invalidation != nil {
            verdict = .init(label: "Verification verdict", value: "Invalidated", result: .failed)
        } else if let readiness = item.readiness?.value1 {
            let word: String
            switch readiness._class {
            case .ready_clean: word = "Clean"
            case .ready_degraded: word = "Degraded"
            }
            verdict = .init(
                label: "Verification verdict",
                value: stale ? "\(word), stale" : word,
                result: stale || readiness._class == .ready_degraded ? .failed : .passed)
        } else if item._type == .ready_for_final_review {
            verdict = .init(label: "Verification verdict", value: "Unavailable", result: .failed)
        }
        if let detail {
            rows.append(
                .init(
                    label: "Bound to",
                    value:
                        "Head \(AttentionDisplay.shortRevision(detail.candidate_head)) · "
                        + "Base \(detail.base.base_ref)@\(AttentionDisplay.shortRevision(detail.base.base_sha))",
                    result: stale ? .failed : .passed))
        }
        if let invalidation {
            rows.append(
                .init(
                    label: AttentionDisplay.label(invalidation.reason),
                    value:
                        "bound \(AttentionDisplay.shortRevision(invalidation.bound)), "
                        + "observed \(AttentionDisplay.shortRevision(invalidation.observed))",
                    result: .failed))
        }
        for requirement in detail?.requirements ?? [] {
            rows.append(Self.requirementRow(requirement))
        }
        if let scope = item.scope_decision?.value1 {
            rows.append(
                .init(
                    label: "Scope kept",
                    value:
                        "\(scope.paths.joined(separator: ", ")) left unchanged by operator decision; required work remains unmet",
                    result: .note))
        }
        if let notice = item.commit_plan_notice?.value1 {
            rows.append(
                .init(
                    label: "Commit plan",
                    value: AttentionDisplay.label(notice),
                    result: .note))
        }
        if let freshness {
            rows.append(
                .init(
                    label: "Base freshness",
                    value: freshness.advanced
                        ? "Advanced past \(AttentionDisplay.shortRevision(freshness.admitted_base_sha)), "
                            + "now \(AttentionDisplay.shortRevision(freshness.observed_base_sha))"
                        : "Current",
                    result: freshness.advanced ? .failed : .passed))
        }
        if let history = item.yield_history?.value1 {
            let unresolved = history.rounds.reduce(into: 0) { count, round in
                count += round.findings_ingested - round.fixed - round.declined - round.deferred
            }
            if unresolved > 0 {
                rows.append(
                    .init(
                        label: "Terminal review",
                        value: unresolved == 1
                            ? "1 finding unresolved" : "\(unresolved) findings unresolved",
                        result: .failed))
            } else {
                switch history.terminal_outcome {
                case .clean:
                    rows.append(.init(label: "Terminal review", value: "Clean", result: .passed))
                case .findings:
                    rows.append(
                        .init(
                            label: "Terminal review",
                            value: "Findings dispositioned",
                            result: .passed))
                }
            }
        }
        guard verdict != nil || !rows.isEmpty else { return nil }
        self.verdict = verdict
        // Group by class rather than sorting, so each class keeps the
        // daemon's row order without relying on sort stability.
        self.rows = Result.allCases.flatMap { result in rows.filter { $0.result == result } }
        let countTokens = Result.allCases.compactMap { result -> String? in
            let count = rows.filter { $0.result == result }.count
            return count == 0 ? nil : result.countToken(count)
        }
        countSummary = countTokens.joined(separator: " · ")
        let tokens = ([verdict?.value].compactMap { $0 } + countTokens)
        verdictLine = tokens.joined(separator: " · ")
        // The spoken label joins the same tokens with commas: a middle dot
        // is a visual separator whose readout depends on the listener's
        // punctuation verbosity.
        summary = "Readiness checklist: " + tokens.joined(separator: ", ") + "."
        accessibilitySummary =
            summary + " "
            + ([verdict].compactMap { $0 } + self.rows).map { row in
                "\(row.label): \(row.value), \(row.result.accessibilityState)"
            }.joined(separator: "; ") + "."
    }
}

extension DecisionChecklistPresentation {
    /// One evaluated requirement as a checklist row. The label is the daemon's
    /// requirement key; the value is its typed state, and a waived failure
    /// names the waiver's identity, the dimension it covers, and its granting
    /// authority (plan §6) while keeping the failure marker, so a degraded
    /// card says why it is degraded without opening the technical details.
    fileprivate static func requirementRow(
        _ requirement: Components.Schemas.ReadinessRequirement
    ) -> Row {
        let advisory = requirement.kind == .optional
        let label = advisory ? "\(requirement.requirement_key) (optional)" : requirement.requirement_key
        let state = AttentionDisplay.label(requirement.state)
        switch requirement.state {
        case .passed:
            return .init(label: label, value: state, result: .passed)
        case .not_applicable:
            return .init(label: label, value: state, result: .note)
        case .failed, .not_run:
            if let waiver = requirement.waiver?.value1 {
                return .init(
                    label: label,
                    value:
                        "\(state), waived for \(waiver.dimension) by "
                        + AttentionDisplay.label(waiver.authority).lowercased()
                        + ", waiver \(waiver.id)",
                    result: .waived)
            }
            return .init(
                label: label, value: advisory ? "\(state) (advisory)" : state,
                result: advisory ? .advisory : .failed)
        }
    }
}

struct DecisionYieldPresentation: Equatable {
    struct Round: Equatable, Identifiable {
        let number: Int
        let newFindings: Int
        let recurringFindings: Int

        var id: Int { number }
        var total: Int { newFindings + recurringFindings }
        var text: String {
            "Round \(number): \(newFindings) new, \(recurringFindings) recurring"
        }
    }

    let rounds: [Round]
    let summary: String
    /// What the closed Review Yield fold says of itself: how many rounds
    /// ran, and how the daemon recorded the last one where it did.
    let foldSummary: String

    init(rounds: [Round], lastRound: String? = nil) {
        self.rounds = rounds
        summary = "Review yield: " + rounds.map(\.text).joined(separator: "; ") + "."
        foldSummary =
            ([rounds.count == 1 ? "1 round" : "\(rounds.count) rounds", lastRound] as [String?])
            .compactMap { $0 }.joined(separator: " · ")
    }

    init?(_ item: Components.Schemas.AttentionItem) {
        guard let history = item.yield_history?.value1 else { return nil }
        let lastRound =
            switch history.terminal_outcome {
            case .clean: "last clean"
            case .findings: "last had findings"
            }
        self.init(
            rounds: history.rounds.map {
                .init(
                    number: $0.round,
                    newFindings: $0.new_findings,
                    recurringFindings: $0.recurring_findings)
            },
            lastRound: lastRound)
    }
}

struct DecisionStageRailPresentation: Equatable {
    enum State: Equatable {
        case completed
        case current
        case failed
        case pending

        var accessibilityLabel: String {
            switch self {
            case .completed: "completed"
            case .current: "current"
            case .failed: "failed"
            case .pending: "pending"
            }
        }
    }

    struct Entry: Equatable, Identifiable {
        let id: String
        let title: String
        let detail: String?
        let context: String?
        let timestamp: String?
        /// The instant `timestamp` shortens, so the rail can keep the exact
        /// value one gesture away. Not part of the spoken label.
        let instant: Date?
        let state: State

        init(
            id: String,
            title: String,
            detail: String? = nil,
            context: String? = nil,
            timestamp: String? = nil,
            instant: Date? = nil,
            state: State
        ) {
            self.id = id
            self.title = title
            self.detail = detail
            self.context = context
            self.timestamp = timestamp
            self.instant = instant
            self.state = state
        }

        var accessibilityLabel: String {
            [title, state.accessibilityLabel, detail, context, timestamp]
                .compactMap { $0 }
                .joined(separator: ", ")
        }
    }

    let entries: [Entry]
    let summary: String

    /// The stages the run reached, newest first, as a card lists history
    /// (frame 5.3). A stage the run never reached is left out: above the
    /// failure it would read as the latest event. The summary still counts
    /// every stage.
    var reachedNewestFirst: Self {
        .init(entries: entries.filter { $0.state != .pending }.reversed(), summary: summary)
    }

    static func failure(stages: [String], failedStageIndex: Int) -> Self? {
        guard stages.indices.contains(failedStageIndex) else { return nil }
        let entries = stages.enumerated().map { index, stage in
            Entry(
                id: "\(index)-\(stage)",
                title: stage,
                state: index < failedStageIndex ? .completed : index == failedStageIndex ? .failed : .pending)
        }
        return .init(
            entries: entries,
            summary:
                "\(stages[failedStageIndex]) failed, stage \(failedStageIndex + 1) of \(stages.count).")
    }

    static func timeline(entries: [Entry]) -> Self {
        let summary =
            entries.isEmpty
            ? "No stage, round, or decision history recorded."
            : "Stage history: "
                + entries.map { entry in
                    [entry.title, entry.detail, entry.context, entry.timestamp]
                        .compactMap { $0 }
                        .joined(separator: ", ")
                }.joined(separator: "; ") + "."
        return .init(entries: entries, summary: summary)
    }
}

struct DecisionComparisonPresentation: Equatable {
    struct Position: Equatable, Identifiable {
        let title: String
        let text: String

        var id: String { title }
    }

    let positions: [Position]
    let verifiableFacts: [DecisionFactPresentation.Fact]
    let summary: String

    init(
        positions: [Position],
        verifiableFacts: [DecisionFactPresentation.Fact]
    ) {
        self.positions = positions
        self.verifiableFacts = verifiableFacts
        let positionSummary = positions.map { "\($0.title): \($0.text)" }.joined(separator: "; ")
        summary = "Disputed positions: \(positionSummary)."
    }
}

struct DecisionChangeSummaryPresentation: Equatable {
    let text: String
    let summary: String

    init(text: String) {
        self.text = text
        summary = "Change summary: \(text)"
    }
}

struct DecisionFactPresentation: Equatable {
    struct Fact: Equatable, Identifiable {
        let label: String
        let value: String

        var id: String { label }
    }

    let title: String
    let facts: [Fact]
}

struct DecisionModuleContainer<Content: View>: View {
    let title: String
    var dashed = false
    @ViewBuilder let content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: title)
            content
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.ground))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(
                    Color.rule,
                    style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : [])))
    }
}

/// A change as one row (R28): the keyword, then the counts in the diff cuts
/// and what they count in mono. The final review counts a diff's files; a
/// revised specification counts lines against the revision it supersedes.
struct DecisionChangeRow: View {
    /// What the counts measure, which the row draws after them.
    enum Measure: Equatable {
        case files(Int)
        case lines
    }

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    let keyword: String
    let counts: DiffCounts
    let measure: Measure

    init(diff: Components.Schemas.DiffStats) {
        keyword = "Change"
        counts = AttentionDisplay.diffCounts(diff)
        measure = .files(diff.files_changed)
    }

    init(specification diff: Components.Schemas.SpecDiff, sinceRevision prior: Int) {
        keyword = "Change Since Revision \(prior)"
        counts = .init(added: diff.lines_added, removed: diff.lines_removed)
        measure = .lines
    }

    var body: some View {
        let layout =
            dynamicTypeSize >= .accessibility1
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 3))
            : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: 12))
        layout {
            KeywordLabel(text: keyword)
            if dynamicTypeSize < .accessibility1 {
                Spacer(minLength: 12)
            }
            Text("\(counts.text)\(trailing)")
                .font(FreesideFont.monoValue)
                .foregroundStyle(Color.ink)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(spokenLabel))
    }

    private var trailing: String {
        switch measure {
        case .files(let count): " · \(AttentionDisplay.fileCount(count))"
        case .lines: " lines"
        }
    }

    /// The row in words, since the plus and minus signs carry the meaning
    /// only on screen.
    var spokenLabel: String {
        switch measure {
        case .files(let count):
            "\(keyword): \(counts.spoken), \(AttentionDisplay.fileCount(count))"
        case .lines:
            "\(keyword): \(counts.added) lines added, \(counts.removed) removed"
        }
    }
}

/// The daemon's readiness verdict as a bordered item (survey card 4b): the
/// verdict chip and the row counts, the rows that need reading, and the
/// passed ones one disclosure away.
struct DecisionChecklistModuleView: View {
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @ScaledMetric(relativeTo: .callout) private var markerDiameter: CGFloat = screenshotMetricBase(
        8, relativeTo: .callout)
    @State private var passedExpanded = false
    let presentation: DecisionChecklistPresentation

    private static let rowGap = DecisionCardComposition.Scale.refined.moduleGap

    var body: some View {
        VStack(alignment: .leading, spacing: Self.rowGap) {
            KeywordLabel(text: "Readiness checklist")
            verdictLine
            ForEach(presentation.leadingRows) { row in
                checklistRow(row)
            }
            let passed = presentation.passedRows
            if !passed.isEmpty {
                // Closed, the labels still name every passed requirement, so
                // no row drops out of the surface.
                SentenceDisclosure(
                    label: "\(passed.count) Passed",
                    summary: passedExpanded ? nil : passed.map(\.label).joined(separator: " · "),
                    isExpanded: $passedExpanded
                ) {
                    VStack(alignment: .leading, spacing: Self.rowGap) {
                        ForEach(passed) { row in
                            checklistRow(row)
                        }
                    }
                }
            }
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(presentation.accessibilitySummary))
    }

    /// The module's first line: the daemon's verdict word, then the counts of
    /// the rows below it.
    @ViewBuilder private var verdictLine: some View {
        // The rows below stack at an accessibility size through the
        // fact-row rule; the leading line follows them rather than
        // wrapping its counts into a narrow trailing column.
        let layout =
            dynamicTypeSize >= .accessibility1
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 3))
            : AnyLayout(HStackLayout(alignment: .center, spacing: 10))
        layout {
            if let verdict = presentation.verdict {
                StateChip(
                    label: verdict.value, color: .ink,
                    cut: verdict.result == .failed ? .attention : .ink)
            }
            if !presentation.countSummary.isEmpty {
                Text(presentation.countSummary)
                    .font(FreesideFont.monoValue)
                    .foregroundStyle(Color.ink)
            }
        }
    }

    @ViewBuilder private func checklistRow(
        _ row: DecisionChecklistPresentation.Row
    ) -> some View {
        // The dot says only whether the row needs reading; the value beside
        // it names the state in words.
        let needsReading = row.result == .failed || row.result == .waived
        let label = HStack(alignment: .center, spacing: 10) {
            Circle()
                .fill(needsReading ? Color.waxText : Color.ink)
                .frame(width: markerDiameter, height: markerDiameter)
            Text(row.label)
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.ink)
        }
        let value = Text(row.value)
            .font(FreesideFont.monoValue)
            .foregroundStyle(Color.ink)
        // The fact-row rule owns when a value is too long for a trailing
        // column; the marker keeps the checklist's own row shape.
        if FactRow.stacks(row.value, at: dynamicTypeSize) {
            VStack(alignment: .leading, spacing: 3) {
                label
                value.fixedSize(horizontal: false, vertical: true)
            }
        } else {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                label
                Spacer(minLength: 12)
                value.multilineTextAlignment(.trailing)
            }
        }
    }
}

struct DecisionYieldChartModuleView: View {
    /// A fixed key beside accessibility-size text reads as a speck, and a
    /// legend that can't be identified does not key anything.
    @ScaledMetric(relativeTo: .caption) private var legendSwatch: CGFloat = 8
    let presentation: DecisionYieldPresentation
    var showsBars = true
    /// When set, the rounds fold into a "Review Yield" disclosure in place
    /// of the module card: the final review reads its verdict first and the
    /// rounds that led to it on demand (D07).
    var isExpanded: Binding<Bool>? = nil

    var body: some View {
        if let isExpanded {
            SentenceDisclosure(
                label: Self.title, summary: presentation.foldSummary, isExpanded: isExpanded
            ) {
                VStack(alignment: .leading, spacing: 8) {
                    rounds
                }
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.top, 8)
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text(presentation.summary))
            }
        } else {
            VStack(alignment: .leading, spacing: DecisionCardComposition.Scale.refined.moduleGap) {
                KeywordLabel(text: Self.title)
                rounds
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Text(presentation.summary))
        }
    }

    private static let title = "Review Yield"

    /// One row a round (frame 5.2): the round on the left, its counts on
    /// the right in the two fills' colors, and, where bars draw, one capsule
    /// beneath scaled against the busiest round.
    @ViewBuilder
    private var rounds: some View {
        let busiest = max(presentation.rounds.map(\.total).max() ?? 1, 1)
        ForEach(presentation.rounds) { round in
            VStack(alignment: .leading, spacing: 5) {
                HStack(alignment: .firstTextBaseline, spacing: 12) {
                    Text("Round \(round.number)")
                        .font(FreesideFont.factLabel)
                        .foregroundStyle(Color.ink)
                    Spacer(minLength: 0)
                    Text(
                        "\(Text("\(round.newFindings) new").foregroundStyle(Color.accentText)) · \(Text("\(round.recurringFindings) recurring").foregroundStyle(Color.waxText))"
                    )
                    .font(FreesideFont.monoValue)
                    .foregroundStyle(Color.inkDim)
                }
                if showsBars {
                    GeometryReader { geometry in
                        HStack(spacing: 0) {
                            Rectangle()
                                .fill(Color.accentBorder)
                                .frame(
                                    width: geometry.size.width
                                        * CGFloat(round.newFindings) / CGFloat(busiest))
                            Rectangle()
                                .fill(Color.waxText)
                                .frame(
                                    width: geometry.size.width
                                        * CGFloat(round.recurringFindings) / CGFloat(busiest))
                        }
                        .clipShape(Capsule())
                    }
                    .frame(height: 8)
                }
            }
        }
        // The bars carry two fills with no other key; the legend names
        // them where they render.
        if showsBars {
            HStack(spacing: 16) {
                legendToken(color: .accentBorder, text: "new findings")
                legendToken(color: .waxText, text: "recurring")
            }
        }
    }

    private func legendToken(color: Color, text: String) -> some View {
        HStack(spacing: 6) {
            Circle()
                .fill(color)
                .frame(width: legendSwatch, height: legendSwatch)
            Text(text)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
    }
}

struct DecisionComparisonModuleView: View {
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    let presentation: DecisionComparisonPresentation

    var body: some View {
        DecisionModuleContainer(title: "Positions") {
            let layout =
                dynamicTypeSize >= .accessibility1
                ? AnyLayout(VStackLayout(alignment: .leading, spacing: 8))
                : AnyLayout(HStackLayout(alignment: .top, spacing: 8))
            layout {
                ForEach(presentation.positions) { position in
                    VStack(alignment: .leading, spacing: 4) {
                        Text(position.title)
                            .font(FreesideFont.sans(.callout, weight: .semibold))
                        Text(position.text)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .padding(10)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(RoundedRectangle(cornerRadius: 6).fill(Color.neutralWash))
                }
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(presentation.summary))
    }
}

struct StageRail: View {
    enum AccessibilityStyle {
        case summary
        case entries
    }

    /// How much of each entry the rail draws. A run row has no room for
    /// headline labels under four dots, so `.compact` draws the dots
    /// alone and leaves the names to the accessibility summary and, on
    /// the desktop, the hover tooltip.
    enum LabelStyle {
        case full
        case compact
    }

    @ScaledMetric(relativeTo: .body) private var connectorLength: CGFloat = 44
    @ScaledMetric(relativeTo: .body) private var compactConnectorLength: CGFloat = 14
    let title: String?
    let presentation: DecisionStageRailPresentation
    let axis: Axis
    var showsSummaryText = true
    var accessibilityStyle: AccessibilityStyle = .summary
    var labelStyle: LabelStyle = .full

    @ViewBuilder
    var body: some View {
        switch accessibilityStyle {
        case .summary:
            rail
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text(presentation.summary))
        case .entries:
            rail
        }
    }

    private var rail: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let title {
                Text(title)
                    .font(FreesideFont.title)
            }
            switch (axis, labelStyle) {
            case (.vertical, _):
                verticalRail
            case (.horizontal, .full):
                horizontalRail
            case (.horizontal, .compact):
                compactHorizontalRail
            }
            if showsSummaryText || presentation.entries.isEmpty {
                Text(presentation.summary)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.inkDim)
            }
        }
    }

    private var verticalRail: some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(presentation.entries.enumerated()), id: \.element.id) { index, entry in
                HStack(alignment: .top, spacing: 12) {
                    VStack(spacing: 0) {
                        marker(entry.state)
                        if index < presentation.entries.count - 1 {
                            Rectangle()
                                .fill(Color.milestoneConnector)
                                .frame(width: 2, height: connectorLength)
                                .accessibilityHidden(true)
                        }
                    }
                    entryLabel(entry)
                }
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text(entry.accessibilityLabel))
            }
        }
    }

    private var horizontalRail: some View {
        HStack(alignment: .top, spacing: 0) {
            ForEach(Array(presentation.entries.enumerated()), id: \.element.id) { index, entry in
                VStack(spacing: 6) {
                    HStack(spacing: 0) {
                        Rectangle()
                            .fill(index == 0 ? Color.clear : Color.milestoneConnector)
                            .frame(height: 2)
                        marker(entry.state)
                        Rectangle()
                            .fill(
                                index == presentation.entries.count - 1
                                    ? Color.clear : Color.milestoneConnector
                            )
                            .frame(height: 2)
                    }
                    entryLabel(entry)
                        .multilineTextAlignment(.center)
                }
                .frame(maxWidth: .infinity)
            }
        }
    }

    private var compactHorizontalRail: some View {
        HStack(spacing: 0) {
            ForEach(Array(presentation.entries.enumerated()), id: \.element.id) { index, entry in
                if index > 0 {
                    Rectangle()
                        .fill(Color.milestoneConnector)
                        .frame(width: compactConnectorLength, height: 2)
                        .accessibilityHidden(true)
                }
                marker(entry.state)
                    .help(entry.accessibilityLabel)
            }
        }
    }

    /// The current entry is a filled ink marker and a prior one a hollow
    /// ring, so the rail marks where the work stands without the accent:
    /// accent means an Inbox item waits on the operator, which a milestone
    /// never says.
    @ViewBuilder
    private func marker(_ state: DecisionStageRailPresentation.State) -> some View {
        switch state {
        case .completed:
            ChronologyMarker(isCurrent: false)
        case .current:
            ChronologyMarker(isCurrent: true)
        case .failed, .pending:
            Circle()
                .fill(state == .failed ? Color.waxText : Color.milestoneConnector)
                .frame(width: ChronologyMarker.diameter, height: ChronologyMarker.diameter)
                .accessibilityHidden(true)
        }
    }

    /// The entry the rail stands on (current, or failed in wax) reads
    /// semibold; every other title recedes to regular ink-dim. The faces
    /// are the refined scale's (frame 5.3): a 16pt title, and the context
    /// and the time in the 13.5pt mono a trailing summary uses, so no
    /// line a rail draws is under the 11.5pt floor on macOS.
    private func entryLabel(_ entry: DecisionStageRailPresentation.Entry) -> some View {
        let emphasized = entry.state == .current || entry.state == .failed
        return VStack(alignment: axis == .vertical ? .leading : .center, spacing: 4) {
            Text(entry.title)
                .font(FreesideFont.railTitle(emphasized: emphasized))
                .foregroundStyle(
                    entry.state == .failed ? Color.waxText : emphasized ? Color.ink : Color.inkDim)
            if let detail = entry.detail {
                // A receded title must not sit over a heavier detail.
                Text(detail)
                    .font(FreesideFont.railDetail(emphasized: emphasized))
                    .foregroundStyle(emphasized ? AnyShapeStyle(.foreground) : AnyShapeStyle(Color.inkDim))
            }
            if let context = entry.context {
                Text(context)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
            }
            if let timestamp = entry.timestamp {
                let text = Text(timestamp)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
                if let instant = entry.instant {
                    text.exactInstant(instant)
                } else {
                    text
                }
            }
        }
        .frame(maxWidth: axis == .horizontal ? .infinity : nil, alignment: .leading)
    }
}
