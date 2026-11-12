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
    /// The final review's reading order for its pull request (plan §7,
    /// §9): the agent's stops, each with the daemon's read of the lines it
    /// points at.
    case reviewGuide
    case claims
    case evidence
    case details
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

    /// The modules the decision rests on: everything ahead of the control
    /// group.
    var leadModules: Range<Int> { 0..<controlGroupStart }
    /// The modules a control group holds between its reviewing action and
    /// its actions. Empty on a type with no reviewing action.
    var controlGroupModules: Range<Int> { controlGroupStart..<actionInsertionIndex }
    /// The closed folds under the card's one hairline.
    var foldedModules: Range<Int> {
        actionInsertionIndex..<(actionInsertionIndex + foldedModuleCount)
    }
    /// The supporting modules after the folds.
    var supportingModules: Range<Int> { foldedModules.upperBound..<modules.count }

    private var controlGroupStart: Int { reviewingActionInsertionIndex ?? actionInsertionIndex }

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
            // claims move to the card's foot.
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

    /// The attachment claims the card's foot lists, on both platforms. A
    /// claim the card leads with stays out, so it renders once.
    func footClaims(
        from claims: [Components.Schemas.AgentClaim],
        prominentClaimIndex: Int?
    ) -> [Components.Schemas.AgentClaim] {
        let lead = cardLeadClaims(from: claims, prominentClaimIndex: prominentClaimIndex)
        return claims.filter {
            $0.text == nil && !AgentClaimLabels.isApprovalMaterial($0.label) && !lead.contains($0)
        }
    }

    /// The claims the Evidence fold lists (#2607): the foot's attachment
    /// claims and, on iPhone, the text claims a supporting claims module
    /// would otherwise draw below the actions. The Mac lists those text
    /// claims in its action region instead, so the card draws each once.
    func evidenceClaims(
        from claims: [Components.Schemas.AgentClaim],
        prominentClaimIndex: Int?,
        on platform: UnverifiedContext.Platform
    ) -> [Components.Schemas.AgentClaim] {
        let foot = footClaims(from: claims, prominentClaimIndex: prominentClaimIndex)
        switch platform {
        case .mac:
            return foot
        case .phone:
            let supporting = supportingModules.filter { modules[$0] == .claims }.flatMap {
                claimsInCard(from: claims, at: $0, prominentClaimIndex: prominentClaimIndex, on: .phone)
            }
            return foot + supporting
        }
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
            .system_health, .blocked, .unknown:
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
        case .unknown:
            // A type this build doesn't know can't vouch that the daemon
            // composed its reason, so the shell quotes it as unverified prose.
            return true
        }
    }

    /// An item's `reason` as the card draws it, wherever it draws it.
    struct Reason: Equatable {
        let text: String
        /// An agent wrote it, so it draws quoted under the unverified label.
        let isAgentWritten: Bool

        var label: String { isAgentWritten ? "Agent reason" : "Reason" }
    }

    /// The reason the card's Recorded Context carries in full on every type
    /// (plan §9 revision 82), including one the card draws nowhere else, such as the
    /// diminishing-returns `Binding:` line. Nil only when the daemon recorded
    /// no reason.
    static func reason(for item: Components.Schemas.AttentionItem) -> Reason? {
        guard !item.reason.isEmpty else { return nil }
        return Reason(text: item.reason, isAgentWritten: reasonIsAgentWritten(item._type))
    }

    /// Whether the card draws `item`'s reason outside Recorded Context. The reason is
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
    /// Recorded Context carries the reason in full on every type either way
    /// (plan §9 revision 82), so a reason this drops is still one disclosure
    /// away.
    func drawsReason(for item: Components.Schemas.AttentionItem) -> Bool {
        if item._type == .review_diminishing_returns, item.review_diminishing != nil {
            return false
        }
        return !Self.reasonIsAgentSummary(item._type) || summaries(from: item.agent_claims).isEmpty
    }

    /// A card's gaps, padding, and corner (R10). The gaps between sections,
    /// inside a module, within a control group, and above the folds, which
    /// sit under the card's one hairline, are the platform ladder's. Every
    /// decision type draws on it.
    enum Scale {
        private static let ladder = FreesideLadder.current
        static let sectionGap = ladder.sectionGap
        /// Between the eyebrow and the ask.
        static let headGap: CGFloat = 10
        static let moduleGap = ladder.moduleGap
        static let controlGap = ladder.controlGap
        static let foldGap: CGFloat = 10
        /// The space between the hairline and the first fold.
        static let foldLead = ladder.foldLead
        static let cornerRadius: CGFloat = 12
        #if os(macOS)
            /// The widest the detail column grows (R18, revised 3 Nov 2026).
            /// Every detail surface draws in one column, centered in the
            /// pane and as wide as the pane's margins allow up to this cap,
            /// so selecting something changes the content, not its shape,
            /// and a wide window uses its width instead of leaving most of
            /// the pane empty.
            static let columnCap: CGFloat = 960
            /// Where the column sits in a pane wider than it: centered.
            static let columnAlignment = Alignment.top
        #else
            /// A regular-width iOS detail page keeps the 640pt card at the
            /// pane's leading margin: the Mac's wider, centered column
            /// (3 Nov 2026) left iPhone and iPad unchanged.
            static let columnCap: CGFloat = 640
            static let columnAlignment = Alignment.topLeading
        #endif
        /// The widest a line of running text draws, at any column width: a
        /// 640pt card's line, inside its 22pt padding. The ask, the
        /// statement or reason, claims prose, finding messages, and the
        /// conversation thread keep it, leading in the column; fact rows,
        /// specification and diff blocks, attachment rows, checklists, the
        /// stage rail, comparisons, control rows, and folds take the full
        /// column.
        static let proseMeasure: CGFloat = 596

        /// The pane's margin around a card (R18): 16 all round at phone
        /// width; in the detail column 24 from each side, 20 from the top,
        /// and 40 under the card, so every card in a pane starts at one x and
        /// one y.
        static func paneMargin(compact: Bool) -> EdgeInsets {
            compact
                ? EdgeInsets(top: 16, leading: 16, bottom: 16, trailing: 16)
                : EdgeInsets(top: 20, leading: 24, bottom: 40, trailing: 24)
        }

        /// A card at phone width is 16 from each side, 18 from the top, and
        /// 16 from the bottom; a wider one sits 22 in, with 20 under its
        /// last line.
        static func padding(compact: Bool) -> EdgeInsets {
            compact
                ? EdgeInsets(top: 18, leading: 16, bottom: 16, trailing: 16)
                : EdgeInsets(top: 22, leading: 22, bottom: 20, trailing: 22)
        }
    }

    /// Whether the conversation draws inside the card's modules, under the
    /// specification, instead of directly under the ask. On spec approval
    /// the thread is about the specification and ends in the reply link
    /// (frame 5.1); every other card keeps it under its ask.
    static func placesConversationWithSpecification(
        for type: Components.Schemas.AttentionType
    ) -> Bool {
        type == .spec_approval
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
            .system_health, .blocked, .unknown:
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
            .system_health, .blocked, .unknown:
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

    /// The receipt sentence for an accepted command that only records
    /// (`ActionOutcome.records`) on an item that stays open, or nil for any
    /// other command. Such a command changes nothing else on the card, so its
    /// receipt draws with the control group, beside the button the operator
    /// used, and counts a repeat so a second accepted press reads differently
    /// from the first (#1978). A concluding action, `discuss`, and a
    /// record-only action whose item has since closed keep the head receipt.
    static func recordOnlyReceipt(
        action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem,
        repeatCount: Int
    ) -> String? {
        guard ActionOutcome.of(action) == .records, item.status == .open else { return nil }
        let label = AttentionDisplay.label(action, for: item)
        return repeatCount > 1
            ? "\(label) recorded \(repeatCount) times. This item stays open."
            : "\(label) recorded. This item stays open."
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

    /// Where the card shell draws the `reason` ahead of Recorded Context. The reason
    /// is drawn by the shell, not by a module, so its place is a rule of the
    /// type rather than a position in `modules`.
    enum ReasonPlacement: Equatable {
        /// Directly under the ask, ahead of the actions.
        case underAsk
        /// A closed "Recorded Context" disclosure below the actions.
        case recordedContext
        /// Off the card's face: the Recorded Context fold alone carries it,
        /// as it does on every type.
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
            .effect_proposal, .blocked, .unknown:
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
            .review_contradiction, .review_configuration, .publish_blocked, .system_health, .unknown:
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
    /// the exact start in Identifiers, so the reason leaves the face. That holds
    /// only for the wait the daemon writes today: an item with no typed
    /// wait keeps the generic ask and its reason under the ask. A wait on PR
    /// checks or an external review is not a blocked item's wait: its one
    /// writer (`deadlineRegistration` in daemon/cmd/freesided/scheduler.go,
    /// #2752) records only that the deadline elapsed, on the ready item,
    /// which draws it as a Checks or Review Facts row with the exact instant
    /// in Identifiers.
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

    /// What accepting does, on a finding adjudication whose reason folds: the
    /// first sentence of the line the daemon writes for it
    /// (`findingAdjudicationReason` in
    /// daemon/internal/engine/finding_adjudication.go), which follows any
    /// "Changed after Discuss" lines. An action's consequence never folds
    /// (plan §9), and the recommendation beside the batch action says which
    /// findings accepting covers, not what the run does next. Nil on any
    /// other item and where the reason carries no such line; the whole
    /// reason is one disclosure away either way.
    static func reasonLead(for item: Components.Schemas.AttentionItem) -> String? {
        guard item._type == .finding_adjudication,
            reasonPlacement(for: item) == .recordedContext,
            let line = item.reason.split(separator: "\n")
                .first(where: { $0.hasPrefix("Accepting ") })
        else { return nil }
        guard let end = line.range(of: ". ") else { return String(line) }
        return String(line[..<end.upperBound]).trimmingCharacters(in: .whitespaces)
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
            /// the card's foot.
            case mac
            /// Claims and the recommendation draw in their own modules and
            /// the card's foot.
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
        /// The recommendation block draws, in the agent-claim register.
        var drawsUnverifiedRecommendation = false
        /// The card has a change summary to draw in its fact block.
        var hasChangeSummary = false
        /// The card draws a dispute's two positions, each under its own
        /// unverified keyword.
        var hasComparison = false
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
            // A module under the actions draws inside a fold (Recorded
            // Context or Evidence, #2607), so its keyword is never in view
            // to carry the control.
            if index < actionInsertionIndex,
                drawsUnverifiedKeyword(module, at: index, for: item, in: context)
            {
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

    /// What a claims module draws in the card, which leaves out what the
    /// foot lists. macOS lists text claims in the action region, so a claims
    /// module draws there only where the claim is the card's own lead (D08).
    func claimsInCard(
        from claims: [Components.Schemas.AgentClaim],
        at moduleIndex: Int,
        prominentClaimIndex: Int?,
        on platform: UnverifiedContext.Platform
    ) -> [Components.Schemas.AgentClaim] {
        let module = self.claims(from: claims, at: moduleIndex, prominentClaimIndex: prominentClaimIndex)
        switch platform {
        case .mac:
            return drawsLeadClaimsInCard && claimsAreProminent(at: moduleIndex) ? module : []
        case .phone:
            let foot = footClaims(from: claims, prominentClaimIndex: prominentClaimIndex)
            return module.filter { !foot.contains($0) }
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
            // A routed dispute heads its findings with one
            // `Findings (unverified)` keyword: the reviewer's messages and
            // the model's routes are unchecked.
            if item.review_dispute?.value1.findings?.isEmpty == false { return true }
            guard let binding = item.finding_adjudication?.value1 else { return false }
            return FindingCardPresentation.cards(binding)
                .contains(where: \.modelBacked)
        case .factBlock:
            return context.hasChangeSummary
        case .summary:
            // The final review draws its summary section even with no inline
            // text, to say the summary is unavailable.
            return item._type == .ready_for_final_review
                || !summaries(from: item.agent_claims).isEmpty
        case .reviewGuide:
            return item.review_guide != nil
        case .claims:
            let drawn = claimsInCard(
                from: Self.listedClaims(item), at: index,
                prominentClaimIndex: context.prominentClaimIndex, on: context.platform)
            return !drawn.isEmpty
        case .stopCause:
            // The drift audit's explanation and fixes are the audit model's
            // words, drawn under an unverified keyword (frame 5.2).
            return DecisionStopCausePresentation(item)?.audit != nil
        case .comparison:
            return context.hasComparison
        case .foldedFacts, .specRevision, .specification, .checklist, .stageRail,
            .yieldChart, .evidence, .details:
            return false
        }
    }

    /// The claims a card's claim lists draw from: the item's claims, less
    /// the guide's claim while the Review Guide module draws it, so the
    /// guide renders once. Its digest stays in Identifiers. The foot, the
    /// Evidence fold, and a claims module read from this. The Mac action
    /// region reads the item's claims directly: it draws only inline claim
    /// text, and the guide's claim carries none.
    static func listedClaims(
        _ item: Components.Schemas.AttentionItem
    ) -> [Components.Schemas.AgentClaim] {
        guard let digest = item.review_guide?.value1.claim_digest else { return item.agent_claims }
        return item.agent_claims.filter {
            !($0.label == AgentClaimLabels.reviewGuide && $0.digest == digest)
        }
    }

    /// The guide's claim, which names the invocation that wrote the guide.
    static func reviewGuideClaim(
        _ item: Components.Schemas.AttentionItem
    ) -> Components.Schemas.AgentClaim? {
        guard let digest = item.review_guide?.value1.claim_digest else { return nil }
        return item.agent_claims.first {
            $0.label == AgentClaimLabels.reviewGuide && $0.digest == digest
        }
    }

    /// Whether a review stop offers to open the pull request at its lines.
    /// It sends View PR's `open_pr`, so it draws only where the surface
    /// offers that action. A stale item draws none: its pull request's head
    /// no longer holds the code a stop points at.
    static func opensReviewStops(
        _ item: Components.Schemas.AttentionItem, ranking: DecisionActionRanking
    ) -> Bool {
        !isStale(item)
            && ([ranking.recommended, ranking.reviewing].contains(.open_pr)
                || ranking.principal.contains(.open_pr))
    }

    /// The excerpt a stop opens the pull request at: its first resolved
    /// head excerpt. Nil when it has none, and the stop offers no action.
    static func openingExcerpt(
        _ stop: Components.Schemas.ReviewStop
    ) -> Components.Schemas.ReviewExcerpt? {
        stop.excerpts.first { $0.side == .head && $0.text != nil }
    }

    /// Whether a final review's verdict no longer describes what the pull
    /// request holds: the daemon superseded the binding. That is a daemon
    /// fact; the client compares nothing.
    static func isStale(_ item: Components.Schemas.AttentionItem) -> Bool {
        item._type == .ready_for_final_review && item.readiness_invalidation != nil
    }

    /// The sentence a stale final review leads with (frame 7.3): what moved
    /// after verification, and the coordinates the verdict was reached at.
    /// Built only from typed facts the item carries; nil when they can't
    /// make the sentence (no verdict to name, or an invalidation whose
    /// coordinates are not revisions), and the checklist's chip and rows
    /// carry the staleness alone.
    static func staleNotice(for item: Components.Schemas.AttentionItem) -> String? {
        guard isStale(item), let readiness = item.readiness?.value1 else { return nil }
        let was: String
        switch readiness._class {
        case .ready_clean: was = "clean"
        case .ready_degraded: was = "degraded"
        case .unknown: was = "of an unrecognized class"
        }
        let detail = item.readiness_detail?.value1
        guard let invalidation = item.readiness_invalidation?.value1 else { return nil }
        switch invalidation.reason {
        case .head_changed:
            return "The head changed after verification. The verdict below was \(was) at head "
                + "\(AttentionDisplay.shortRevision(invalidation.bound)); the head is now at "
                + "\(AttentionDisplay.shortRevision(invalidation.observed))."
        // A base advance after handoff no longer invalidates a verdict
        // (plan §5.16, revision 110); an item concluded before then keeps
        // the reason, and its checklist row carries the pair alone.
        case .base_advanced, .retargeted, .identity_changed:
            return nil
        case .unknown:
            // The verdict is stale for a reason this build can't name; say so
            // rather than drop the warning.
            return "The verdict below was \(was) and is no longer current."
        }
    }

    /// The one action a card's row draws filled, or none (R6). A
    /// recommendation the card draws holds the fill in its own block, so the
    /// row fills nothing, unless the operator holds a pick, when the control
    /// that sends the picks is filled: on a finding adjudication a held route
    /// pick (`holdsPick`) moves the fill to Choose Another Route, because the
    /// acceptance sends the proposed routes and drops the pick. Without a
    /// recommendation the fill goes to the type's forward
    /// action, where the row offers it and it needs no confirmation: a
    /// destructive action is never filled. A stale final review fills
    /// nothing: the proof no longer covers the head `View PR` would open. `requested_decision` may repeat
    /// an action, so the row fills only the first and no card draws two.
    static func filledAction(
        for item: Components.Schemas.AttentionItem, ranking: DecisionActionRanking,
        holdsPick: Bool = false
    ) -> Components.Schemas.Action? {
        guard !isStale(item) else { return nil }
        if ranking.recommended != nil {
            let picksFill =
                holdsPick && item._type == .finding_adjudication
                && ranking.principal.contains(.choose_alternative_route)
            return picksFill ? .choose_alternative_route : nil
        }
        guard let forward = forwardAction(for: item._type),
            ranking.principal.contains(forward) || ranking.reviewing == forward,
            AttentionDisplay.confirmationConsequence(forward, for: item) == nil
        else { return nil }
        return forward
    }

    /// Whether the recommendation's button takes the card's one fill (R6).
    /// The recommendation block and the iPhone footer that stands in for it
    /// both read this, so neither draws a fill the row already holds. A
    /// destructive recommendation keeps its own tone whatever this says.
    static func recommendationIsFilled(
        for item: Components.Schemas.AttentionItem, ranking: DecisionActionRanking,
        holdsPick: Bool
    ) -> Bool {
        ranking.recommended != nil
            && filledAction(for: item, ranking: ranking, holdsPick: holdsPick) == nil
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
            .publish_blocked, .blocked, .unknown:
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
            .system_health, .blocked, .unknown:
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
            // agent says changed, then the agent's reading order for the
            // pull request (plan §9 revision 112), then the diff and the
            // daemon's verdict on it, then View PR, so the supported next
            // step follows what it rests on. Returning the work sits below
            // any fact block; the review's round-by-round yield is history,
            // so it folds under the actions.
            return .init(
                modules: [
                    .recommendation, .summary, .reviewGuide, .facts, .checklist, .factBlock,
                    .yieldChart, .claims, .evidence, .details,
                ],
                actionInsertionIndex: 6,
                reviewingActionInsertionIndex: 5,
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
                    .comparison, .claims, .findingFacts, .factBlock, .facts, .summary, .claims, .evidence,
                    .details,
                ],
                actionInsertionIndex: 5,
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
            // Frames 5.1 and 7.2: the agent's summary, then what changed
            // since the last revision, then the specification as one item
            // that opens its readers, then the conversation, which draws
            // with `.specification` (`placesConversationWithSpecification`).
            // All of it is what the approval weighs, so it sits above the
            // actions. 7.2 draws the conversation ahead of the item; one
            // order serves both frames.
            return .init(
                modules: [
                    .recommendation, .summary, .specRevision, .specification, .facts, .factBlock,
                    .claims, .evidence, .details,
                ],
                actionInsertionIndex: 5,
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
        case .system_health, .blocked, .unknown:
            return .init(
                modules: [.recommendation, .facts, .factBlock, .claims, .evidence, .details],
                actionInsertionIndex: 2,
                reviewingActionInsertionIndex: nil)
        }
    }
}

/// Where a card's foot draws its folds (#2607), kept apart from the view so
/// the placement is testable without rendering. The rule the task page
/// density handoff sets: if you'd copy it, it's in Identifiers; if you'd
/// read it, it's in the body.
struct DecisionCardFoot: Equatable {
    enum Fold: Equatable {
        /// The agent's claims and the evidence packet, directly above the
        /// control group.
        case evidence
        /// The one reading fold under the hairline.
        case recordedContext
        /// The card's own identifier rows. A card on a task page hands them
        /// to the page's Identifiers footer instead.
        case identifiers
    }

    let aboveActions: [Fold]
    let underHairline: [Fold]

    init(
        evidenceClaims: [Components.Schemas.AgentClaim],
        attachments: Int,
        recordedContext: DecisionRecordedContext,
        identifierRows: [AttentionDisplay.BindingRow],
        onPage: Bool
    ) {
        aboveActions = evidenceClaims.isEmpty && attachments == 0 ? [] : [.evidence]
        var underHairline: [Fold] = []
        if !recordedContext.parts.isEmpty { underHairline.append(.recordedContext) }
        if !onPage, !identifierRows.isEmpty { underHairline.append(.identifiers) }
        self.underHairline = underHairline
    }
}

/// What a card's Recorded Context holds, in the order it draws them: the
/// daemon's reason in full, the folded modules, the reading rows, and the
/// supporting modules.
struct DecisionRecordedContext: Equatable {
    var hasReason = false
    var hasRunDetails = false
    var hasReviewYield = false
    /// The reading rows: `AttentionDisplay.detailReadingRows`, then the
    /// requested actions not offered on this surface.
    var readingRows: [AttentionDisplay.BindingRow] = []
    var hasFactBlock = false
    var hasAgentSummary = false
    var hasReport = false

    /// The closed fold's mono line: what it holds, in drawing order.
    var parts: [String] {
        var parts: [String] = []
        if hasReason { parts.append("reason") }
        if hasRunDetails { parts.append("run details") }
        if hasReviewYield { parts.append("review yield") }
        let readingParts = Set(readingRows.compactMap { Self.readingPart($0.label) })
        parts.append(contentsOf: Self.readingPartOrder.filter(readingParts.contains))
        if hasFactBlock { parts.append("facts") }
        if hasAgentSummary { parts.append("agent summary") }
        if hasReport { parts.append("report") }
        return parts
    }

    var summary: String { parts.joined(separator: " · ") }

    /// The reading rows' parts, in the order `detailReadingRows` builds
    /// them and the view appends the unavailable actions.
    private static let readingPartOrder = ["posture", "readiness", "review rounds", "not offered here"]

    private static func readingPart(_ label: String) -> String? {
        if label == "Posture" { return "posture" }
        if label == "Readiness" || label.hasPrefix("Requirement ") || label.hasPrefix("Waiver ") {
            return "readiness"
        }
        if label.hasPrefix("Review Round ") || label == "Terminal Review" { return "review rounds" }
        if label == "Requested, Not Available Here" { return "not offered here" }
        return nil
    }
}

/// The card's closed-by-default disclosures. The view keeps the open set as
/// local state; a caller names the ones that start open, which is how a
/// screenshot shows what a folded section holds.
enum DecisionDisclosure: Hashable {
    case runDetails
    case recordedContext
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
/// folds. The Other Agents row also carries each offer's usage reading, read
/// from `pools`; its type doesn't fold. A notice is not a coordinate: the
/// commit-plan notice says something about the candidate the operator is
/// deciding on, so it stays visible on every type.
///
/// `pools`, `locale`, and `timeZone` take no defaults here, unlike
/// `cardFacts`, so a card that places its facts names the pools it reads
/// rather than dropping the readings by omission.
struct DecisionFactPlacement: Equatable {
    static let foldedTitle = "Run and Binding Details"

    let visible: [AttentionDisplay.FactRow]
    let folded: [AttentionDisplay.FactRow]

    init(
        _ item: Components.Schemas.AttentionItem,
        includesCommitPlan: Bool,
        now: Date,
        pools: [Components.Schemas.UsagePoolSnapshot],
        locale: Locale,
        timeZone: TimeZone
    ) {
        let facts = AttentionDisplay.cardFacts(
            item, now: now, pools: pools, locale: locale, timeZone: timeZone)
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
/// "Evidence and Alternatives" disclosure holds, in render order. Kept apart
/// from the view so the destination of each proposal and binding field is
/// testable without rendering.
struct FindingCardPresentation: Equatable, Identifiable {
    static let disclosureTitle = "Evidence and Alternatives"

    /// One route the operator may send for this finding.
    struct RouteOption: Equatable {
        let route: Components.Schemas.AdjudicationRoute
        let label: String
        /// What choosing it does. A proposal states one for each alternative
        /// and none for its own route, whose rationale stands above the
        /// list.
        let consequence: String?
        let isProposed: Bool
    }

    /// The daemon's finding id. An open disclosure and an alternative
    /// selection are both held under it, so neither follows a card's
    /// position when the proposals reorder.
    let id: String

    // The card face.
    let heading: String
    /// The daemon-authenticated finding text, exactly as bound.
    let message: String
    /// The message's title, which runs in with the heading, and the rest of
    /// it as Markdown blocks under the head (`FindingPresentation`). The
    /// daemon normalizes the message's whitespace, so the body is at most
    /// the rest of one paragraph.
    let title: AttributedString?
    let body: [SpecificationBlock]
    /// The route's keyword, the compact mark naming who proposed it, and
    /// the full producer sentence (#2608).
    let producer: AttentionDisplay.AdjudicationProducerPresentation
    let route: String
    /// The proposal's reason, drawn clamped under the route in the same
    /// voice: the rationale is what accepting rests on, so it is read where
    /// the route is (#2608).
    let rationale: String

    /// Whether a model wrote the proposal. Its rationale, evidence, cited
    /// rules, assumptions, and questions are then the model's words, drawn
    /// as quotes under unverified keywords; the daemon's fast path wrote
    /// them otherwise, and they draw as its own text.
    var modelBacked: Bool { producer.modelBacked }

    // An open disclosure draws, in render order: the proposal's qualities,
    // the evidence, the daemon's coordinates in their own register, what
    // the proposal cites and assumes, the routes the operator may send,
    // and what the proposal left open.
    /// The proposal's goal relationship, confidence, and work-unit
    /// compatibility on one line.
    let qualities: String
    let evidence: [String]
    let daemonFacts: [AttentionDisplay.FactRow]
    let citedRules: [String]
    let assumptions: [String]
    /// The proposed route first, then each alternative the proposal offers.
    /// Empty when it offers none: one route is not a choice.
    let routeOptions: [RouteOption]
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
        id = proposal.finding_id
        heading = "Finding \(number)"
        message = proposal.finding_message
        let finding = FindingPresentation(message: proposal.finding_message)
        title = finding.title
        body = finding.body
        producer = AttentionDisplay.adjudicationProducerPresentation(
            proposal.producer, confidence: proposal.confidence?.value1)
        route = AttentionDisplay.label(proposal.route)
        rationale = proposal.rationale
        // An adjudicator that recorded no confidence gets no word for it,
        // rather than a quality with nothing in it.
        qualities = [
            Self.quality(proposal.goal_relationship),
            proposal.confidence.map { "\(AttentionDisplay.label($0.value1)) confidence" },
            Self.quality(proposal.compatibility?.value1),
        ]
        .compactMap(\.self)
        .joined(separator: " · ")
        evidence = proposal.evidence

        // The binding digest stays in Identifiers with the rest of
        // the binding (`AttentionDisplay.findingAdjudicationRows`).
        var daemonFacts: [AttentionDisplay.FactRow] = [
            .init("Finding", proposal.finding_id, monospaced: true)
        ]
        if let location = proposal.finding_location?.value1 {
            daemonFacts.append(
                .init("Location", AttentionDisplay.findingLocation(location), monospaced: true))
        }
        // Two rows, not the frame's one "Run · Round" row: a run id beside a
        // round has nowhere to break on a phone but mid-token.
        daemonFacts += [
            .init("Run", binding.run_id, monospaced: true),
            .init("Round", "\(binding.round)", monospaced: true),
        ]
        self.daemonFacts = daemonFacts

        citedRules = proposal.cited_rules
        assumptions = proposal.assumptions
        routeOptions =
            proposal.offered_alternatives.isEmpty
            ? []
            : [
                RouteOption(
                    route: proposal.route, label: route, consequence: nil, isProposed: true)
            ]
                // A route this app doesn't know is never offered: picking it
                // would send `unknown` back (api/README.md, Response
                // Tolerance).
                + proposal.offered_alternatives.filter { $0.route != .unknown }.map {
                    RouteOption(
                        route: $0.route, label: AttentionDisplay.label($0.route),
                        consequence: $0.consequence, isProposed: false)
                }
        gatingQuestions = proposal.open_questions
    }

    private static func quality(_ relationship: Components.Schemas.GoalRelationship) -> String {
        switch relationship {
        case .required: "Required by the goal"
        case .adjacent: "Adjacent to the goal"
        case .contradictory: "Contradicts the goal"
        case .unclear: "Goal relationship unclear"
        case .unknown: "Goal relationship unrecognized"
        }
    }

    private static func quality(
        _ compatibility: Components.Schemas.WorkUnitCompatibility?
    ) -> String {
        guard let compatibility else { return "compatibility not assessed" }
        return switch compatibility {
        case .allowed: "allowed in the work unit"
        case .work_unit_revision_required: "work-unit revision required"
        case .separate_work_required: "separate work required"
        case .human_decision_required: "human decision required"
        case .unknown: "compatibility unknown"
        }
    }

    var citedRulesKeyword: String { citedRules.count == 1 ? "Cited Rule" : "Cited Rules" }

    var gatingQuestionsKeyword: String {
        gatingQuestions.count == 1 ? "Open Question" : "Open Questions"
    }

    /// The heading and message as one spoken element.
    var messageAccessibilityLabel: String {
        message.isEmpty ? heading : "\(heading). \(message)"
    }

    /// The route spoken with the finding it belongs to, its keyword, and
    /// the full sentence naming who proposed it, so a model-proposed route
    /// is never heard as a fact the daemon established, wherever VoiceOver
    /// lands first. The keyword and mark are drawn only, so this says both,
    /// with the confidence a model's mark draws.
    var routeAccessibilityLabel: String {
        let producer = [producer.spokenKeyword, producer.sentence, producer.spokenConfidence]
            .compactMap(\.self)
            .joined(separator: ", ")
        return "\(heading) \(producer): \(route)"
    }

    /// The reason's clamp control, said with the finding it belongs to.
    var reasonSpokenName: String { "reason for \(heading)" }

    /// Every card's disclosure reads "Evidence and Alternatives", so the
    /// spoken control says whose it is, then what it holds: the spoken
    /// label replaces the drawn one, summary included.
    var disclosureAccessibilityLabel: String {
        [Self.disclosureTitle, heading, disclosureSummary].compactMap(\.self)
            .joined(separator: ", ")
    }

    /// What the closed disclosure holds, counted so it says so before it is
    /// opened: evidence, routes, and questions, naming only those present.
    /// Cited rules and assumptions stay inside uncounted: with all five,
    /// a full finding's summary wraps under its label on a 560pt Mac card,
    /// a line more before the actions (#1141), so the owner chose the three counts the canvas draws (#2608). Daemon
    /// Facts are always inside, so they are not counted. Nil when there is
    /// nothing to count.
    var disclosureSummary: String? {
        let counts = [
            Self.count(evidence.count, "evidence", "evidence"),
            Self.count(routeOptions.count, "route", "routes"),
            Self.count(gatingQuestions.count, "question", "questions"),
        ]
        .compactMap(\.self)
        return counts.isEmpty ? nil : counts.joined(separator: " · ")
    }

    private static func count(_ count: Int, _ singular: String, _ plural: String) -> String? {
        count == 0 ? nil : "\(count) \(count == 1 ? singular : plural)"
    }

    /// How many lines a finding's reason draws before More: two on a card
    /// with one finding, one once the card binds two or more, so two
    /// realistic findings hold their recorded budget to the actions
    /// (`DecisionLayoutBudgetTests`; the first viewport is 520pt, #1141).
    /// Two lines there measured 526pt at planning, before the Evidence fold
    /// moved above the actions (#2607, #2608).
    static func reasonLineLimit(findingCount: Int) -> Int {
        findingCount <= 1 ? 2 : 1
    }

    /// A held alternative, said on the card's face. The route list sits inside
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
        /// The row holds a coordinate that moved after verification. It
        /// reads in wax and is not a failed requirement, so the verdict
        /// line does not count it.
        var isStale = false

        var id: String { label }

        var accessibilityState: String { isStale ? "stale" : result.accessibilityState }
    }

    /// The daemon's verdict row, drawn as the module's leading line rather
    /// than as a row. Optional because the initializer is type-agnostic,
    /// though only `ready_for_final_review` composes a checklist.
    let verdict: Row?
    /// Every row but the verdict, grouped by severity class and keeping the
    /// daemon's order inside each class.
    let rows: [Row]
    /// What the verdict was before it went stale ("was Clean"), nil on a
    /// current verdict or where the item carries none.
    let priorVerdict: String?
    /// What follows the verdict chip: the prior verdict of a stale card,
    /// then the count tokens.
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
        // Staleness is a daemon fact: the superseding invalidation demotes
        // the verdict and its bound coordinates; the client compares
        // nothing itself and derives no reason from the verdict class.
        let stale = invalidation != nil
        var priorVerdict: String?
        let verdictWord: String? = item.readiness.map {
            switch $0.value1._class {
            case .ready_clean: "Clean"
            case .ready_degraded: "Degraded"
            case .unknown: "Unknown"
            }
        }
        if stale {
            verdict = .init(label: "Verification verdict", value: "Stale", result: .failed)
            priorVerdict = verdictWord.map { "was \($0)" }
        } else if let verdictWord {
            verdict = .init(
                label: "Verification verdict",
                value: verdictWord,
                result: item.readiness?.value1._class == .ready_degraded ? .failed : .passed)
        } else if item._type == .ready_for_final_review {
            verdict = .init(label: "Verification verdict", value: "Unavailable", result: .failed)
        }
        // A moved head leads the Bound-to row with both of its values
        // (frame 7.3), so the row that went stale is one row and no second
        // row repeats the pair. Any other invalidation keeps its own row.
        if let detail {
            let head = "Head \(AttentionDisplay.shortRevision(detail.candidate_head))"
            let base =
                "Base \(detail.base.base_ref)@\(AttentionDisplay.shortRevision(detail.base.base_sha))"
            // The invalidation's own pair, not the detail's coordinate: the
            // divergence is the daemon's fact and the row shows both sides.
            let moved = invalidation.map {
                "\(AttentionDisplay.shortRevision($0.bound)) → \(AttentionDisplay.shortRevision($0.observed))"
            }
            let value: String
            switch (invalidation?.reason, moved) {
            case (.head_changed?, let moved?):
                value = "Head \(moved) · \(base)"
            case (.base_advanced?, _), (.retargeted?, _), (.identity_changed?, _),
                (.head_changed?, nil), (.unknown?, _), (nil, _):
                value = "\(head) · \(base)"
            }
            rows.append(
                .init(
                    label: "Bound to", value: value, result: stale ? .failed : .passed,
                    isStale: stale))
            // The tip the handoff check resolved (plan §7). A later advance
            // of the target marks nothing stale, so this row never does; a
            // detail recorded before the field existed shows no row.
            if let tip = detail.handoff_tip {
                var checked = "Tip \(AttentionDisplay.shortRevision(tip))"
                if let merge = detail.handoff_merge {
                    checked +=
                        " · verified on merge \(AttentionDisplay.shortRevision(merge.merge_sha)) of the head into it"
                }
                rows.append(.init(label: "Checked at", value: checked, result: .passed))
            }
        }
        if let invalidation {
            let movedCoordinateIsBound = detail != nil && invalidation.reason == .head_changed
            if !movedCoordinateIsBound {
                rows.append(
                    .init(
                        label: AttentionDisplay.label(invalidation.reason),
                        value:
                            "\(AttentionDisplay.shortRevision(invalidation.bound)) → "
                            + AttentionDisplay.shortRevision(invalidation.observed),
                        result: .failed, isStale: true))
            }
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
                case .unknown:
                    // An outcome this build doesn't know is never read as a pass.
                    rows.append(.init(label: "Terminal review", value: "Unrecognized outcome", result: .note))
                }
            }
        }
        guard verdict != nil || !rows.isEmpty else { return nil }
        self.verdict = verdict
        // Group by class rather than sorting, so each class keeps the
        // daemon's row order without relying on sort stability.
        self.rows = Result.allCases.flatMap { result in rows.filter { $0.result == result } }
        let countTokens = Result.allCases.compactMap { result -> String? in
            let count = rows.filter { $0.result == result && !$0.isStale }.count
            return count == 0 ? nil : result.countToken(count)
        }
        self.priorVerdict = priorVerdict
        let trailingTokens = [priorVerdict].compactMap { $0 } + countTokens
        countSummary = trailingTokens.joined(separator: " · ")
        let tokens = ([verdict?.value].compactMap { $0 } + trailingTokens)
        verdictLine = tokens.joined(separator: " · ")
        // The spoken label joins the same tokens with commas: a middle dot
        // is a visual separator whose readout depends on the listener's
        // punctuation verbosity.
        summary = "Readiness checklist: " + tokens.joined(separator: ", ") + "."
        accessibilitySummary =
            summary + " "
            + ([verdict].compactMap { $0 } + self.rows).map { row in
                "\(row.label): \(row.value), \(row.accessibilityState)"
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
        case .failed, .not_run, .unknown:
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

    init(rounds: [Round]) {
        self.rounds = rounds
        summary = "Review yield: " + rounds.map(\.text).joined(separator: "; ") + "."
    }

    init?(_ item: Components.Schemas.AttentionItem) {
        guard let history = item.yield_history?.value1 else { return nil }
        self.init(
            rounds: history.rounds.map {
                .init(
                    number: $0.round,
                    newFindings: $0.new_findings,
                    recurringFindings: $0.recurring_findings)
            })
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

    init(specification diff: Components.Schemas.SpecDiff, sinceVersion prior: Int) {
        keyword = "Change Since Version \(prior)"
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

    private static let rowGap = DecisionCardComposition.Scale.moduleGap

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
            .foregroundStyle(row.isStale ? Color.waxText : Color.ink)
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
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    let presentation: DecisionYieldPresentation
    var showsBars = true

    var body: some View {
        VStack(alignment: .leading, spacing: DecisionCardComposition.Scale.moduleGap) {
            KeywordLabel(text: Self.title)
            rounds
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(presentation.summary))
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
                // The counts drop under the round at accessibility sizes
                // (R22); beside it they wrap into a narrow column.
                let layout =
                    dynamicTypeSize.isAccessibilitySize
                    ? AnyLayout(VStackLayout(alignment: .leading, spacing: 3))
                    : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: 12))
                layout {
                    Text("Round \(round.number)")
                        .font(FreesideFont.factLabel)
                        .foregroundStyle(Color.ink)
                    if !dynamicTypeSize.isAccessibilitySize { Spacer(minLength: 0) }
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

/// The two positions of a dispute, the reviewer's over the agent's, each a
/// quote under its own unverified keyword (frame 7.4): both are a model's
/// words about the change, and stacking them gives each the full measure.
struct DecisionComparisonModuleView: View {
    let presentation: DecisionComparisonPresentation
    /// Whether the first position's keyword draws the card's one
    /// explanation control.
    var carriesInfo = false
    var rendersInteractiveControls = true

    var body: some View {
        VStack(alignment: .leading, spacing: DecisionCardComposition.Scale.sectionGap) {
            ForEach(Array(presentation.positions.enumerated()), id: \.element.id) { index, position in
                VStack(alignment: .leading, spacing: DecisionCardComposition.Scale.moduleGap) {
                    UnverifiedLabel(
                        text: position.title, carriesInfo: carriesInfo && index == 0,
                        rendersInteractiveControls: rendersInteractiveControls)
                    QuoteBlock {
                        Text(position.text)
                            .font(FreesideFont.statement)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .accessibilityElement(children: .ignore)
                    .accessibilityAddTraits(.isStaticText)
                    .accessibilityLabel(Text("\(position.title): \(position.text)"))
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

struct StageRail: View {
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
    var labelStyle: LabelStyle = .full

    var body: some View {
        rail
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Text(presentation.summary))
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
    /// semibold; every other title recedes to regular ink-dim, in the
    /// refined scale's 16pt title (frame 5.3).
    private func entryLabel(_ entry: DecisionStageRailPresentation.Entry) -> some View {
        let emphasized = entry.state == .current || entry.state == .failed
        return Text(entry.title)
            .font(FreesideFont.railTitle(emphasized: emphasized))
            .foregroundStyle(entry.state == .failed ? Color.waxText : emphasized ? Color.ink : Color.inkDim)
            .frame(maxWidth: axis == .horizontal ? .infinity : nil, alignment: .leading)
    }
}
