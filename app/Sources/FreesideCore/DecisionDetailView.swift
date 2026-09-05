import Foundation
import FreesideAPI
import SwiftUI

#if os(iOS)
    import UIKit
#elseif os(macOS)
    import AppKit
#endif

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

    private enum MessageEditor: String, Identifiable {
        case discuss
        case requestChanges
        case answerAndRetry
        case answerWithoutRetry
        case returnToAgent
        var id: String { rawValue }
    }

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @ScaledMetric(relativeTo: .callout) private var bannerGlyphSize: CGFloat = screenshotMetricBase(
        10, relativeTo: .callout)
    @State private var model: DecisionModel
    @State private var proposalEditor: ProposalEditor?
    @State private var messageEditor: MessageEditor?
    @State private var specApprovalReader: SpecApprovalReader?
    @State private var pendingConfirmation: PendingConfirmation?
    @State private var capabilityRetrySnapshot: Components.Schemas.AttentionItemSnapshot?
    @State private var sectionPreferences: DecisionSectionPreferences
    @State private var inspectorPresented: Bool
    @State private var detailWidth: CGFloat = 0
    @State private var recommendationVisible = true
    @State private var provenanceExpanded = false
    @State private var lostResponseExpanded = false
    @State private var alternativeSelections: [String: Components.Schemas.AdjudicationRoute] = [:]
    /// The finding rows the operator opened, by finding id. Empty by default:
    /// a finding_adjudication card leads with collapsed rows so its actions
    /// stay in the first viewport (#1107).
    @State private var expandedFindings: Set<String>
    private let attachments: AttachmentLoader
    private let graphics: DecisionGraphicPresentations
    private let loadsAttachments: Bool
    private let showsValidationProgress: Bool
    private let now: Date
    private let itemID: String
    private let detailsRevealRequest: TechnicalDetailsRevealRequest?
    private let onConsumeDetailsRevealRequest: (UUID) -> Void
    private let externalInspectorPresented: Binding<Bool>?
    private let onSelectItem: (String) -> Void

    @MainActor
    init(
        store: InboxStore,
        itemID: String,
        detailsExpanded: Bool = false,
        expandedFindings: Set<String> = [],
        detailsRevealRequest: TechnicalDetailsRevealRequest? = nil,
        onConsumeDetailsRevealRequest: @escaping (UUID) -> Void = { _ in },
        graphics: DecisionGraphicPresentations = .init(),
        loadsAttachments: Bool = true,
        showsValidationProgress: Bool = true,
        now: Date = .now,
        sectionPreferences: DecisionSectionPreferences? = nil,
        inspectorPresented: Binding<Bool>? = nil,
        onSelectItem: @escaping (String) -> Void = { _ in },
        onConclusion: @escaping @MainActor (DecisionConclusion) -> Void = { _ in }
    ) {
        _model = State(
            initialValue: DecisionModel(
                store: store, itemID: itemID, onConclusion: onConclusion))
        let revealsTechnicalDetails =
            detailsExpanded || detailsRevealRequest?.itemID == itemID
        _sectionPreferences = State(
            initialValue: sectionPreferences
                ?? DecisionSectionPreferences(
                    detailsExpandedOverride: revealsTechnicalDetails ? true : nil))
        _inspectorPresented = State(initialValue: revealsTechnicalDetails)
        _expandedFindings = State(initialValue: expandedFindings)
        attachments = store.attachments
        self.itemID = itemID
        self.detailsRevealRequest = detailsRevealRequest
        self.onConsumeDetailsRevealRequest = onConsumeDetailsRevealRequest
        externalInspectorPresented = inspectorPresented
        self.onSelectItem = onSelectItem
        self.graphics = graphics
        self.loadsAttachments = loadsAttachments
        self.showsValidationProgress = showsValidationProgress
        self.now = now
    }

    var body: some View {
        platformBody(
            Group {
                if let snapshot = model.snapshot {
                    ScrollViewReader { scrollProxy in
                        ScrollView {
                            card(
                                snapshot.item,
                                proposalFacts: model.proposalFacts,
                                accessibilityLayout: isAccessibilityLayout,
                                compactLayout: horizontalSizeClass == .compact,
                                wideLayout: usesWideLayout,
                                inspectorPresented: inspectorBinding.wrappedValue
                            )
                            .padding(14)
                            .freesideCard()
                            .padding()
                            .frame(
                                maxWidth: usesWideLayout ? 1_040 : 560,
                                alignment: .topLeading)
                        }
                        .coordinateSpace(name: "decision-card-scroll")
                        .onGeometryChange(for: CGFloat.self) { geometry in
                            geometry.size.width
                        } action: { width in
                            detailWidth = width
                        }
                        .onChange(of: detailsRevealRequest) {
                            revealTechnicalDetailsIfRequested(using: scrollProxy)
                        }
                        .onAppear {
                            revealTechnicalDetailsIfRequested(using: scrollProxy)
                        }
                    }
                } else {
                    UnavailableStateView(
                        title: "Item unavailable",
                        systemImage: "questionmark.circle",
                        description: "This attention item is not in the inbox.")
                }
            }
            // Re-validate on open and whenever the cache is evicted for a new
            // sync epoch (the id carries the store's cache generation), so a
            // card left open across a restore recertifies the re-bootstrapped
            // snapshot instead of sitting on a stale validation (issue #162).
            .task(id: model.revalidationID) {
                // Record card_opened the moment the card is on screen, before
                // validation and the action-surface fetch, so open-to-decision
                // includes their latency and a fast resolve-and-leave still
                // records the open (plan §8, §9).
                model.emitCardOpened()
                await model.validate()
                // Fetch the device's action surface separately, after the open
                // is recorded (plan §8).
                await model.refreshActionSurface()
            }
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
            .sheet(item: $messageEditor) { editor in
                switch editor {
                case .discuss:
                    MessageComposerSheet(
                        title: "Discuss",
                        prompt: "Send a message to the agent. The item stays open while it replies.",
                        submitLabel: "Send"
                    ) { message in
                        await model.submitDiscuss(message: message)
                    }
                case .requestChanges:
                    MessageComposerSheet(
                        title: "Request changes",
                        prompt: "Describe the revision the specification needs.",
                        submitLabel: "Request changes",
                        byteLimit: 8192
                    ) { message in
                        await model.submitRequestChanges(message: message)
                    }
                case .answerAndRetry:
                    MessageComposerSheet(
                        title: "Answer and retry",
                        prompt: "Answer the agent's question and retry the blocked work.",
                        submitLabel: "Answer and retry", byteLimit: 8192
                    ) { message in
                        await model.submitAnswer(
                            .answer_and_retry, message: message,
                            answerRoute: AgentQuestionPresentation.answerRoute(for: model.snapshot?.item))
                    }
                case .answerWithoutRetry:
                    MessageComposerSheet(
                        title: "Answer without retry",
                        prompt: "Record the answer and conclude the question without restarting work.",
                        submitLabel: "Record answer", byteLimit: 8192
                    ) { message in
                        await model.submitAnswer(.answer_without_retry, message: message)
                    }
                case .returnToAgent:
                    MessageComposerSheet(
                        title: "Return to agent",
                        prompt: "Describe what the agent should change before the work returns for review.",
                        submitLabel: "Return to agent", byteLimit: 8192
                    ) { message in
                        await model.submitReturnToAgent(message: message)
                    }
                }
            }
            .confirmationDialog(
                "Choose retry capabilities",
                isPresented: capabilityRetryIsPresented,
                titleVisibility: .visible
            ) {
                if let reviewedSnapshot = capabilityRetrySnapshot {
                    ForEach(
                        reviewedSnapshot.item.execution_failure?.value1.offered_manifests ?? [],
                        id: \.digest
                    ) { manifest in
                        Button("\(manifest.name) · \(manifest.egress_profile.rawValue)") {
                            capabilityRetrySnapshot = nil
                            Task {
                                await model.submitCapabilityRetry(
                                    manifestDigest: manifest.digest,
                                    reviewedSnapshot: reviewedSnapshot)
                            }
                        }
                    }
                }
                Button("Cancel", role: .cancel) { capabilityRetrySnapshot = nil }
            } message: {
                Text("The daemon will verify the selected manifest again before admission.")
            }
            #if os(iOS)
                .sheet(item: $specApprovalReader) { reader in
                    if let item = model.snapshot?.item {
                        specApprovalReaderSheet(reader, item: item)
                    }
                }
            #endif
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
                capabilityRetrySnapshot = nil
                // A new item version can carry a different recommendation. The
                // sticky action would otherwise stay reachable from the last
                // version's scroll position, offering a replacement action
                // whose reason and provenance were never on screen.
                recommendationVisible = true
            }
            #if os(macOS)
                .focusedSceneValue(\.decisionCommandActions, focusedDecisionCommandActions)
                .onExitCommand { cancelPendingAction() }
                // Return takes a validated recommendation and otherwise
                // yields the responder chain: with nothing to take there is
                // nothing to announce, and swallowing the key to post an
                // unavailability banner made every card without a
                // recommendation answer Return with a notice.
                .onKeyPress(.return) {
                    guard canTakeRecommendation else { return .ignored }
                    takeRecommendationFromKeyboard()
                    return .handled
                }
            #endif
        )
    }

    private var inspectorBinding: Binding<Bool> {
        externalInspectorPresented ?? $inspectorPresented
    }

    private func revealTechnicalDetailsIfRequested(using scrollProxy: ScrollViewProxy) {
        guard let detailsRevealRequest, detailsRevealRequest.itemID == itemID else { return }
        detailsExpanded.wrappedValue = true
        model.emitDetailsOpenedBeforeActing()
        #if os(macOS)
            inspectorBinding.wrappedValue = true
        #else
            withAnimation {
                scrollProxy.scrollTo(ScrollTarget.technicalDetails, anchor: .top)
            }
            onConsumeDetailsRevealRequest(detailsRevealRequest.nonce)
        #endif
    }

    #if os(macOS)
        private func revealTechnicalDetailsInInspectorIfRequested(
            using scrollProxy: ScrollViewProxy
        ) {
            guard let detailsRevealRequest, detailsRevealRequest.itemID == itemID else { return }
            detailsExpanded.wrappedValue = true
            model.emitDetailsOpenedBeforeActing()
            withAnimation {
                scrollProxy.scrollTo(ScrollTarget.technicalDetails, anchor: .top)
            }
            onConsumeDetailsRevealRequest(detailsRevealRequest.nonce)
        }
    #endif

    @ViewBuilder
    private func platformBody<Content: View>(_ content: Content) -> some View {
        #if os(iOS)
            content.safeAreaInset(edge: .bottom, spacing: 0) {
                // The same ranking gate the card's own recommendation block
                // uses: the served action surface decides what this client may
                // submit, so a stored recommendation the surface no longer
                // ranks must not reappear as a footer button once the block
                // itself has stopped rendering (#1107 review).
                if let item = model.snapshot?.item,
                    let recommendation = DecisionRecommendationPresentation.of(item),
                    actionRanking(item).recommended == recommendation.action,
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
                .inspector(isPresented: inspectorBinding) {
                    if let item = model.snapshot?.item {
                        ScrollViewReader { scrollProxy in
                            ScrollView {
                                inspectorContent(item)
                                    .padding()
                            }
                            .onChange(of: detailsRevealRequest) {
                                revealTechnicalDetailsInInspectorIfRequested(using: scrollProxy)
                            }
                            .onAppear {
                                revealTechnicalDetailsInInspectorIfRequested(using: scrollProxy)
                            }
                        }
                        .background(Color.sidebarGround)
                        .inspectorColumnWidth(min: 280, ideal: 340, max: 440)
                    } else {
                        UnavailableStateView(
                            title: "No decision selected",
                            systemImage: "sidebar.trailing",
                            description: "Select an item to inspect its facts.")
                    }
                }
        #endif
    }

    private var isAccessibilityLayout: Bool {
        dynamicTypeSize >= .accessibility1
    }

    private var usesWideLayout: Bool {
        #if os(macOS)
            detailWidth >= 1_000 && !isAccessibilityLayout
        #else
            false
        #endif
    }

    private var confirmationIsPresented: Binding<Bool> {
        Binding(
            get: { pendingConfirmation != nil },
            set: { presented in
                if !presented { pendingConfirmation = nil }
            })
    }

    private var capabilityRetryIsPresented: Binding<Bool> {
        Binding(
            get: { capabilityRetrySnapshot != nil },
            set: { presented in
                if !presented { capabilityRetrySnapshot = nil }
            })
    }

    private var confirmationTitle: String {
        guard let pendingConfirmation else { return "Confirm action" }
        return "Confirm \(AttentionDisplay.label(pendingConfirmation.action).lowercased())?"
    }

    @ViewBuilder
    private func card(
        _ item: Components.Schemas.AttentionItem,
        proposalFacts: Components.Schemas.RunProposalFactsSnapshot?,
        rendersInteractiveControls: Bool = true,
        accessibilityLayout: Bool,
        compactLayout: Bool,
        wideLayout: Bool,
        inspectorPresented: Bool = false,
        actionRegionFrameChanged: ((CGRect) -> Void)? = nil
    ) -> some View {
        let composition = DecisionCardComposition.forType(item._type)
        VStack(alignment: .leading, spacing: 16) {
            header(item, accessibilityLayout: accessibilityLayout)
            banner(accessibilityLayout: accessibilityLayout)
            Text(AttentionDisplay.ask(item))
                .font(FreesideFont.sectionTitle)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            // The ask and the daemon's reason are one question and its answer,
            // so nothing renders between them. The reason stays labeled
            // because the daemon writes it as a sentence fragment. A type
            // whose reason is the agent's summary shows it once, under the
            // unverified claim label, and gets no Context section (#1098).
            if composition.rendersContext(for: item) {
                context(item)
            }

            if let conversation = model.conversation {
                ConversationView(
                    snapshot: conversation,
                    attachments: attachments,
                    loadsAttachments: loadsAttachments,
                    now: now,
                    rendersInteractiveControls: rendersInteractiveControls)
            }

            if let replacement = model.revisedSpecification {
                Button {
                    onSelectItem(replacement.item.id)
                } label: {
                    HStack(alignment: .firstTextBaseline) {
                        Label("Revised specification ready", systemImage: "doc.badge.arrow.up")
                        Spacer()
                        Image(systemName: "chevron.right")
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
            }

            #if os(macOS)
                if wideLayout {
                    // The two columns are read side by side, so the action
                    // region at the top of the right column is reachable
                    // before anything further down the left one. The modules
                    // a composition places ahead of actionInsertionIndex must
                    // still precede the actions, so they render full width
                    // above the split rather than beside it.
                    ForEach(
                        Array(
                            composition.modules.prefix(composition.actionInsertionIndex)
                                .enumerated()), id: \.offset
                    ) {
                        index, module in
                        cardModule(
                            module,
                            moduleIndex: index,
                            item: item,
                            composition: composition,
                            proposalFacts: proposalFacts,
                            rendersInteractiveControls: rendersInteractiveControls,
                            accessibilityLayout: accessibilityLayout,
                            inspectorPresented: inspectorPresented)
                        if index + 1 == composition.reviewingActionInsertionIndex {
                            reviewingAction(item)
                        }
                    }
                    HStack(alignment: .top, spacing: 16) {
                        VStack(alignment: .leading, spacing: 16) {
                            ForEach(
                                Array(
                                    composition.modules.enumerated()
                                        .dropFirst(composition.actionInsertionIndex)),
                                id: \.offset
                            ) {
                                index, module in
                                cardModule(
                                    module,
                                    moduleIndex: index,
                                    item: item,
                                    composition: composition,
                                    proposalFacts: proposalFacts,
                                    rendersInteractiveControls: rendersInteractiveControls,
                                    accessibilityLayout: accessibilityLayout,
                                    inspectorPresented: inspectorPresented)
                                if index + 1 == composition.reviewingActionInsertionIndex {
                                    reviewingAction(item)
                                }
                            }
                        }
                        .frame(maxWidth: 560, alignment: .topLeading)

                        actionRegion(
                            item,
                            stackedLayout: accessibilityLayout || compactLayout,
                            includesReviewing: composition.reviewingActionInsertionIndex == nil,
                            rendersInteractiveControls: rendersInteractiveControls
                        )
                        .frame(width: 360, alignment: .topLeading)
                    }
                } else {
                    ForEach(Array(composition.modules.enumerated()), id: \.offset) {
                        index, module in
                        cardModule(
                            module,
                            moduleIndex: index,
                            item: item,
                            composition: composition,
                            proposalFacts: proposalFacts,
                            rendersInteractiveControls: rendersInteractiveControls,
                            accessibilityLayout: accessibilityLayout,
                            inspectorPresented: inspectorPresented)
                        if index + 1 == composition.actionInsertionIndex {
                            actionRegion(
                                item,
                                stackedLayout: accessibilityLayout || compactLayout,
                                includesReviewing: composition.reviewingActionInsertionIndex == nil,
                                rendersInteractiveControls: rendersInteractiveControls
                            )
                            .onGeometryChange(for: CGRect.self) { geometry in
                                geometry.frame(in: .named(Self.cardCoordinateSpace))
                            } action: { frame in
                                actionRegionFrameChanged?(frame)
                            }
                        }
                        if index + 1 == composition.reviewingActionInsertionIndex {
                            reviewingAction(item)
                        }
                    }
                }
            #else
                ForEach(Array(composition.modules.enumerated()), id: \.offset) {
                    index, module in
                    cardModule(
                        module,
                        moduleIndex: index,
                        item: item,
                        composition: composition,
                        proposalFacts: proposalFacts,
                        rendersInteractiveControls: rendersInteractiveControls,
                        accessibilityLayout: accessibilityLayout,
                        inspectorPresented: inspectorPresented)
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
            #endif
        }
        // The card's own space, so a measurement reads from the card's top
        // edge rather than the scroll view's (#1107).
        .coordinateSpace(name: Self.cardCoordinateSpace)
    }

    /// The card content's coordinate space: the first-viewport budget is
    /// measured from the top of the card, not the window or the scroll view.
    static let cardCoordinateSpace = "decision-card"

    #if os(macOS)
        @ViewBuilder
        private func actionRegion(
            _ item: Components.Schemas.AttentionItem,
            stackedLayout: Bool,
            includesReviewing: Bool,
            rendersInteractiveControls: Bool
        ) -> some View {
            VStack(alignment: .leading, spacing: 16) {
                if let recommendation = DecisionRecommendationPresentation.of(item),
                    actionRanking(item).recommended == recommendation.action
                {
                    recommendationBlock(recommendation, item: item)
                }
                let actionClaims = item.agent_claims.filter {
                    $0.text != nil && $0.label != AgentClaimLabels.summary
                        && !AgentClaimLabels.isApprovalMaterial($0.label)
                }
                if !actionClaims.isEmpty {
                    cardSection("Agent claims (unverified)", dashed: true) {
                        claimRows(
                            actionClaims,
                            rendersInteractiveControls: rendersInteractiveControls)
                    }
                }
                actions(
                    item,
                    stackedLayout: stackedLayout,
                    includesReviewing: includesReviewing)
            }
        }
    #endif

    @ViewBuilder
    private func cardModule(
        _ module: DecisionCardModule,
        moduleIndex: Int,
        item: Components.Schemas.AttentionItem,
        composition: DecisionCardComposition,
        proposalFacts: Components.Schemas.RunProposalFactsSnapshot?,
        rendersInteractiveControls: Bool,
        accessibilityLayout: Bool,
        inspectorPresented: Bool
    ) -> some View {
        switch module {
        case .facts:
            factsSection(
                item,
                includesCommitPlan: !composition.modules.contains(.checklist))
            if let proposalFacts {
                cardSection("Authenticated proposal") {
                    proposalRows(proposalFacts)
                }
            }
        case .agentQuestion:
            agentQuestionLead(item)
        case .specRevision:
            specRevisionLead(item)
        case .specification:
            specificationMaterial(
                item,
                rendersInteractiveControls: rendersInteractiveControls)
        case .recommendation:
            #if os(iOS)
                if let recommendation = DecisionRecommendationPresentation.of(item),
                    actionRanking(item).recommended == recommendation.action
                {
                    recommendationBlock(recommendation, item: item)
                }
            #endif
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
        case .findingFacts:
            // The labeled proposal and the daemon-fact register lead the §9
            // finding_adjudication card (docs/plan.md §9, #984), so this
            // module renders ahead of actionInsertionIndex on every layout;
            // see DecisionCardComposition.forType(.finding_adjudication).
            if let adjudication = item.finding_adjudication?.value1 {
                findingAdjudicationLead(
                    adjudication,
                    rendersInteractiveControls: rendersInteractiveControls)
            }
        case .factBlock:
            factBlocks(item, rendersInteractiveControls: rendersInteractiveControls)
        case .summary:
            agentSummary(
                composition.summaries(from: item.agent_claims),
                rendersInteractiveControls: rendersInteractiveControls)
        case .claims:
            #if os(iOS)
                claims(
                    composition.claims(
                        from: item.agent_claims,
                        at: moduleIndex,
                        prominentClaimIndex: graphics.prominentClaimIndex),
                    accessibilityLayout: accessibilityLayout,
                    prominent: composition.claimsAreProminent(at: moduleIndex),
                    rendersInteractiveControls: rendersInteractiveControls)
            #endif
        case .evidence:
            #if os(macOS)
                if composition.reviewingActionInsertionIndex != nil {
                    // The open inspector renders the same attachments beside
                    // the card, so the card's own Evidence module collapses to
                    // a pointer at the packet rather than drawing it twice
                    // (#1107). Closing the inspector restores the rows.
                    //
                    // The pointer waits on the inspector's own Evidence
                    // disclosure, which starts closed and persists its state.
                    // At ordinary type sizes the card's module ignores that
                    // preference and always draws its rows (`lowerSection`
                    // only builds a DisclosureGroup for the accessibility
                    // layout), so pointing at a closed inspector section would
                    // take visible attachments off screen and leave them
                    // nowhere. Duplication is what the row exists to prevent,
                    // and there is none while the inspector is not drawing
                    // them.
                    if inspectorPresented, evidenceExpanded.wrappedValue,
                        !item.evidence_snapshot.isEmpty
                    {
                        cardSection("Evidence") {
                            Text(Self.evidencePointer(item.evidence_snapshot.count))
                                .foregroundStyle(Color.inkDim)
                                .accessibilityLabel(
                                    Text(
                                        Self.evidencePointerAccessibilityLabel(
                                            item.evidence_snapshot.count)))
                        }
                    } else {
                        evidence(
                            item,
                            accessibilityLayout: accessibilityLayout,
                            rendersInteractiveControls: rendersInteractiveControls)
                    }
                }
            #else
                evidence(
                    item,
                    accessibilityLayout: accessibilityLayout,
                    rendersInteractiveControls: rendersInteractiveControls)
            #endif
        case .details:
            #if os(iOS)
                details(item, accessibilityLayout: accessibilityLayout)
            #endif
        }
    }

    @ViewBuilder
    private func agentSummary(
        _ claims: [Components.Schemas.AgentClaim],
        rendersInteractiveControls: Bool
    ) -> some View {
        if !claims.isEmpty {
            cardSection("Agent summary (unverified)", dashed: true) {
                Text("Written by the agent, not checked by the daemon.")
                    .foregroundStyle(Color.inkDim)
                ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
                    Text("Source: agent invocation `\(producerInvocationID(claim))`")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.inkDim)
                        .textSelection(.enabled)
                    AttachmentRow(
                        label: "Summary",
                        digest: claim.digest,
                        metadata: claim.metadata,
                        attachments: attachments,
                        loadsAttachments: loadsAttachments,
                        text: claim.text,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
            }
        }
    }

    private func producerInvocationID(_ claim: Components.Schemas.AgentClaim) -> String {
        switch claim.provenance {
        case .head_bound(let provenance):
            provenance.producer_invocation_id
        case .head_independent(let provenance):
            provenance.producer_invocation_id
        }
    }

    @ViewBuilder
    private func context(_ item: Components.Schemas.AttentionItem) -> some View {
        if !item.reason.isEmpty {
            cardSection("Context") {
                Text(item.reason)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    /// The Section 9 card facts for this item type, read from its typed fact
    /// fields (#724). Rendered from the `.facts` module, which every
    /// composition places ahead of its action region; a type whose lead is its
    /// own module contributes no rows and the section disappears rather than
    /// rendering an empty container.
    @ViewBuilder
    private func factsSection(
        _ item: Components.Schemas.AttentionItem,
        includesCommitPlan: Bool
    ) -> some View {
        let facts = AttentionDisplay.cardFacts(item, now: now)
        let notice = includesCommitPlan ? item.commit_plan_notice?.value1 : nil
        if !facts.isEmpty || notice != nil {
            cardSection("Facts") {
                ForEach(facts) { fact in
                    factRow(fact.label, value: fact.value, monospaced: fact.monospaced)
                }
                if let notice {
                    factRow("Commit plan", value: AttentionDisplay.label(notice))
                }
            }
        }
    }

    @ViewBuilder
    private func factBlocks(
        _ item: Components.Schemas.AttentionItem,
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

        if let adjudication = item.finding_adjudication?.value1 {
            findingAdjudicationDetail(
                adjudication,
                rendersInteractiveControls: rendersInteractiveControls)
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
    }

    /// The card's ask is the shell's question; this module carries the
    /// decisions the agent stopped on, and each one leads with its own
    /// question in the serif. Who stopped and what blocks the run are facts,
    /// so they render once as fact rows rather than as a preface the operator
    /// reads before reaching anything to answer (#1107).
    ///
    /// The daemon types the decision structure, but the question, the
    /// blocking explanation, the option labels, and the tradeoffs are all
    /// prose from the asking invocation's Question claim, so each decision
    /// renders in the claim register the card uses everywhere else: the
    /// dashed border and a register label above the question. Plan §9 has
    /// this type lead with "the question as a labeled agent claim,
    /// self-contained: what is blocked and any enumerated options", and
    /// without the register an operator reads agent prose in the solid
    /// daemon-fact box. The per-option marker stays on the recommendation it
    /// qualifies; it speaks for one option, not for the question around it.
    @ViewBuilder
    private func agentQuestionLead(_ item: Components.Schemas.AttentionItem) -> some View {
        if let presentation = AgentQuestionPresentation(item) {
            ForEach(Array(presentation.decisions.enumerated()), id: \.offset) { _, decision in
                VStack(alignment: .leading, spacing: 8) {
                    KeywordLabel(text: "Agent question (unverified)")
                    Text(decision.question)
                        .font(FreesideFont.itemTitle)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(decision.whyBlocking)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                    ForEach(Array(decision.options.enumerated()), id: \.offset) { _, option in
                        VStack(alignment: .leading, spacing: 2) {
                            HStack(alignment: .firstTextBaseline, spacing: 6) {
                                Text(option.label)
                                    .font(FreesideFont.sans(.callout, weight: .semibold))
                                if option.recommended {
                                    Label(
                                        "Agent recommends (unverified)",
                                        systemImage: "quote.bubble"
                                    )
                                    .font(FreesideFont.caption)
                                    .foregroundStyle(Color.accentText)
                                }
                            }
                            Text(option.tradeoffs)
                                .font(FreesideFont.callout)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                }
                .foregroundStyle(Color.ink)
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .freesideCard(dashed: true)
            }
        }
    }

    @ViewBuilder
    private func specRevisionLead(_ item: Components.Schemas.AttentionItem) -> some View {
        if let revision = item.spec_revision?.value1,
            let priorIteration = Self.priorSpecRevisionIteration(in: item)
        {
            cardSection("Specification revision") {
                Text(
                    "Revision \(revision.iteration), supersedes revision \(priorIteration), +\(revision.diff.lines_added) −\(revision.diff.lines_removed) lines"
                )
                .font(FreesideFont.sans(.callout, weight: .semibold))
                .fixedSize(horizontal: false, vertical: true)

                Divider()
                Text("Agent responses are unverified.")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)

                ForEach(Array(revision.prior_comments.enumerated()), id: \.offset) {
                    index, comment in
                    VStack(alignment: .leading, spacing: 6) {
                        Text("You, iteration \(comment.iteration)")
                            .font(FreesideFont.caption)
                            .foregroundStyle(Color.accentText)
                        Text(comment.body)
                            .fixedSize(horizontal: false, vertical: true)

                        if let addressal = revision.claimed_addressals.first(where: {
                            $0.comment_id == comment.comment_id
                        }) {
                            Label("Agent response", systemImage: "quote.bubble")
                                .font(FreesideFont.caption)
                                .foregroundStyle(Color.inkDim)
                            Text(addressal.response)
                                .fixedSize(horizontal: false, vertical: true)
                        } else {
                            Label("No addressal claimed", systemImage: "questionmark.bubble")
                                .font(FreesideFont.caption)
                                .foregroundStyle(Color.inkDim)
                        }
                    }
                    if index < revision.prior_comments.count - 1 {
                        Divider()
                    }
                }
            }
        }
    }

    @ViewBuilder
    private func specificationMaterial(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let specification = Self.specificationClaim(in: item) {
            let iteration = Self.specificationRevisionIteration(in: item)
            let title = iteration.map { "Specification, revision \($0)" } ?? "Specification"
            cardSection("Approval material") {
                if specification.text != nil {
                    approvalMaterialRow(
                        title: title,
                        detail: "Daemon-bound digest \(specification.digest)",
                        reader: .specification,
                        rendersInteractiveControls: rendersInteractiveControls)
                } else {
                    AttachmentRow(
                        label: title,
                        digest: specification.digest,
                        metadata: specification.metadata,
                        attachments: attachments,
                        loadsAttachments: loadsAttachments,
                        rendersInteractiveControls: rendersInteractiveControls)
                }

                if let priorIteration = Self.priorSpecRevisionIteration(in: item) {
                    Divider()
                    approvalMaterialRow(
                        title: "Diff from revision \(priorIteration)",
                        detail: "Bounded unified diff",
                        reader: .diff,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
            }
        }
    }

    static func specificationRevisionIteration(
        in item: Components.Schemas.AttentionItem
    ) -> Int? {
        if let iteration = item.spec_revision?.value1.iteration {
            return iteration
        }
        guard let artifactID = specificationClaim(in: item)?.artifact_id,
            let suffix = artifactID.split(separator: "-").last,
            let iteration = Int(suffix), iteration > 0
        else { return nil }
        return iteration
    }

    static func priorSpecRevisionIteration(
        in item: Components.Schemas.AttentionItem
    ) -> Int? {
        item.spec_revision?.value1.prior_comments.last?.iteration
    }

    @ViewBuilder
    private func approvalMaterialRow(
        title: String,
        detail: String,
        reader: SpecApprovalReader,
        rendersInteractiveControls: Bool
    ) -> some View {
        if rendersInteractiveControls {
            Button {
                openSpecApprovalReader(reader)
            } label: {
                HStack(alignment: .center, spacing: 12) {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(title)
                            .font(FreesideFont.sans(.callout, weight: .semibold))
                            .foregroundStyle(Color.ink)
                        Text(detail)
                            .font(FreesideFont.caption)
                            .foregroundStyle(Color.inkDim)
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                    Spacer(minLength: 8)
                    Text("Open")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.accentText)
                    Image(systemName: "chevron.right")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.accentText)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Open \(title)")
        } else {
            HStack(alignment: .center, spacing: 12) {
                VStack(alignment: .leading, spacing: 3) {
                    Text(title)
                        .font(FreesideFont.sans(.callout, weight: .semibold))
                    Text(detail)
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.inkDim)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                Spacer(minLength: 8)
                Label("Open", systemImage: "chevron.right")
                    .labelStyle(.titleAndIcon)
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.accentText)
            }
        }
    }

    static func specificationClaim(
        in item: Components.Schemas.AttentionItem
    ) -> Components.Schemas.AgentClaim? {
        item.agent_claims.first { claim in
            claim.label == AgentClaimLabels.specification
        }
    }

    private func openSpecApprovalReader(_ reader: SpecApprovalReader) {
        specApprovalReader = reader
        #if os(macOS)
            inspectorBinding.wrappedValue = true
        #endif
    }

    @ViewBuilder
    private func specApprovalReaderContent(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem,
        rendersScrollableContent: Bool = true
    ) -> some View {
        switch reader {
        case .specification:
            if let specification = Self.specificationClaim(in: item),
                let text = specification.text
            {
                SpecificationReaderView(
                    text: text.content,
                    mediaType: text.media_type,
                    digest: specification.digest,
                    rendersScrollableContent: rendersScrollableContent)
            } else {
                UnavailableStateView(
                    title: "Specification unavailable",
                    systemImage: "doc",
                    description: "This approval does not carry a readable specification.")
            }
        case .diff:
            if let revision = item.spec_revision?.value1 {
                UnifiedDiffView(
                    unified: revision.diff.unified,
                    linesAdded: revision.diff.lines_added,
                    linesRemoved: revision.diff.lines_removed,
                    truncated: revision.diff.truncated,
                    rendersScrollableContent: rendersScrollableContent)
            } else {
                UnavailableStateView(
                    title: "Diff unavailable",
                    systemImage: "doc.text.magnifyingglass",
                    description: "This is the first specification revision.")
            }
        }
    }

    #if os(iOS)
        @ViewBuilder
        private func specApprovalReaderSheet(
            _ reader: SpecApprovalReader,
            item: Components.Schemas.AttentionItem
        ) -> some View {
            NavigationStack {
                specApprovalReaderContent(reader, item: item)
                    .padding()
                    .navigationTitle(reader == .specification ? "Specification" : "Specification changes")
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Done") { specApprovalReader = nil }
                        }
                    }
            }
        }
    #endif

    @ViewBuilder
    private func claims(
        _ claims: [Components.Schemas.AgentClaim],
        accessibilityLayout: Bool,
        prominent: Bool,
        rendersInteractiveControls: Bool
    ) -> some View {
        if !claims.isEmpty {
            if prominent {
                cardSection("Agent claims (unverified)", dashed: true) {
                    claimRows(
                        claims,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
            } else {
                lowerSection(
                    "Agent claims (unverified)",
                    isExpanded: claimsExpanded,
                    accessibilityLayout: accessibilityLayout,
                    dashed: true
                ) {
                    claimRows(
                        claims,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
            }
        }
    }

    @ViewBuilder
    private func claimRows(
        _ claims: [Components.Schemas.AgentClaim],
        rendersInteractiveControls: Bool = true
    ) -> some View {
        Text("Written by the agent, not checked by the daemon.")
            .foregroundStyle(Color.inkDim)
        // Position is the only stable identity: two claims may bind the same
        // artifact under different labels and neither field is unique.
        ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
            AttachmentRow(
                label: claim.label, digest: claim.digest,
                metadata: claim.metadata,
                attachments: attachments,
                loadsAttachments: loadsAttachments,
                text: claim.text,
                rendersInteractiveControls: rendersInteractiveControls)
        }
    }

    /// The card's Evidence module while the inspector holds the same packet:
    /// how many attachments it has and where they are, never a second copy of
    /// the rows themselves.
    static func evidencePointer(_ count: Int) -> String {
        "\(count == 1 ? "1 attachment" : "\(count) attachments") → inspector"
    }

    /// The pointer row spoken: the arrow is a direction, not a character worth
    /// reading out.
    static func evidencePointerAccessibilityLabel(_ count: Int) -> String {
        "\(count == 1 ? "1 attachment" : "\(count) attachments"), shown in the inspector"
    }

    @ViewBuilder
    private func evidence(
        _ item: Components.Schemas.AttentionItem,
        accessibilityLayout: Bool,
        rendersInteractiveControls: Bool
    ) -> some View {
        if !item.evidence_snapshot.isEmpty {
            lowerSection(
                "Evidence",
                isExpanded: evidenceExpanded,
                accessibilityLayout: accessibilityLayout
            ) {
                ForEach(item.evidence_snapshot, id: \.id) { artifact in
                    AttachmentRow(
                        label: artifact._type.rawValue, digest: artifact.digest,
                        metadata: artifact.metadata,
                        attachments: attachments,
                        loadsAttachments: loadsAttachments,
                        rendersInteractiveControls: rendersInteractiveControls)
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
            isExpanded: detailsExpanded,
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

    #if os(macOS)
        @ViewBuilder
        private func inspectorContent(
            _ item: Components.Schemas.AttentionItem,
            rendersInteractiveControls: Bool = true
        ) -> some View {
            // The card carries the Section 9 facts and the authenticated
            // proposal, so the inspector carries only what the card omits:
            // attachment claims, the evidence packet, and the technical
            // bindings. A second copy of the same rows made an open inspector
            // repeat the card beside it.
            VStack(alignment: .leading, spacing: 12) {
                if let specApprovalReader {
                    HStack {
                        Text(
                            specApprovalReader == .specification
                                ? "Specification" : "Specification changes"
                        )
                        .font(FreesideFont.sectionTitle)
                        Spacer()
                        Button {
                            self.specApprovalReader = nil
                        } label: {
                            Label("Close reader", systemImage: "xmark")
                                .labelStyle(.iconOnly)
                        }
                        .buttonStyle(.plain)
                    }
                    specApprovalReaderContent(specApprovalReader, item: item)
                } else {
                    let attachmentClaims = item.agent_claims.filter {
                        $0.text == nil && !AgentClaimLabels.isApprovalMaterial($0.label)
                    }
                    if !attachmentClaims.isEmpty {
                        inspectorSection(
                            "Agent claims (unverified)",
                            isExpanded: claimsExpanded,
                            dashed: true
                        ) {
                            claimRows(
                                attachmentClaims,
                                rendersInteractiveControls: rendersInteractiveControls)
                        }
                    }
                    if !item.evidence_snapshot.isEmpty {
                        inspectorSection("Evidence", isExpanded: evidenceExpanded) {
                            ForEach(item.evidence_snapshot, id: \.id) { artifact in
                                AttachmentRow(
                                    label: artifact._type.rawValue,
                                    digest: artifact.digest,
                                    metadata: artifact.metadata,
                                    attachments: attachments,
                                    loadsAttachments: loadsAttachments,
                                    rendersInteractiveControls: rendersInteractiveControls)
                            }
                        }
                    }
                    inspectorSection("Details", isExpanded: detailsExpanded) {
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
            }
            .environment(\.dynamicTypeSize, dynamicTypeSize)
        }
    #endif

    @ViewBuilder
    private func proposalRows(_ facts: Components.Schemas.RunProposalFactsSnapshot) -> some View {
        factRow("Intent", value: facts.intent.rawValue)
        factRow("Expected cost", value: "\(facts.expected_cost_units) units")
        factRow("Components", value: "\(facts.scope.component_count)")
        factRow("Declared paths", value: "\(facts.scope.declared_path_count)")
        factRow("Control plane", value: facts.scope.touches_control_plane ? "Yes" : "No")
        if let prior = facts.supersedes?.value1 {
            Divider()
            Text("Revision context")
                .font(FreesideFont.sans(.caption, weight: .semibold))
            proposalRevisionRows(prior)
        }
    }

    private var claimsExpanded: Binding<Bool> {
        preferenceBinding(\.claimsExpanded)
    }

    private var evidenceExpanded: Binding<Bool> {
        preferenceBinding(\.evidenceExpanded)
    }

    private var detailsExpanded: Binding<Bool> {
        preferenceBinding(\.detailsExpanded)
    }

    private func preferenceBinding(
        _ keyPath: ReferenceWritableKeyPath<DecisionSectionPreferences, Bool>
    ) -> Binding<Bool> {
        Binding(
            get: { sectionPreferences[keyPath: keyPath] },
            set: { sectionPreferences[keyPath: keyPath] = $0 })
    }

    /// The project-owned card content without navigation and presentation
    /// containers that ImageRenderer cannot draw off-screen on macOS.
    @ViewBuilder
    func screenshotCard(
        _ item: Components.Schemas.AttentionItem,
        at dynamicTypeSize: DynamicTypeSize,
        proposalFacts: Components.Schemas.RunProposalFactsSnapshot? = nil,
        compactLayout: Bool = false,
        detailWidth: CGFloat = 560,
        inspectorPresented: Bool = false,
        actionRegionFrameChanged: ((CGRect) -> Void)? = nil
    ) -> some View {
        let wideLayout = detailWidth >= 1_000 && dynamicTypeSize < .accessibility1
        card(
            item,
            proposalFacts: proposalFacts,
            rendersInteractiveControls: false,
            accessibilityLayout: dynamicTypeSize >= .accessibility1,
            compactLayout: compactLayout,
            wideLayout: wideLayout,
            inspectorPresented: inspectorPresented,
            actionRegionFrameChanged: actionRegionFrameChanged
        )
        .padding(14)
        .freesideCard()
        .padding()
        .frame(maxWidth: wideLayout ? 1_040 : 560, alignment: .topLeading)
    }

    func screenshotBanner() -> some View {
        bannerLabel(
            "Submission failed: the daemon rejected the command.",
            systemImage: "exclamationmark",
            tint: .waxText,
            wash: .waxWash
        )
        .padding()
    }

    func screenshotRetryableReceipt(expanded: Bool, accessibilityLayout: Bool) -> some View {
        RetryableReceipt(
            isExpanded: .constant(expanded),
            accessibilityLayout: accessibilityLayout,
            failureMessage: "the daemon did not answer",
            retry: {}
        )
        .padding()
    }

    @ViewBuilder
    func screenshotSpecApprovalReader(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(reader == .specification ? "Specification" : "Specification changes")
                .font(FreesideFont.sectionTitle)
            specApprovalReaderContent(
                reader,
                item: item,
                rendersScrollableContent: false)
        }
        .padding()
        .frame(maxWidth: .infinity, alignment: .topLeading)
        .background(Color.ground)
    }

    #if os(macOS)
        func screenshotInspector(
            _ item: Components.Schemas.AttentionItem,
            at dynamicTypeSize: DynamicTypeSize
        ) -> some View {
            inspectorContent(item, rendersInteractiveControls: false)
                .padding()
                .frame(width: 360, alignment: .topLeading)
                .background(Color.sidebarGround)
        }
    #endif

    // Split across the §9 leads-with/below boundary (#984): the labeled
    // proposal and the daemon-fact register lead (findingAdjudicationLead,
    // rendered from the .findingFacts module ahead of actionInsertionIndex),
    // while assumptions, cited rules, alternatives, and gating questions stay
    // below the action region (findingAdjudicationDetail, still reached from
    // .factBlock). Both iterate the same proposals in the same order so a
    // multi-finding item keeps each finding's lead and detail content
    // correspondingly ordered.
    //
    // The lead collapses: each finding is one row (its id, the recommended
    // route, the goal relationship, and the adjudicator's confidence) under
    // its producer register, and the full proposal and daemon facts open in
    // place above the action region, which two expanded findings pushed a
    // full viewport below the fold (#1107).
    @ViewBuilder
    private func findingAdjudicationLead(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        rendersInteractiveControls: Bool
    ) -> some View {
        ForEach(binding.proposals, id: \.finding_id) { proposal in
            let producer = AttentionDisplay.adjudicationProducerPresentation(proposal.producer)
            DisclosureGroup(isExpanded: findingExpansion(proposal.finding_id)) {
                VStack(alignment: .leading, spacing: 12) {
                    factRow(
                        "Work-unit compatibility",
                        value: AttentionDisplay.label(proposal.compatibility?.value1))
                    Text(proposal.rationale)
                        .fixedSize(horizontal: false, vertical: true)
                    if !proposal.evidence.isEmpty {
                        // The engine fast path also populates evidence (the
                        // finding's own containment location, a daemon fact),
                        // so the label follows producer.modelBacked like the
                        // surrounding register instead of always reading
                        // "model-derived" (#892, #984 review).
                        VStack(alignment: .leading, spacing: 3) {
                            Text(
                                producer.modelBacked
                                    ? "Evidence (model-derived)" : "Evidence (daemon-derived)"
                            )
                            .font(FreesideFont.sans(.callout, weight: .semibold))
                            ForEach(Array(proposal.evidence.enumerated()), id: \.offset) {
                                _, value in
                                Label(value, systemImage: "circle.fill")
                                    .labelStyle(FindingListLabelStyle())
                            }
                        }
                    }
                    // The finding message and location are daemon-authenticated
                    // coordinates, so they keep their own solid register inside
                    // the expanded row, never mixed into the model-backed
                    // content around them (#892).
                    cardSection("Daemon facts") {
                        if !proposal.finding_message.isEmpty {
                            factRow("Finding message", value: proposal.finding_message)
                        }
                        if let location = proposal.finding_location?.value1 {
                            factRow(
                                "Finding location",
                                value: AttentionDisplay.findingLocation(location), monospaced: true)
                        }
                        factRow(
                            "Binding digest", value: binding.adjudication_digest, monospaced: true)
                        factRow("Run", value: binding.run_id)
                        factRow("Round", value: "\(binding.round)")
                    }
                }
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
                .padding(.top, 8)
            } label: {
                VStack(alignment: .leading, spacing: 4) {
                    KeywordLabel(text: producer.label)
                    Text(Self.findingSummary(proposal))
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                }
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(
                    Text(
                        "\(producer.label). \(Self.findingSummaryAccessibilityLabel(proposal))"))
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .freesideCard(dashed: producer.modelBacked)
        }
    }

    /// One finding's collapsed row: the daemon's finding id, then the three
    /// values that decide the route (the recommendation, how the finding
    /// relates to the goal, and the adjudicator's confidence where it recorded
    /// one). The producer register labels the row above it, so the row itself
    /// carries no register word.
    static func findingSummary(
        _ proposal: Components.Schemas.FindingAdjudicationProposal
    ) -> String {
        var parts = [
            proposal.finding_id,
            AttentionDisplay.label(proposal.route),
            AttentionDisplay.label(proposal.goal_relationship),
        ]
        if let confidence = proposal.confidence?.value1 {
            parts.append(AttentionDisplay.label(confidence))
        }
        return parts.joined(separator: " · ")
    }

    /// The collapsed row spoken. The visible row separates four bare values
    /// with "·", which reads as four unlabelled words, so the spoken form
    /// names the field each one answers. The producer register joins it at
    /// the call site, so a model-proposed route is never spoken as a fact the
    /// daemon established.
    static func findingSummaryAccessibilityLabel(
        _ proposal: Components.Schemas.FindingAdjudicationProposal
    ) -> String {
        var parts = [
            "Finding \(proposal.finding_id)",
            "recommended route \(AttentionDisplay.label(proposal.route))",
            "goal relationship \(AttentionDisplay.label(proposal.goal_relationship))",
        ]
        if let confidence = proposal.confidence?.value1 {
            parts.append("confidence \(AttentionDisplay.label(confidence))")
        }
        return parts.joined(separator: ", ")
    }

    private func findingExpansion(_ findingID: String) -> Binding<Bool> {
        Binding(
            get: { expandedFindings.contains(findingID) },
            set: { expanded in
                if expanded {
                    expandedFindings.insert(findingID)
                } else {
                    expandedFindings.remove(findingID)
                }
            })
    }

    @ViewBuilder
    private func findingAdjudicationDetail(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        rendersInteractiveControls: Bool
    ) -> some View {
        // A single proposal's detail groups need no label: they are the only
        // candidate. Once findingAdjudicationLead moved ahead of the action
        // region, a multi-proposal item's detail groups (still one .factBlock
        // pass per proposal) lost the lead section's adjacency to
        // "Finding <id>", so a second-plus proposal repeats it here — the
        // generic "Assumptions"/"Viable alternatives" titles otherwise give no
        // way to tell which finding a later picker selection applies to
        // (#984 review).
        let identifiesEachFinding = binding.proposals.count > 1
        ForEach(binding.proposals, id: \.finding_id) { proposal in
            if identifiesEachFinding,
                !proposal.assumptions.isEmpty || !proposal.cited_rules.isEmpty
                    || !proposal.offered_alternatives.isEmpty || !proposal.open_questions.isEmpty
            {
                Text("Finding \(proposal.finding_id)")
                    .font(FreesideFont.sans(.caption, weight: .semibold))
                    .foregroundStyle(Color.inkDim)
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

    static func runProposalRevision(
        from facts: Components.Schemas.RunProposalFactsSnapshot,
        expectedCost: Int,
        componentCount: Int,
        touchesControlPlane: Bool
    ) -> Components.Schemas.RunProposalRevisionInput? {
        guard
            expectedCost != facts.expected_cost_units
                || componentCount != facts.scope.component_count
                || touchesControlPlane != facts.scope.touches_control_plane
        else { return nil }

        // The daemon binds this count to the durable declaration, so it must
        // not become operator input.
        return .init(
            intent: facts.intent,
            expected_cost_units: expectedCost,
            scope: .init(
                component_count: componentCount,
                declared_path_count: facts.scope.declared_path_count,
                touches_control_plane: touchesControlPlane))
    }

    static func parseExpectedCost(_ text: String) -> Int? {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let value = Int(trimmed), (1...1_000_000).contains(value) else { return nil }
        return value
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
            contentsOf: AttentionDisplay.unavailableActionRows(actionRanking(item).unavailable))
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
            if AttentionDisplay.showsPriorityBadge(item.priority) {
                PriorityBadge(priority: item.priority)
            }
            if let posture = item.posture?.value1, AttentionDisplay.showsPostureBadge(posture) {
                HealthPostureBadge(posture: posture)
            }
            if AttentionDisplay.showsLifecycleBadge(item.status) {
                StatusBadge(status: item.status)
            }
        }
    }

    @ViewBuilder
    private func banner(accessibilityLayout: Bool) -> some View {
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
                RetryableReceipt(
                    isExpanded: $lostResponseExpanded,
                    accessibilityLayout: accessibilityLayout,
                    failureMessage: model.submissionError
                ) {
                    Task { await model.retryLostResponse() }
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

    /// The recommendation leads its card in the register its revalidated
    /// provenance supports (plan §9): daemon policy and project policy render
    /// as card facts, agent judgment as a labeled unverified proposal. Inside
    /// that register it argues before it acts: the label carries the register
    /// and the daemon's confidence, then the reason, then the button. The
    /// block led with the act until 2026-09-03, on the reading that an
    /// operator decides rather than audits; the owner reversed that after the
    /// September UI audit (#1104, #1107), so the button is the conclusion of
    /// the argument above it rather than a control the reason trails.
    private func recommendationBlock(
        _ recommendation: DecisionRecommendationPresentation,
        item: Components.Schemas.AttentionItem
    ) -> some View {
        cardSection(
            recommendation.label,
            dashed: recommendation.register.isUnverifiedClaim,
            border: .accentBorder,
            fill: .accentWash
        ) {
            if recommendation.register.isUnverifiedClaim {
                Text("Written by an agent, not checked by the daemon.")
                    .foregroundStyle(Color.inkDim)
            }
            KeywordLabel(text: "Why")
            Text(recommendation.reason)
                .fixedSize(horizontal: false, vertical: true)
            actionButton(
                recommendation.action,
                item: item,
                tone: AttentionDisplay.confirmationConsequence(
                    recommendation.action,
                    for: item) == nil ? .primary : .destructive,
                showsIcon: false
            )
            // The iOS sticky footer appears when this button is off screen, so
            // the measurement is the button's own, not the block's.
            .onGeometryChange(for: Bool.self) { geometry in
                Self.recommendationActionVisible(
                    frame: geometry.frame(in: .named("decision-card-scroll")),
                    viewportHeight: geometry.bounds(of: .named("decision-card-scroll"))?.height)
            } action: { visible in
                recommendationVisible = visible
            }
            // The digests, policy key, and judgment site revalidate the
            // recommendation; an operator deciding never reads them, so they
            // stay one disclosure away below the act rather than inside the
            // argument for it.
            DisclosureGroup(isExpanded: $provenanceExpanded) {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(recommendation.sourceFacts) { fact in
                        factRow(fact.label, value: fact.value, monospaced: fact.monospaced)
                    }
                }
                .padding(.top, 6)
            } label: {
                KeywordLabel(text: "Provenance")
            }
        }
    }

    /// Whether the recommended action is on screen, which is what the iOS
    /// sticky footer stands in for: the footer offers the action exactly when
    /// this returns false.
    ///
    /// The measurement follows the button rather than the recommendation
    /// block. The block's own frame was never an exact answer, since a block
    /// sitting entirely below the fold also reports a positive `maxY`, but
    /// while the block led with its button the two moved together. Putting
    /// the reason above the button (#1107) created the state that matters
    /// here: a long reason at a large Dynamic Type size leaves the block's top
    /// on screen with its button below the fold, where measuring the block
    /// suppresses the footer and leaves nothing to press.
    ///
    /// `viewportHeight` is nil when the scroll coordinate space is not an
    /// ancestor, as on the macOS inspector, where the footer does not exist
    /// and the old top-edge test is enough.
    static func recommendationActionVisible(
        frame: CGRect,
        viewportHeight: CGFloat?
    ) -> Bool {
        guard let viewportHeight else { return frame.maxY > 0 }
        return frame.maxY > 0 && frame.minY < viewportHeight
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

    private func inspectorSection<Content: View>(
        _ title: String,
        isExpanded: Binding<Bool>,
        dashed: Bool = false,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
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
    }

    private func factRow(
        _ label: String,
        value: String,
        monospaced: Bool = false
    ) -> some View {
        // No `valueColor`: a trailing value inherits the row's own ink, so a
        // monospaced fact stays dim in the row, and a stacked value keeps the
        // full-ink contrast against its dim label.
        FactRow(label: label, value: value)
            .font(monospaced ? FreesideFont.monoCaption : FreesideFont.callout)
            .foregroundStyle(monospaced ? Color.inkDim : Color.ink)
    }

    /// One labeled attachment row. Content leads in the evidence layer and its
    /// binding digest stays a subordinate, copyable caption in every state.
    /// A text claim renders its daemon-verified inline content directly;
    /// otherwise the fetched bytes render in an explicit attachment state.
    /// Attachment bytes remain memory-only.
    struct AttachmentRow: View {
        let label: String
        let digest: String
        /// The reference's daemon-validated §5.15 metadata. Nil only for a
        /// conversation message attachment, which the wire carries as a bare
        /// digest with no metadata; that row falls back to fetch-and-inspect
        /// and shows no typed media/size caption.
        var metadata: Components.Schemas.EvidenceMetadata? = nil
        let attachments: AttachmentLoader
        let loadsAttachments: Bool
        var text: Components.Schemas.ClaimText? = nil
        var rendersInteractiveControls = true

        // Task identity for the attachment load: the digest fixes the content,
        // and availability is the one mutable field that must re-trigger the
        // load when it changes between reads.
        private struct AttachmentLoadIdentity: Equatable {
            let digest: String
            let availability: AttachmentReference.Availability?
        }

        @State private var showsImagePreview = false
        @State private var showsNonImagePreview = false
        @State private var nonImagePreview: NonImagePreview?
        @State private var previewRequestID: UUID?
        @State private var isDisplayingAttachment = false

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
                    let reference = metadata.map(AttachmentReference.init)
                    fetchedAttachment
                        // Keyed on availability as well as the digest: the
                        // contract lets a refresh flip availability for the same
                        // digest without an item-version change, and SwiftUI
                        // reuses this row, so a digest-only id would never re-run
                        // the load and the loader's downgrade/recovery logic
                        // would never see the new reference.
                        .task(
                            id: AttachmentLoadIdentity(
                                digest: digest, availability: reference?.availability)
                        ) {
                            guard loadsAttachments else { return }
                            if let reference {
                                await attachments.load(digest, reference: reference)
                            } else {
                                await attachments.load(digest)
                            }
                        }
                    if let metadata {
                        metadataCaption(metadata)
                    }
                }
                digestCaption
            }
            .onAppear {
                guard rendersInteractiveControls else { return }
                isDisplayingAttachment = true
                attachments.beginDisplaying(digest)
            }
            .onChange(of: digest) { oldDigest, newDigest in
                previewRequestID = nil
                nonImagePreview = nil
                showsImagePreview = false
                showsNonImagePreview = false
                guard rendersInteractiveControls, isDisplayingAttachment else { return }
                attachments.endDisplaying(oldDigest)
                attachments.beginDisplaying(newDigest)
            }
            .onDisappear {
                previewRequestID = nil
                nonImagePreview = nil
                showsImagePreview = false
                showsNonImagePreview = false
                guard rendersInteractiveControls, isDisplayingAttachment else { return }
                isDisplayingAttachment = false
                attachments.endDisplaying(digest)
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
                if rendersInteractiveControls {
                    Button {
                        showsImagePreview = true
                    } label: {
                        platformImage(image)
                            .resizable()
                            .scaledToFit()
                            .frame(maxWidth: 320, alignment: .leading)
                            .clipShape(RoundedRectangle(cornerRadius: 6))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Open \(label) attachment image")
                    .sheet(isPresented: $showsImagePreview) {
                        ZoomableAttachmentSheet(label: label, image: image)
                    }
                } else {
                    platformImage(image)
                        .resizable()
                        .scaledToFit()
                        .frame(maxWidth: 320, alignment: .leading)
                        .clipShape(RoundedRectangle(cornerRadius: 6))
                }
            case .notImage(let bytes, let observedByteCount):
                VStack(alignment: .leading, spacing: 6) {
                    Label("Not an image", systemImage: "doc")
                        .accessibilityLabel("\(label) attachment, not an image")
                    Text(byteCount(observedByteCount))
                        .foregroundStyle(Color.inkDim)
                    if rendersInteractiveControls {
                        Button("Open attachment") { openNonImage(bytes) }
                    } else {
                        Label("Open attachment", systemImage: "arrow.up.forward.app")
                    }
                }
                .font(FreesideFont.caption)
                .sheet(
                    isPresented: $showsNonImagePreview,
                    onDismiss: { nonImagePreview = nil },
                    content: {
                        if let nonImagePreview {
                            NonImageAttachmentSheet(label: label, preview: nonImagePreview)
                        }
                    }
                )
            case .unavailable:
                VStack(alignment: .leading, spacing: 4) {
                    Label("No bytes available", systemImage: "photo.badge.exclamationmark")
                        .font(FreesideFont.sans(.caption, weight: .semibold))
                    Text("The daemon reports the attachment bytes are not available")
                        .font(FreesideFont.caption)
                }
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.waxWash, in: RoundedRectangle(cornerRadius: 8))
                .accessibilityElement(children: .combine)
                .accessibilityLabel(
                    "\(label) attachment has no bytes available. The daemon reports the attachment bytes are not available"
                )
            case .fetchFailed:
                VStack(alignment: .leading, spacing: 6) {
                    Label("Couldn't load", systemImage: "arrow.clockwise.circle")
                        .font(FreesideFont.sans(.caption, weight: .semibold))
                    Text("The fetch failed. Try again.")
                        .font(FreesideFont.caption)
                    if rendersInteractiveControls, loadsAttachments {
                        Button("Retry") { retryFetch() }
                            .buttonStyle(.bordered)
                            .controlSize(.small)
                            .accessibilityLabel("Retry loading \(label) attachment")
                    }
                }
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.waxWash, in: RoundedRectangle(cornerRadius: 8))
                .accessibilityElement(children: .combine)
                .accessibilityLabel(
                    "\(label) attachment failed to load. The fetch failed. Try again."
                )
            case .tooLarge(let reason):
                VStack(alignment: .leading, spacing: 4) {
                    VStack(alignment: .leading, spacing: 4) {
                        Label("Too large here", systemImage: "arrow.up.left.and.arrow.down.right")
                            .font(FreesideFont.sans(.caption, weight: .semibold))
                        switch reason {
                        case .download(let bytesSeenAtLeast, _):
                            Text("At least \(byteCount(bytesSeenAtLeast))")
                        case .image(let width, let height, _),
                            .imageBudget(let width, let height, _):
                            Text("\(width) × \(height) pixels")
                        }
                        #if os(iOS)
                            Text(iOSTooLargeRecovery(reason))
                        #elseif os(macOS)
                            switch reason {
                            case .download(_, let limit):
                                Text("Exceeds the \(byteCount(limit)) inline preview limit")
                            case .image(_, _, let pixelLimit):
                                Text("Exceeds the \(pixelLimit.formatted())-pixel inline preview limit")
                            case .imageBudget(_, _, let pixelLimit):
                                Text(
                                    "Would exceed the \(pixelLimit.formatted())-pixel active inline image budget"
                                )
                            }
                        #endif
                    }
                    .accessibilityElement(children: .combine)
                    .accessibilityLabel(tooLargeAccessibilityLabel(reason))

                    if case .imageBudget = reason,
                        rendersInteractiveControls,
                        loadsAttachments
                    {
                        Button("Load this image") {
                            Task { await attachments.loadReplacingRetainedImages(digest) }
                        }
                        .buttonStyle(.bordered)
                        .controlSize(.small)
                        .accessibilityLabel(
                            "Load \(label) attachment image, replacing retained images if needed")
                    }
                }
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
            case .loading, nil:
                HStack(spacing: 8) {
                    if rendersInteractiveControls {
                        ProgressView()
                            .controlSize(.small)
                    } else {
                        Image(systemName: "arrow.triangle.2.circlepath")
                    }
                    Text("fetching by digest…")
                }
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
                .accessibilityElement(children: .combine)
                .accessibilityLabel("\(label) attachment loading")
            }
        }

        /// The daemon-validated media type and size (plan §5.15), read from
        /// the reference's typed metadata rather than inferred from a fetch.
        private func metadataCaption(
            _ metadata: Components.Schemas.EvidenceMetadata
        ) -> some View {
            Text("\(metadata.media_type.rawValue) · \(byteCount(Int(metadata.size_bytes)))")
                .font(FreesideFont.mono(.caption2))
                .foregroundStyle(Color.inkDim)
                .lineLimit(1)
                .textSelection(.enabled)
                .accessibilityLabel(
                    "\(label): \(metadata.media_type.rawValue), \(byteCount(Int(metadata.size_bytes)))"
                )
        }

        private var digestCaption: some View {
            HStack(spacing: 8) {
                Text("Digest \(digest)")
                    .font(FreesideFont.mono(.caption2))
                    .foregroundStyle(Color.inkDim)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .textSelection(.enabled)
                Spacer(minLength: 0)
                if rendersInteractiveControls {
                    Button(action: copyDigest) {
                        Image(systemName: "doc.on.doc")
                    }
                    .buttonStyle(.borderless)
                    .help("Copy digest")
                    .accessibilityLabel("Copy \(label) attachment digest")
                } else {
                    Image(systemName: "doc.on.doc")
                }
            }
        }

        private func copyDigest() {
            #if os(iOS)
                UIPasteboard.general.string = digest
            #elseif os(macOS)
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(digest, forType: .string)
            #endif
        }

        private func retryFetch() {
            let requestedDigest = digest
            let reference = metadata.map(AttachmentReference.init)
            Task {
                if let reference {
                    await attachments.load(requestedDigest, reference: reference)
                } else {
                    await attachments.load(requestedDigest)
                }
            }
        }

        private func openNonImage(_ bytes: Data?) {
            if let bytes {
                previewRequestID = nil
                nonImagePreview = NonImagePreview(bytes: bytes)
                showsNonImagePreview = true
                return
            }
            let requestID = UUID()
            let requestedDigest = digest
            previewRequestID = requestID
            Task {
                guard
                    let loadedBytes = await attachments.nonImageBytes(for: requestedDigest),
                    previewRequestID == requestID
                else { return }
                previewRequestID = nil
                nonImagePreview = NonImagePreview(bytes: loadedBytes)
                showsNonImagePreview = true
            }
        }

        private func byteCount(_ count: Int) -> String {
            ByteCountFormatter.string(fromByteCount: Int64(count), countStyle: .file)
        }

        private func tooLargeAccessibilityLabel(
            _ reason: AttachmentLoader.Phase.TooLargeReason
        ) -> String {
            let size: String
            switch reason {
            case .download(let bytesSeenAtLeast, _):
                size = "at least \(byteCount(bytesSeenAtLeast))"
            case .image(let width, let height, _),
                .imageBudget(let width, let height, _):
                size = "\(width) by \(height) pixels"
            }
            #if os(iOS)
                return "\(label) attachment too large here, \(size). \(iOSTooLargeRecovery(reason))"
            #elseif os(macOS)
                let boundary: String
                switch reason {
                case .download(_, let byteLimit):
                    boundary =
                        "exceeding the \(byteCount(byteLimit)) inline preview limit"
                case .image(_, _, let pixelLimit):
                    boundary =
                        "exceeding the \(pixelLimit.formatted())-pixel inline preview limit"
                case .imageBudget(_, _, let pixelLimit):
                    boundary =
                        "exceeding the \(pixelLimit.formatted())-pixel active inline image budget"
                }
                return "\(label) attachment too large here, \(size), \(boundary)"
            #endif
        }

        #if os(iOS)
            private func iOSTooLargeRecovery(
                _ reason: AttachmentLoader.Phase.TooLargeReason
            ) -> String {
                AttachmentLoader.macOSCanPreview(reason)
                    ? "Open on the Mac"
                    : "Too large to preview on the Mac"
            }
        #endif

        private func platformImage(_ image: PlatformImage) -> Image {
            #if canImport(UIKit)
                Image(uiImage: image)
            #elseif canImport(AppKit)
                Image(nsImage: image)
            #endif
        }
    }

    struct NonImagePreview: Equatable {
        static let textByteLimit = 64 << 10

        let byteCount: Int
        let text: String?
        let isTruncated: Bool

        init(bytes: Data) {
            byteCount = bytes.count
            var prefix = Data(bytes.prefix(Self.textByteLimit))
            var decoded = String(data: prefix, encoding: .utf8)
            if decoded == nil, bytes.count > prefix.count {
                // A valid scalar may straddle the display cutoff. UTF-8 uses
                // at most four bytes, so trim only that incomplete tail.
                for _ in 0..<3 where decoded == nil && !prefix.isEmpty {
                    prefix.removeLast()
                    decoded = String(data: prefix, encoding: .utf8)
                }
            }
            text = decoded
            isTruncated = bytes.count > prefix.count
        }
    }

    private struct ZoomableAttachmentSheet: View {
        let label: String
        let image: PlatformImage
        @Environment(\.dismiss) private var dismiss
        @State private var scale: CGFloat = 1
        @State private var committedScale: CGFloat = 1
        @State private var offset: CGSize = .zero
        @State private var committedOffset: CGSize = .zero

        var body: some View {
            NavigationStack {
                GeometryReader { _ in
                    platformImage(image)
                        .resizable()
                        .scaledToFit()
                        .scaleEffect(scale)
                        .offset(offset)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .contentShape(Rectangle())
                        .gesture(zoomAndPan)
                        .accessibilityLabel("\(label) attachment preview")
                }
                .background(Color.ground)
                .navigationTitle(label)
                .toolbar {
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Done") { dismiss() }
                    }
                }
            }
            #if os(macOS)
                .frame(
                    minWidth: 480, idealWidth: 720,
                    minHeight: 360, idealHeight: 600)
            #endif
        }

        private var zoomAndPan: some Gesture {
            SimultaneousGesture(
                MagnifyGesture()
                    .onChanged { value in
                        scale = min(max(committedScale * value.magnification, 1), 6)
                    }
                    .onEnded { _ in
                        committedScale = scale
                        if scale == 1 {
                            offset = .zero
                            committedOffset = .zero
                        }
                    },
                DragGesture()
                    .onChanged { value in
                        guard scale > 1 else { return }
                        offset = CGSize(
                            width: committedOffset.width + value.translation.width,
                            height: committedOffset.height + value.translation.height)
                    }
                    .onEnded { _ in committedOffset = offset }
            )
        }

        private func platformImage(_ image: PlatformImage) -> Image {
            #if os(iOS)
                Image(uiImage: image)
            #elseif os(macOS)
                Image(nsImage: image)
            #endif
        }
    }

    private struct NonImageAttachmentSheet: View {
        let label: String
        let preview: NonImagePreview
        @Environment(\.dismiss) private var dismiss

        var body: some View {
            NavigationStack {
                Group {
                    if let text = preview.text {
                        VStack(alignment: .leading, spacing: 8) {
                            if preview.isTruncated {
                                Text(
                                    "Showing the first \(byteCount(NonImagePreview.textByteLimit)) of \(byteCount(preview.byteCount))"
                                )
                                .font(FreesideFont.caption)
                                .foregroundStyle(Color.inkDim)
                                .padding(.horizontal)
                            }
                            ScrollView {
                                Text(text)
                                    .font(.system(.body, design: .monospaced))
                                    .textSelection(.enabled)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    .padding()
                            }
                        }
                    } else {
                        UnavailableStateView(
                            title: "Preview unavailable",
                            systemImage: "doc",
                            description: "This \(byteCount(preview.byteCount)) attachment is not text.")
                    }
                }
                .navigationTitle(label)
                .toolbar {
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Done") { dismiss() }
                    }
                }
            }
            #if os(macOS)
                .frame(
                    minWidth: 480, idealWidth: 720,
                    minHeight: 360, idealHeight: 600)
            #endif
        }

        private func byteCount(_ count: Int) -> String {
            ByteCountFormatter.string(fromByteCount: Int64(count), countStyle: .file)
        }
    }

    private struct RetryableReceipt: View {
        @Binding var isExpanded: Bool
        let accessibilityLayout: Bool
        let failureMessage: String?
        let retry: () -> Void
        @ScaledMetric(relativeTo: .callout) private var glyphSize: CGFloat = screenshotMetricBase(
            10, relativeTo: .callout)

        var body: some View {
            let layout =
                accessibilityLayout
                ? AnyLayout(VStackLayout(alignment: .leading, spacing: 8))
                : AnyLayout(HStackLayout(alignment: .firstTextBaseline, spacing: 8))
            VStack(alignment: .leading, spacing: 8) {
                layout {
                    Label {
                        Text("The response was lost.")
                            .lineLimit(1)
                            .minimumScaleFactor(0.75)
                            .textSelection(.enabled)
                    } icon: {
                        Image(systemName: "arrow.clockwise")
                            .font(.system(size: glyphSize, weight: .semibold))
                    }
                    if !accessibilityLayout {
                        Spacer(minLength: 0)
                    }
                    Button("Retry", action: retry)
                        .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
                        .accessibilityLabel("Retry the lost decision")
                }
                DisclosureGroup(isExpanded: $isExpanded) {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(
                            "The decision may already be recorded. Retry resends the same command and returns the original result."
                        )
                        if let failureMessage {
                            Text(failureMessage)
                        }
                    }
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                } label: {
                    Text("Details")
                        .foregroundStyle(Color.accentText)
                }
            }
            .font(FreesideFont.callout)
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.accentWash, in: RoundedRectangle(cornerRadius: 8))
            .foregroundStyle(Color.accentText)
        }
    }

    /// A card banner: tinted wash, glyph and message in the state color.
    private func bannerLabel(_ text: String, systemImage: String, tint: Color, wash: Color) -> some View {
        Label {
            Text(text)
        } icon: {
            Image(systemName: systemImage)
                .font(.system(size: bannerGlyphSize, weight: .semibold))
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
                    // The label reads as the disabled state it describes;
                    // the spinner keeps its water tint because the work is
                    // still in progress.
                    Text("Validating current state…")
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(Color.inkFaint)
                }
            }

            if !ranking.principal.isEmpty {
                // Keyed by position: requested_decision does not enforce
                // uniqueness, and duplicate identities may not drop a button.
                if stackedLayout {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(Array(ranking.principal.enumerated()), id: \.offset) { _, action in
                            actionButton(action, item: item, tone: .secondary)
                        }
                    }
                } else {
                    HStack(alignment: .top, spacing: 8) {
                        ForEach(Array(ranking.principal.enumerated()), id: \.offset) { _, action in
                            actionButton(action, item: item, tone: .secondary)
                        }
                    }
                }
            }

            if includesReviewing, let reviewing = ranking.reviewing {
                actionButton(reviewing, item: item, tone: .secondary)
            }

            overflowMenu(ranking.overflow, item: item)

            if ranking.notDecidableHere {
                bannerLabel(
                    "This decision needs a written answer, and this build cannot carry one. Nothing is blocked by opening it; the item stays open until answered.",
                    systemImage: "exclamationmark.bubble",
                    tint: .accentText,
                    wash: .accentWash
                )
                .onAppear { model.emitNotDecidableHereShown() }
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
            actionButton(reviewing, item: item, tone: .secondary)
        }
    }

    private func actionRanking(
        _ item: Components.Schemas.AttentionItem
    ) -> DecisionActionRanking {
        let composition = DecisionCardComposition.forType(item._type)
        return DecisionActionRanking(
            requested: item.requested_decision,
            recommendedAction: DecisionRecommendationPresentation.of(item)?.action,
            reservesRecommendedAction: composition.modules.contains(.recommendation),
            servedActions: model.actionSurface?.actions)
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
                Text("More actions \u{25BE}")
            }
            .menuStyle(.button)
            .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
            .disabled(!model.actionsEnabled)
            .accessibilityLabel("More decision actions")
            // Subordinate to the actions above it, and centered under them
            // on both layouts rather than filling a row of its own.
            .frame(maxWidth: .infinity, alignment: .center)
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
            .frame(maxWidth: .infinity, minHeight: 44)
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
        case .discuss:
            messageEditor = .discuss
        case .request_changes:
            messageEditor = .requestChanges
        case .answer_and_retry:
            messageEditor = .answerAndRetry
        case .answer_without_retry:
            messageEditor = .answerWithoutRetry
        case .return_to_agent:
            messageEditor = .returnToAgent
        case .start_with_changes:
            proposalEditor = .revision
        case .snooze:
            proposalEditor = .snooze
        case .choose_alternative_route:
            guard let binding = item?.finding_adjudication?.value1 else { return }
            Task { await model.submitFindingAlternatives(selectedAlternatives(binding)) }
        case .retry_with_capabilities:
            capabilityRetrySnapshot = model.snapshot
        default:
            Task { await model.submit(action) }
        }
    }

    #if os(macOS)
        private var focusedDecisionCommandActions: FocusedDecisionCommandActions {
            FocusedDecisionCommandActions(
                canTakeRecommendation: canTakeRecommendation,
                takeRecommendation: takeRecommendationFromKeyboard,
                cancelPendingAction: cancelPendingAction)
        }

        private var canTakeRecommendation: Bool {
            guard let item = model.snapshot?.item else { return false }
            let recommendation = DecisionRecommendationPresentation.of(item)
            return DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: actionRanking(item).recommended,
                presentedRecommendation: recommendation?.action,
                actionsEnabled: model.actionsEnabled,
                isSubmittable: recommendation.map { model.isSubmittable($0.action) } ?? false,
                inputIsReady: recommendation.map {
                    actionInputReady($0.action, item: item)
                } ?? false)
        }

        private func takeRecommendationFromKeyboard() {
            guard let item = model.snapshot?.item,
                let recommendation = DecisionRecommendationPresentation.of(item),
                canTakeRecommendation
            else { return }
            trigger(recommendation.action, item: item)
        }

        private func cancelPendingAction() {
            proposalEditor = nil
            messageEditor = nil
            pendingConfirmation = nil
            capabilityRetrySnapshot = nil
        }
    #endif
}

private struct FindingListLabelStyle: LabelStyle {
    @ScaledMetric(relativeTo: .callout) private var bulletSize: CGFloat = screenshotMetricBase(
        4, relativeTo: .callout)

    func makeBody(configuration: Configuration) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            configuration.icon
                .font(.system(size: bulletSize))
                .foregroundStyle(Color.inkDim)
            configuration.title
        }
    }
}

/// macOS ImageRenderer does not apply its injected Dynamic Type environment to
/// `ScaledMetric`. Mirror the screenshot-only font bridge's iOS scale so the
/// matrix still exercises the production metric behavior.
private func screenshotMetricBase(
    _ value: CGFloat,
    relativeTo style: Font.TextStyle
) -> CGFloat {
    #if os(macOS)
        guard FreesideFont.screenshotDynamicTypeSize != nil else { return value }
        return value * FreesideFont.size(of: style) / 16
    #else
        return value
    #endif
}

struct RunProposalRevisionSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var expectedCostText: String
    @State private var componentCount: Int
    @State private var touchesControlPlane: Bool
    private let originalFacts: Components.Schemas.RunProposalFactsSnapshot
    let submit: (Components.Schemas.RunProposalRevisionInput) -> Void

    init(
        facts: Components.Schemas.RunProposalFactsSnapshot,
        submit: @escaping (Components.Schemas.RunProposalRevisionInput) -> Void
    ) {
        _expectedCostText = State(initialValue: String(facts.expected_cost_units))
        _componentCount = State(initialValue: facts.scope.component_count)
        _touchesControlPlane = State(initialValue: facts.scope.touches_control_plane)
        originalFacts = facts
        self.submit = submit
    }

    private var revision: Components.Schemas.RunProposalRevisionInput? {
        guard let expectedCost = DecisionDetailView.parseExpectedCost(expectedCostText) else {
            return nil
        }
        return DecisionDetailView.runProposalRevision(
            from: originalFacts,
            expectedCost: expectedCost,
            componentCount: componentCount,
            touchesControlPlane: touchesControlPlane)
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                Form {
                    LabeledContent("Intent", value: "Implement subject")
                        .listRowBackground(Color.ground2)
                    LabeledContent("Expected cost (units)") {
                        expectedCostField
                    }
                    .listRowBackground(Color.ground2)
                    Stepper("Components: \(componentCount)", value: $componentCount, in: 1...32)
                        .listRowBackground(Color.ground2)
                    LabeledContent(
                        "Declared paths", value: "\(originalFacts.scope.declared_path_count)"
                    )
                    .listRowBackground(Color.ground2)
                    Toggle("Touches control plane", isOn: $touchesControlPlane)
                        .listRowBackground(Color.ground2)
                }
                .formStyle(.grouped)
                .font(FreesideFont.body)
                .foregroundStyle(Color.ink)
                .navigationTitle("Start with changes")
                .tint(.accentText)
                .scrollContentBackground(.hidden)

                // The submit is a body control in the primary recipe, not a
                // toolbar item; Return and Escape still reach it.
                FreesideSheetActionRow(
                    submitLabel: "Submit",
                    isSubmitEnabled: revision != nil,
                    submit: {
                        if let revision {
                            submit(revision)
                            dismiss()
                        }
                    },
                    cancel: { dismiss() })
            }
            .background(Color.ground)
        }
        .frame(minWidth: 380, minHeight: 280)
    }

    /// The project-owned revision composition without Form and TextField,
    /// whose AppKit-backed controls ImageRenderer cannot draw off-screen.
    func screenshotContent() -> some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Start with changes")
                .font(FreesideFont.largeTitle)
            VStack(alignment: .leading, spacing: 8) {
                KeywordLabel(text: "Intent")
                Text("Implement subject")
                Divider()
                KeywordLabel(text: "Expected cost")
                Text("\(expectedCostText) units")
                Divider()
                KeywordLabel(text: "Components")
                Text("\(componentCount)")
                Divider()
                KeywordLabel(text: "Declared paths")
                Text("\(originalFacts.scope.declared_path_count)")
                Divider()
                KeywordLabel(text: "Touches control plane")
                Text(touchesControlPlane ? "Yes" : "No")
            }
            .padding(14)
            .freesideCard()
            Text("Expected cost must be a whole number from 1 to 1,000,000 units.")
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
            FreesideSheetActionRow(
                submitLabel: "Submit",
                isSubmitEnabled: revision != nil,
                submit: {}, cancel: {})
        }
        .padding(24)
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
    }

    @ViewBuilder private var expectedCostField: some View {
        let field = TextField("Expected cost", text: $expectedCostText)
            .labelsHidden()
            .accessibilityLabel("Expected cost")
            .multilineTextAlignment(.trailing)

        #if os(iOS)
            field.keyboardType(.numberPad)
        #else
            field
        #endif
    }
}

struct RunProposalSnoozeSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var until: Date
    private let now: Date
    private let screenshotTimeZone: TimeZone
    let submit: (Date) -> Void

    init(
        now: Date = Date(),
        screenshotTimeZone: TimeZone = .current,
        submit: @escaping (Date) -> Void
    ) {
        self.now = now
        _until = State(initialValue: now.addingTimeInterval(60 * 60))
        self.screenshotTimeZone = screenshotTimeZone
        self.submit = submit
    }

    static func isValidSnooze(until: Date, now: Date) -> Bool {
        until > now
    }

    private var formattedScreenshotUntil: String {
        let formatter = DateFormatter()
        formatter.dateStyle = .medium
        formatter.timeStyle = .short
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = screenshotTimeZone
        return formatter.string(from: until)
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                Form {
                    DatePicker(
                        "Snooze until", selection: $until, in: now...,
                        displayedComponents: [.date, .hourAndMinute]
                    )
                    .listRowBackground(Color.ground2)
                }
                .formStyle(.grouped)
                .font(FreesideFont.body)
                .foregroundStyle(Color.ink)
                .navigationTitle("Snooze proposal")
                .tint(.accentText)
                .scrollContentBackground(.hidden)

                FreesideSheetActionRow(
                    submitLabel: "Snooze",
                    // Against the current time, not the sheet's opening one:
                    // a chosen moment that has since passed is no longer a
                    // snooze. The screenshot composition uses the injected
                    // `now` instead, so its golden stays deterministic.
                    isSubmitEnabled: Self.isValidSnooze(until: until, now: Date()),
                    submit: {
                        guard Self.isValidSnooze(until: until, now: Date()) else { return }
                        submit(until)
                        dismiss()
                    },
                    cancel: { dismiss() })
            }
            .background(Color.ground)
        }
        .frame(minWidth: 380, minHeight: 220)
    }

    /// The project-owned snooze composition without Form and DatePicker,
    /// whose AppKit-backed controls ImageRenderer cannot draw off-screen.
    func screenshotContent() -> some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Snooze proposal")
                .font(FreesideFont.largeTitle)
            VStack(alignment: .leading, spacing: 8) {
                KeywordLabel(text: "Snooze until")
                Text(formattedScreenshotUntil)
                    .font(FreesideFont.body)
            }
            .padding(14)
            .freesideCard()
            Text("The proposal returns to the inbox at this date and time.")
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
            FreesideSheetActionRow(
                submitLabel: "Snooze",
                isSubmitEnabled: Self.isValidSnooze(until: until, now: now),
                submit: {}, cancel: {})
        }
        .padding(24)
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
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
