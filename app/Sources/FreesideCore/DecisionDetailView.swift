import FreesideAPI
import SwiftUI

struct TechnicalDetailsRevealRequest: Equatable {
    let itemID: String
    let nonce: UUID

    func retained(for selectedItemID: String?) -> Self? {
        itemID == selectedItemID ? self : nil
    }

    func consuming(_ consumedNonce: UUID) -> Self? {
        nonce == consumedNonce ? nil : self
    }
}

/// One item's self-contained decision card: header, reason, evidence,
/// labeled agent claims, the bindings the decision will commit against,
/// and exactly the item's requested actions. Actions stay disabled until
/// the model's revalidation of current state succeeds.
struct DecisionDetailView: View {
    private enum ScrollTarget: Hashable {
        case technicalDetails
    }

    private struct PendingConfirmation {
        let action: Components.Schemas.Action
        let reviewedSnapshot: Components.Schemas.AttentionItemSnapshot
    }

    private enum ProposalEditor: String, Identifiable {
        case revision
        case snooze
        var id: String { rawValue }
    }

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @State private var model: DecisionModel
    @State private var proposalEditor: ProposalEditor?
    @State private var pendingConfirmation: PendingConfirmation?
    @State private var detailsExpanded: Bool
    @State private var claimsExpanded = false
    @State private var evidenceExpanded = false
    @State private var recommendationVisible = true
    @State private var alternativeSelections: [String: Components.Schemas.AdjudicationRoute] = [:]
    private let attachments: AttachmentLoader
    private let recommendation: DecisionRecommendationPresentation?
    private let graphics: DecisionGraphicPresentations
    private let loadsAttachments: Bool
    private let showsValidationProgress: Bool
    private let itemID: String
    private let detailsRevealRequest: TechnicalDetailsRevealRequest?
    private let onConsumeDetailsRevealRequest: (UUID) -> Void

    @MainActor
    init(
        store: InboxStore,
        itemID: String,
        detailsExpanded: Bool = false,
        detailsRevealRequest: TechnicalDetailsRevealRequest? = nil,
        onConsumeDetailsRevealRequest: @escaping (UUID) -> Void = { _ in },
        recommendation: DecisionRecommendationPresentation? = nil,
        graphics: DecisionGraphicPresentations = .init(),
        loadsAttachments: Bool = true,
        showsValidationProgress: Bool = true
    ) {
        _model = State(initialValue: DecisionModel(store: store, itemID: itemID))
        _detailsExpanded = State(
            initialValue: detailsExpanded || detailsRevealRequest?.itemID == itemID)
        attachments = store.attachments
        self.itemID = itemID
        self.detailsRevealRequest = detailsRevealRequest
        self.onConsumeDetailsRevealRequest = onConsumeDetailsRevealRequest
        self.recommendation = recommendation
        self.graphics = graphics
        self.loadsAttachments = loadsAttachments
        self.showsValidationProgress = showsValidationProgress
    }

    var body: some View {
        platformBody(
            Group {
                if let snapshot = model.snapshot {
                    ScrollViewReader { scrollProxy in
                        ScrollView {
                            card(
                                snapshot.item,
                                accessibilityLayout: isAccessibilityLayout,
                                compactLayout: horizontalSizeClass == .compact
                            )
                            .padding(14)
                            .freesideCard()
                            .padding()
                            .frame(maxWidth: 560, alignment: .leading)
                        }
                        .coordinateSpace(name: "decision-card-scroll")
                        .onChange(of: detailsRevealRequest) {
                            revealTechnicalDetailsIfRequested(using: scrollProxy)
                        }
                        .onAppear {
                            revealTechnicalDetailsIfRequested(using: scrollProxy)
                        }
                    }
                } else {
                    ContentUnavailableView(
                        "Item unavailable",
                        systemImage: "questionmark.circle",
                        description: Text("This attention item is not in the inbox.")
                    )
                }
            }
            // Re-validate on open and whenever the cache is evicted for a new
            // sync epoch (the id carries the store's cache generation), so a
            // card left open across a restore recertifies the re-bootstrapped
            // snapshot instead of sitting on a stale validation (issue #162).
            .task(id: model.revalidationID) { await model.validate() }
            .sheet(item: $proposalEditor) { editor in
                switch editor {
                case .revision:
                    if let facts = model.proposalFacts {
                        RunProposalRevisionSheet(facts: facts) { revision in
                            Task { await model.submitRunProposalRevision(revision) }
                        }
                    }
                case .snooze:
                    RunProposalSnoozeSheet { until in
                        Task { await model.snooze(until: until) }
                    }
                }
            }
            .navigationTitle(model.snapshot.map { AttentionDisplay.title($0.item) } ?? "Decision")
            .confirmationDialog(
                confirmationTitle,
                isPresented: confirmationIsPresented,
                titleVisibility: .visible
            ) {
                if let confirmation = pendingConfirmation {
                    Button(AttentionDisplay.label(confirmation.action), role: .destructive) {
                        pendingConfirmation = nil
                        Task {
                            await model.submitConfirmed(
                                confirmation.action,
                                reviewedSnapshot: confirmation.reviewedSnapshot)
                        }
                    }
                }
                Button("Cancel", role: .cancel) { pendingConfirmation = nil }
            } message: {
                if let confirmation = pendingConfirmation,
                    let consequence = AttentionDisplay.confirmationConsequence(
                        confirmation.action,
                        for: confirmation.reviewedSnapshot.item)
                {
                    Text(consequence)
                }
            }
            .onChange(of: model.snapshot?.item.item_version) {
                pendingConfirmation = nil
            }
        )
    }

    private func revealTechnicalDetailsIfRequested(using scrollProxy: ScrollViewProxy) {
        guard let detailsRevealRequest, detailsRevealRequest.itemID == itemID else { return }
        detailsExpanded = true
        withAnimation {
            scrollProxy.scrollTo(ScrollTarget.technicalDetails, anchor: .top)
        }
        onConsumeDetailsRevealRequest(detailsRevealRequest.nonce)
    }

    @ViewBuilder
    private func platformBody<Content: View>(_ content: Content) -> some View {
        #if os(iOS)
            content.safeAreaInset(edge: .bottom, spacing: 0) {
                if let item = model.snapshot?.item,
                    let recommendation,
                    !recommendationVisible
                {
                    actionButton(
                        recommendation.action,
                        item: item,
                        tone: AttentionDisplay.confirmationConsequence(
                            recommendation.action,
                            for: item) == nil ? .primary : .destructive,
                        showsIcon: false
                    )
                    .padding(.horizontal)
                    .padding(.vertical, 10)
                    .background(.bar)
                }
            }
        #else
            content
        #endif
    }

    private var isAccessibilityLayout: Bool {
        dynamicTypeSize >= .accessibility1
    }

    private var confirmationIsPresented: Binding<Bool> {
        Binding(
            get: { pendingConfirmation != nil },
            set: { presented in
                if !presented { pendingConfirmation = nil }
            })
    }

    private var confirmationTitle: String {
        guard let pendingConfirmation else { return "Confirm action" }
        return "Confirm \(AttentionDisplay.label(pendingConfirmation.action).lowercased())?"
    }

    @ViewBuilder
    private func card(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool = true,
        accessibilityLayout: Bool,
        compactLayout: Bool
    ) -> some View {
        let composition = DecisionCardComposition.forType(item._type)
        VStack(alignment: .leading, spacing: 16) {
            header(item, accessibilityLayout: accessibilityLayout)
            banner
            Text(AttentionDisplay.ask(item))
                .font(FreesideFont.sectionTitle)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)

            ForEach(Array(composition.modules.enumerated()), id: \.offset) {
                index, module in
                cardModule(
                    module,
                    moduleIndex: index,
                    item: item,
                    composition: composition,
                    rendersInteractiveControls: rendersInteractiveControls,
                    accessibilityLayout: accessibilityLayout)
                if index + 1 == composition.actionInsertionIndex {
                    actions(
                        item,
                        stackedLayout: accessibilityLayout || compactLayout,
                        includesReviewing: composition.reviewingActionInsertionIndex == nil)
                }
                if index + 1 == composition.reviewingActionInsertionIndex {
                    reviewingAction(item)
                }
            }
        }
    }

    @ViewBuilder
    private func cardModule(
        _ module: DecisionCardModule,
        moduleIndex: Int,
        item: Components.Schemas.AttentionItem,
        composition: DecisionCardComposition,
        rendersInteractiveControls: Bool,
        accessibilityLayout: Bool
    ) -> some View {
        switch module {
        case .recommendation:
            if let recommendation,
                actionRanking(item).recommended == recommendation.action
            {
                recommendationBlock(recommendation, item: item)
            }
        case .checklist:
            if let presentation = DecisionChecklistPresentation(item) {
                DecisionChecklistModuleView(presentation: presentation)
            }
        case .stageRail:
            if let presentation = graphics.stageRail {
                cardSection("Failure stage") {
                    StageRail(
                        title: nil,
                        presentation: presentation,
                        axis: accessibilityLayout ? .vertical : .horizontal)
                }
            }
        case .comparison:
            if let presentation = graphics.comparison {
                DecisionComparisonModuleView(presentation: presentation)
            }
        case .yieldChart:
            if let presentation = graphics.diminishingYield ?? DecisionYieldPresentation(item) {
                DecisionYieldChartModuleView(
                    presentation: presentation,
                    showsBars: graphics.diminishingYield != nil)
            }
        case .factBlock:
            factBlocks(
                item,
                includesCommitPlan: !composition.modules.contains(.checklist),
                rendersInteractiveControls: rendersInteractiveControls)
        case .claims:
            claims(
                composition.claims(
                    from: item.agent_claims,
                    at: moduleIndex,
                    prominentClaimIndex: graphics.prominentClaimIndex),
                accessibilityLayout: accessibilityLayout,
                prominent: composition.claimsAreProminent(at: moduleIndex))
        case .evidence:
            evidence(item, accessibilityLayout: accessibilityLayout)
        case .details:
            details(item, accessibilityLayout: accessibilityLayout)
        }
    }

    @ViewBuilder
    private func factBlocks(
        _ item: Components.Schemas.AttentionItem,
        includesCommitPlan: Bool,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let changeSummary = graphics.changeSummary {
            cardSection("Change summary (unverified)", dashed: true) {
                Text(changeSummary.text)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Text(changeSummary.summary))
        }

        cardSection("Context") {
            Text(item.reason)
                .fixedSize(horizontal: false, vertical: true)
        }

        if let adjudication = item.finding_adjudication?.value1 {
            findingAdjudication(
                adjudication,
                rendersInteractiveControls: rendersInteractiveControls)
        }

        if includesCommitPlan, let notice = item.commit_plan_notice?.value1 {
            cardSection("Facts") {
                factRow("Commit plan", value: AttentionDisplay.label(notice))
            }
        }

        if let comparison = graphics.comparison, !comparison.verifiableFacts.isEmpty {
            cardSection("What the daemon can verify") {
                ForEach(comparison.verifiableFacts) { fact in
                    factRow(fact.label, value: fact.value)
                }
            }
        }

        if let attemptTimings = graphics.attemptTimings {
            cardSection(attemptTimings.title) {
                ForEach(attemptTimings.facts) { fact in
                    factRow(fact.label, value: fact.value)
                }
            }
        }

        if let facts = model.proposalFacts {
            cardSection("Authenticated proposal") {
                factRow("Intent", value: facts.intent.rawValue)
                factRow("Expected cost", value: "\(facts.expected_cost_units) units")
                factRow("Components", value: "\(facts.scope.component_count)")
                factRow("Declared paths", value: "\(facts.scope.declared_path_count)")
                factRow(
                    "Control plane", value: facts.scope.touches_control_plane ? "Yes" : "No")
                if let prior = facts.supersedes?.value1 {
                    Divider()
                    Text("Revision context")
                        .font(FreesideFont.sans(.caption, weight: .semibold))
                    proposalRevisionRows(prior)
                }
            }
        }
    }

    @ViewBuilder
    private func claims(
        _ claims: [Components.Schemas.AgentClaim],
        accessibilityLayout: Bool,
        prominent: Bool
    ) -> some View {
        if !claims.isEmpty {
            if prominent {
                cardSection("Agent claims (unverified)", dashed: true) {
                    claimRows(claims)
                }
            } else {
                lowerSection(
                    "Agent claims (unverified)",
                    isExpanded: $claimsExpanded,
                    accessibilityLayout: accessibilityLayout,
                    dashed: true
                ) {
                    claimRows(claims)
                }
            }
        }
    }

    @ViewBuilder
    private func claimRows(_ claims: [Components.Schemas.AgentClaim]) -> some View {
        Text("Written by the agent, not checked by the daemon.")
            .foregroundStyle(Color.inkDim)
        // Position is the only stable identity: two claims may bind the same
        // artifact under different labels and neither field is unique.
        ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
            AttachmentRow(
                label: claim.label, digest: claim.digest,
                attachments: attachments,
                loadsAttachments: loadsAttachments,
                text: claim.text)
        }
    }

    @ViewBuilder
    private func evidence(
        _ item: Components.Schemas.AttentionItem,
        accessibilityLayout: Bool
    ) -> some View {
        if !item.evidence_snapshot.isEmpty {
            lowerSection(
                "Evidence",
                isExpanded: $evidenceExpanded,
                accessibilityLayout: accessibilityLayout
            ) {
                ForEach(item.evidence_snapshot, id: \.id) { artifact in
                    AttachmentRow(
                        label: artifact._type.rawValue, digest: artifact.digest,
                        attachments: attachments,
                        loadsAttachments: loadsAttachments)
                }
            }
        }
    }

    private func details(
        _ item: Components.Schemas.AttentionItem,
        accessibilityLayout: Bool
    ) -> some View {
        lowerSection(
            "Details",
            isExpanded: $detailsExpanded,
            accessibilityLayout: accessibilityLayout
        ) {
            VStack(alignment: .leading, spacing: 6) {
                ForEach(Array(detailRows(item).enumerated()), id: \.offset) { _, row in
                    factRow(row.label, value: row.value, monospaced: true)
                }
            }
        }
        .id(ScrollTarget.technicalDetails)
        .font(FreesideFont.caption)
        .foregroundStyle(Color.inkDim)
        .textSelection(.enabled)
    }

    /// The project-owned card content without navigation and presentation
    /// containers that ImageRenderer cannot draw off-screen on macOS.
    @ViewBuilder
    func screenshotCard(
        _ item: Components.Schemas.AttentionItem,
        at dynamicTypeSize: DynamicTypeSize,
        compactLayout: Bool = false
    ) -> some View {
        card(
            item,
            rendersInteractiveControls: false,
            accessibilityLayout: dynamicTypeSize >= .accessibility1,
            compactLayout: compactLayout
        )
        .padding(14)
        .freesideCard()
        .padding()
        .frame(maxWidth: 560, alignment: .leading)
    }

    @ViewBuilder
    private func findingAdjudication(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        rendersInteractiveControls: Bool
    ) -> some View {
        ForEach(binding.proposals, id: \.finding_id) { proposal in
            let producer = AttentionDisplay.adjudicationProducerPresentation(proposal.producer)
            cardSection(
                producer.label,
                dashed: producer.modelBacked
            ) {
                factRow("Finding", value: proposal.finding_id)
                factRow("Recommended route", value: AttentionDisplay.label(proposal.route))
                factRow(
                    "Goal relationship", value: AttentionDisplay.label(proposal.goal_relationship))
                factRow(
                    "Work-unit compatibility",
                    value: AttentionDisplay.label(proposal.compatibility?.value1))
                if let confidence = proposal.confidence?.value1 {
                    factRow("Proposal confidence", value: AttentionDisplay.label(confidence))
                }
                Text(proposal.rationale)
                    .fixedSize(horizontal: false, vertical: true)
            }

            cardSection("Daemon facts") {
                factRow("Binding digest", value: binding.adjudication_digest)
                factRow("Run", value: binding.run_id)
                factRow("Round", value: "\(binding.round)")
            }

            if !proposal.assumptions.isEmpty {
                findingList("Assumptions", values: proposal.assumptions)
            }
            if !proposal.cited_rules.isEmpty {
                findingList("Cited repository rules", values: proposal.cited_rules)
            }
            if !proposal.offered_alternatives.isEmpty {
                cardSection("Viable alternatives") {
                    ForEach(proposal.offered_alternatives, id: \.route) { alternative in
                        VStack(alignment: .leading, spacing: 3) {
                            Text(AttentionDisplay.label(alternative.route))
                                .font(FreesideFont.sans(.callout, weight: .semibold))
                            Text(alternative.consequence)
                        }
                    }
                    if rendersInteractiveControls {
                        Picker(
                            "Selected route",
                            selection: alternativeSelection(for: proposal)
                        ) {
                            Text("Keep recommendation")
                                .tag(Optional<Components.Schemas.AdjudicationRoute>.none)
                            ForEach(proposal.offered_alternatives, id: \.route) { alternative in
                                Text(AttentionDisplay.label(alternative.route))
                                    .tag(Optional(alternative.route))
                            }
                        }
                        .pickerStyle(.menu)
                    } else {
                        factRow("Selected route", value: "Keep recommendation")
                    }
                }
            }
            if !proposal.open_questions.isEmpty {
                findingList("Gating questions", values: proposal.open_questions)
            }
        }
    }

    private func findingList(_ title: String, values: [String]) -> some View {
        cardSection(title) {
            ForEach(Array(values.enumerated()), id: \.offset) { _, value in
                Label(value, systemImage: "circle.fill")
                    .labelStyle(FindingListLabelStyle())
            }
        }
    }

    private func alternativeSelection(
        for proposal: Components.Schemas.FindingAdjudicationProposal
    ) -> Binding<Components.Schemas.AdjudicationRoute?> {
        Binding(
            get: { alternativeSelections[proposal.finding_id] },
            set: { route in
                if let route {
                    alternativeSelections[proposal.finding_id] = route
                } else {
                    alternativeSelections.removeValue(forKey: proposal.finding_id)
                }
            }
        )
    }

    private func selectedAlternatives(
        _ binding: Components.Schemas.FindingAdjudicationBinding
    ) -> [Components.Schemas.AlternativeChoice] {
        Self.selectedAlternatives(binding, selections: alternativeSelections)
    }

    static func selectedAlternatives(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        selections: [String: Components.Schemas.AdjudicationRoute]
    ) -> [Components.Schemas.AlternativeChoice] {
        binding.proposals.compactMap { proposal in
            guard let route = selections[proposal.finding_id] else { return nil }
            return .init(finding_id: proposal.finding_id, route: route)
        }
    }

    private func actionInputReady(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem
    ) -> Bool {
        guard action == .choose_alternative_route else { return true }
        guard let binding = item.finding_adjudication?.value1 else { return false }
        return !selectedAlternatives(binding).isEmpty
    }

    private func detailRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [AttentionDisplay.BindingRow] {
        var rows = AttentionDisplay.detailBindingRows(
            item,
            priorProposalDigest: model.proposalFacts?.supersedes?.value1.proposal_digest,
            proposalDigest: model.proposalFacts?.proposal_digest
        )
        rows.append(
            contentsOf: actionRanking(item).unavailable.map {
                .init(
                    label: "Requested, not available here",
                    value: AttentionDisplay.label($0))
            })
        return rows
    }

    @ViewBuilder
    private func proposalRevisionRows(
        _ prior: Components.Schemas.RunProposalRevisionFacts
    ) -> some View {
        factRow("Prior intent", value: prior.intent.rawValue)
        factRow("Prior cost", value: "\(prior.expected_cost_units) units")
        factRow(
            "Prior scope",
            value: "\(prior.scope.component_count) components, \(prior.scope.declared_path_count) paths")
        factRow(
            "Prior control plane", value: prior.scope.touches_control_plane ? "Yes" : "No")
    }

    @ViewBuilder
    private func header(
        _ item: Components.Schemas.AttentionItem,
        accessibilityLayout: Bool
    ) -> some View {
        headerBadges(item, accessibilityLayout: accessibilityLayout)
    }

    @ViewBuilder
    private func headerBadges(
        _ item: Components.Schemas.AttentionItem,
        accessibilityLayout: Bool
    ) -> some View {
        let layout =
            accessibilityLayout
            ? AnyLayout(VStackLayout(alignment: .leading, spacing: 4))
            : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: 8))
        layout {
            PriorityBadge(priority: item.priority)
            if let posture = item.posture?.value1 {
                HealthPostureBadge(posture: posture)
            }
            StatusBadge(status: item.status)
        }
    }

    @ViewBuilder
    private var banner: some View {
        if model.phase == .superseded {
            bannerLabel(
                "This item changed before your decision applied. Nothing was committed; re-review the replacement below.",
                systemImage: "arrow.triangle.2.circlepath",
                tint: .accentText, wash: .accentWash
            )
        } else {
            // An applied record persists even when the item stays open
            // (a non-resolving action such as acknowledge or open_pr).
            if let record = model.appliedRecord {
                // Success is quiet: a plain tick on a neutral wash, never
                // green and never the accent.
                bannerLabel(
                    "Decision applied: \(AttentionDisplay.label(record.action))",
                    systemImage: "checkmark",
                    tint: .inkDim, wash: .neutralWash
                )
            }
            // The retry affordance leads: when a preserved command may
            // hold a recorded result, resending it is the actionable
            // step, whatever else failed.
            if model.canRetryLostResponse {
                VStack(alignment: .leading, spacing: 8) {
                    bannerLabel(
                        "The response was lost; the decision may already be recorded.",
                        systemImage: "arrow.clockwise",
                        tint: .accentText, wash: .accentWash
                    )
                    Button("Retry") {
                        Task { await model.retryLostResponse() }
                    }
                    .buttonStyle(FreesideActionButtonStyle(tone: .primary))
                }
            } else if case .failed(let message) = model.validation {
                bannerLabel(
                    "Couldn't validate current state: \(message)",
                    systemImage: "exclamationmark",
                    tint: .waxText, wash: .waxWash
                )
            } else if let message = model.submissionError {
                bannerLabel(
                    "Submission failed: \(message)",
                    systemImage: "exclamationmark",
                    tint: .waxText, wash: .waxWash
                )
            }
        }
    }

    private func recommendationBlock(
        _ recommendation: DecisionRecommendationPresentation,
        item: Components.Schemas.AttentionItem
    ) -> some View {
        cardSection(
            "Recommended",
            border: .accentBorder,
            fill: .accentWash
        ) {
            KeywordLabel(text: "Why")
            Text(recommendation.reason)
                .fixedSize(horizontal: false, vertical: true)
            if let confidence = recommendation.confidence {
                factRow("Confidence", value: confidence)
            }
            actionButton(
                recommendation.action,
                item: item,
                tone: AttentionDisplay.confirmationConsequence(
                    recommendation.action,
                    for: item) == nil ? .primary : .destructive,
                showsIcon: false
            )
            .padding(.top, 4)
        }
        .onGeometryChange(for: Bool.self) { geometry in
            geometry.frame(in: .named("decision-card-scroll")).maxY > 0
        } action: { visible in
            recommendationVisible = visible
        }
    }

    private func cardSection(
        _ title: String,
        dashed: Bool = false,
        border: Color = .rule,
        fill: Color = .ground,
        @ViewBuilder content: () -> some View
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: title)
            content()
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(fill))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(
                    border,
                    style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : []))
        )
    }

    @ViewBuilder
    private func lowerSection<Content: View>(
        _ title: String,
        isExpanded: Binding<Bool>,
        accessibilityLayout: Bool,
        dashed: Bool = false,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        if accessibilityLayout {
            DisclosureGroup(isExpanded: isExpanded) {
                VStack(alignment: .leading, spacing: 8) {
                    content()
                }
                .padding(.top, 8)
            } label: {
                KeywordLabel(text: title)
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .freesideCard(dashed: dashed)
        } else {
            cardSection(title, dashed: dashed) {
                content()
            }
        }
    }

    private func factRow(
        _ label: String,
        value: String,
        monospaced: Bool = false
    ) -> some View {
        DecisionFactRow(label: label, value: value, monospaced: monospaced)
    }

    /// One labeled attachment row. Plan §9's presentation layers supersede
    /// the earlier digest-leading choice: content stays in the evidence layer,
    /// while its binding digest appears once in the card's collapsed details.
    /// A text claim renders its daemon-verified inline content directly;
    /// otherwise fetched image bytes render inline and a failed fetch gets a
    /// placeholder. Attachment bytes remain memory-only.
    private struct AttachmentRow: View {
        let label: String
        let digest: String
        let attachments: AttachmentLoader
        let loadsAttachments: Bool
        var text: Components.Schemas.ClaimText? = nil

        var body: some View {
            VStack(alignment: .leading, spacing: 6) {
                Text(label)
                    .font(FreesideFont.sans(.callout, weight: .semibold))
                if let text {
                    // No accessibility override: VoiceOver must hear the
                    // content, and the visible label already names the claim.
                    claimText(text)
                        .font(FreesideFont.callout)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                } else {
                    fetchedAttachment
                        .task(id: digest) {
                            if loadsAttachments { await attachments.load(digest) }
                        }
                }
            }
        }

        private func claimText(_ text: Components.Schemas.ClaimText) -> Text {
            switch text.media_type {
            case .text_sol_plain:
                return Text(text.content)
            case .text_sol_markdown:
                // Inline-only interpretation keeps a summary a compact
                // paragraph; unparsable markdown falls back to the raw
                // content rather than dropping the claim's body.
                let attributed = try? AttributedString(
                    markdown: text.content,
                    options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))
                return Text(attributed ?? AttributedString(text.content))
            }
        }

        @ViewBuilder
        private var fetchedAttachment: some View {
            switch attachments.phase(for: digest) {
            case .image(let image):
                platformImage(image)
                    .resizable()
                    .scaledToFit()
                    .frame(maxWidth: 320, alignment: .leading)
                    .clipShape(RoundedRectangle(cornerRadius: 6))
                    .accessibilityLabel("\(label) attachment image")
            case .unavailable:
                Label("Attachment unavailable", systemImage: "photo.badge.exclamationmark")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
            case .loading, .notImage, nil:
                EmptyView()
            }
        }

        private func platformImage(_ image: PlatformImage) -> Image {
            #if canImport(UIKit)
                Image(uiImage: image)
            #elseif canImport(AppKit)
                Image(nsImage: image)
            #endif
        }
    }

    /// A card banner: tinted wash, glyph and message in the state color.
    private func bannerLabel(_ text: String, systemImage: String, tint: Color, wash: Color) -> some View {
        Label {
            Text(text)
        } icon: {
            Image(systemName: systemImage)
                .font(.system(size: 10, weight: .semibold))
        }
        .font(FreesideFont.callout)
        .textSelection(.enabled)
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(wash, in: RoundedRectangle(cornerRadius: 8))
        .foregroundStyle(tint)
    }

    @ViewBuilder
    private func actions(
        _ item: Components.Schemas.AttentionItem,
        stackedLayout: Bool,
        includesReviewing: Bool
    ) -> some View {
        let ranking = actionRanking(item)
        VStack(alignment: .leading, spacing: 8) {
            if showsValidationProgress && model.validation == .pending {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small).tint(.waterText)
                    Text("Validating current state…")
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(Color.inkDim)
                }
            }

            if !ranking.principal.isEmpty {
                // Keyed by position: requested_decision does not enforce
                // uniqueness, and duplicate identities may not drop a button.
                if stackedLayout {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(Array(ranking.principal.enumerated()), id: \.offset) { _, action in
                            actionButton(action, item: item, tone: .neutral)
                        }
                    }
                } else {
                    HStack(alignment: .top, spacing: 8) {
                        ForEach(Array(ranking.principal.enumerated()), id: \.offset) { _, action in
                            actionButton(action, item: item, tone: .neutral)
                        }
                    }
                }
            }

            if includesReviewing, let reviewing = ranking.reviewing {
                actionButton(reviewing, item: item, tone: .neutral)
            }

            overflowMenu(ranking.overflow, item: item)

            if ranking.notDecidableHere {
                bannerLabel(
                    "This decision needs a written answer, and this build cannot carry one. Nothing is blocked by opening it; the item stays open until answered.",
                    systemImage: "exclamationmark.bubble",
                    tint: .accentText,
                    wash: .accentWash
                )
            }
            if item._type == .blocked {
                Text("A blocked item is informational; it resolves when the external wait clears.")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
            }
        }
    }

    @ViewBuilder
    private func reviewingAction(_ item: Components.Schemas.AttentionItem) -> some View {
        if let reviewing = actionRanking(item).reviewing {
            actionButton(reviewing, item: item, tone: .neutral)
        }
    }

    private func actionRanking(
        _ item: Components.Schemas.AttentionItem
    ) -> DecisionActionRanking {
        let composition = DecisionCardComposition.forType(item._type)
        return DecisionActionRanking(
            requested: item.requested_decision,
            recommendedAction: recommendation?.action,
            reservesRecommendedAction: composition.modules.contains(.recommendation))
    }

    @ViewBuilder
    private func overflowMenu(
        _ actions: [Components.Schemas.Action],
        item: Components.Schemas.AttentionItem
    ) -> some View {
        if !actions.isEmpty {
            let ordinary = actions.filter {
                AttentionDisplay.confirmationConsequence($0, for: item) == nil
            }
            let consequential = actions.filter {
                AttentionDisplay.confirmationConsequence($0, for: item) != nil
            }
            Menu {
                ForEach(Array(ordinary.enumerated()), id: \.offset) { _, action in
                    Button {
                        trigger(action, item: item)
                    } label: {
                        actionLabel(action)
                    }
                }
                if !ordinary.isEmpty, !consequential.isEmpty {
                    Divider()
                }
                ForEach(Array(consequential.enumerated()), id: \.offset) { _, action in
                    Button(role: .destructive) {
                        trigger(action, item: item)
                    } label: {
                        actionLabel(action)
                    }
                }
            } label: {
                Text("More")
                    .frame(maxWidth: .infinity)
            }
            .menuStyle(.button)
            .buttonStyle(FreesideActionButtonStyle(tone: .neutral))
            .disabled(!model.actionsEnabled)
            .accessibilityLabel("More decision actions")
        }
    }

    private func actionButton(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem,
        tone: FreesideActionButtonStyle.Tone,
        showsIcon: Bool = true
    ) -> some View {
        Button {
            trigger(action, item: item)
        } label: {
            HStack {
                actionLabel(action, showsIcon: showsIcon)
                if model.phase == .submitting(action) {
                    ProgressView().controlSize(.small)
                }
            }
            .frame(maxWidth: .infinity)
        }
        .buttonStyle(FreesideActionButtonStyle(tone: tone))
        .disabled(
            !model.actionsEnabled || !model.isSubmittable(action)
                || !actionInputReady(action, item: item))
    }

    @ViewBuilder
    private func actionLabel(
        _ action: Components.Schemas.Action,
        showsIcon: Bool = true
    ) -> some View {
        if showsIcon, let systemImage = AttentionDisplay.systemImage(action) {
            Label(AttentionDisplay.label(action), systemImage: systemImage)
        } else {
            Text(AttentionDisplay.label(action))
        }
    }

    private func trigger(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem
    ) {
        if AttentionDisplay.confirmationConsequence(action, for: item) != nil {
            guard let snapshot = model.snapshot,
                snapshot.item.id == item.id,
                snapshot.item.item_version == item.item_version
            else { return }
            pendingConfirmation = PendingConfirmation(
                action: action,
                reviewedSnapshot: snapshot)
        } else {
            perform(action, item: item)
        }
    }

    private func perform(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem?
    ) {
        switch action {
        case .start_with_changes:
            proposalEditor = .revision
        case .snooze:
            proposalEditor = .snooze
        case .choose_alternative_route:
            guard let binding = item?.finding_adjudication?.value1 else { return }
            Task { await model.submitFindingAlternatives(selectedAlternatives(binding)) }
        default:
            Task { await model.submit(action) }
        }
    }
}

private struct DecisionFactRow: View {
    let label: String
    let value: String
    let monospaced: Bool
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if dynamicTypeSize >= .accessibility1 {
                VStack(alignment: .leading, spacing: 2) {
                    Text(label)
                        .foregroundStyle(Color.inkDim)
                    Text(value)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                }
            } else {
                LabeledContent(label) {
                    Text(value)
                        .multilineTextAlignment(.trailing)
                }
            }
        }
        .font(monospaced ? FreesideFont.monoCaption : FreesideFont.callout)
        .foregroundStyle(monospaced ? Color.inkDim : Color.ink)
    }
}

private struct FindingListLabelStyle: LabelStyle {
    func makeBody(configuration: Configuration) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            configuration.icon
                .font(.system(size: 4))
                .foregroundStyle(Color.inkDim)
            configuration.title
        }
    }
}

private struct RunProposalRevisionSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var expectedCost: Int
    @State private var componentCount: Int
    @State private var declaredPathCount: Int
    @State private var touchesControlPlane: Bool
    private let originalFacts: Components.Schemas.RunProposalFactsSnapshot
    let submit: (Components.Schemas.RunProposalRevisionInput) -> Void

    init(
        facts: Components.Schemas.RunProposalFactsSnapshot,
        submit: @escaping (Components.Schemas.RunProposalRevisionInput) -> Void
    ) {
        _expectedCost = State(initialValue: facts.expected_cost_units)
        _componentCount = State(initialValue: facts.scope.component_count)
        _declaredPathCount = State(initialValue: facts.scope.declared_path_count)
        _touchesControlPlane = State(initialValue: facts.scope.touches_control_plane)
        originalFacts = facts
        self.submit = submit
    }

    private var changesProposal: Bool {
        expectedCost != originalFacts.expected_cost_units
            || componentCount != originalFacts.scope.component_count
            || declaredPathCount != originalFacts.scope.declared_path_count
            || touchesControlPlane != originalFacts.scope.touches_control_plane
    }

    var body: some View {
        NavigationStack {
            Form {
                LabeledContent("Intent", value: "Implement subject")
                Stepper("Expected cost: \(expectedCost) units", value: $expectedCost, in: 1...1_000_000)
                Stepper("Components: \(componentCount)", value: $componentCount, in: 1...32)
                Stepper("Declared paths: \(declaredPathCount)", value: $declaredPathCount, in: 1...4096)
                Toggle("Touches control plane", isOn: $touchesControlPlane)
            }
            .navigationTitle("Start with changes")
            .tint(.accentText)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Submit") {
                        submit(
                            .init(
                                intent: .implement_subject,
                                expected_cost_units: expectedCost,
                                scope: .init(
                                    component_count: componentCount,
                                    declared_path_count: declaredPathCount,
                                    touches_control_plane: touchesControlPlane)))
                        dismiss()
                    }
                    .disabled(!changesProposal)
                }
            }
        }
        .frame(minWidth: 380, minHeight: 280)
    }
}

private struct RunProposalSnoozeSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var until = Date().addingTimeInterval(60 * 60)
    let submit: (Date) -> Void

    var body: some View {
        NavigationStack {
            Form {
                DatePicker(
                    "Snooze until", selection: $until, in: Date()..., displayedComponents: [.date, .hourAndMinute])
            }
            .navigationTitle("Snooze proposal")
            .tint(.accentText)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Snooze") {
                        submit(until)
                        dismiss()
                    }
                }
            }
        }
        .frame(minWidth: 380, minHeight: 220)
    }
}

struct HealthPostureBadge: View {
    let posture: Components.Schemas.HealthPosture

    var body: some View {
        StateChip(label: AttentionDisplay.label(posture), color: color)
    }

    private var color: Color {
        switch posture {
        case .blocking: return .waxText
        case .advisory: return .inkDim
        }
    }
}
