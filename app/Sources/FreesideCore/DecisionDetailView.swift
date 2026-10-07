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
    private struct SummaryRevealRequest: Equatable {
        let identity: DecisionSummaryIdentity
        let nonce = UUID()
    }

    private enum ScrollTarget: Hashable {
        case technicalDetails
        case summaryReport(DecisionSummaryIdentity)
    }

    /// A consequential action awaiting its seal. Identified by action and
    /// item version, so the sheet it presents is bound to one reviewed
    /// snapshot and a version change replaces rather than reuses it.
    private struct PendingConfirmation: Identifiable {
        let action: Components.Schemas.Action
        let reviewedSnapshot: Components.Schemas.AttentionItemSnapshot

        var id: String { "\(action.rawValue)@\(reviewedSnapshot.item.item_version)" }
    }

    private enum ProposalEditor: String, Identifiable {
        case revision
        case effectRevision
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
    @State private var actionDetailsRevealRequest: TechnicalDetailsRevealRequest?
    @State private var sectionPreferences: DecisionSectionPreferences
    @State private var inspectorPresented: Bool
    @State private var detailWidth: CGFloat = 0
    @State private var recommendationVisible = true
    @State private var provenanceExpanded = false
    @State private var lostResponseExpanded = false
    @State private var alternativeSelections: [String: Components.Schemas.AdjudicationRoute]
    /// The finding rows the operator opened, by finding id. Empty by default:
    /// a finding_adjudication card leads with collapsed rows so its actions
    /// stay in the first viewport (#1107).
    @State private var expandedFindings: Set<String>
    @State private var expandedSummaryReports: Set<DecisionSummaryIdentity> = []
    @State private var summaryRevealRequest: SummaryRevealRequest?
    @State private var expandedDisclosures: Set<DecisionDisclosure>
    private let expandsSummaryReports: Bool
    private let attachments: AttachmentLoader
    private let graphics: DecisionGraphicPresentations
    private let loadsAttachments: Bool
    private let showsValidationProgress: Bool
    private let now: Date
    private let itemID: String
    private let externalDetailsRevealRequest: TechnicalDetailsRevealRequest?
    private let onConsumeDetailsRevealRequest: (UUID) -> Void
    private let externalInspectorPresented: Binding<Bool>?
    private let onSelectItem: (String) -> Void

    @MainActor
    init(
        store: InboxStore,
        itemID: String,
        detailsExpanded: Bool = false,
        expandedFindings: Set<String> = [],
        alternativeSelections: [String: Components.Schemas.AdjudicationRoute] = [:],
        expandsSummaryReports: Bool = false,
        expandedDisclosures: Set<DecisionDisclosure> = [],
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
        _alternativeSelections = State(initialValue: alternativeSelections)
        _expandedDisclosures = State(initialValue: expandedDisclosures)
        attachments = store.attachments
        self.itemID = itemID
        self.expandsSummaryReports = expandsSummaryReports
        self.externalDetailsRevealRequest = detailsRevealRequest
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
                                effectProposalFacts: model.effectProposalFacts,
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
                        .onChange(of: summaryRevealRequest) {
                            if let summaryRevealRequest {
                                scrollProxy.scrollTo(
                                    ScrollTarget.summaryReport(summaryRevealRequest.identity), anchor: .top)
                            }
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
                        TaskProposalRevisionSheet(facts: facts) { revision in
                            Task { await model.submitTaskProposalRevision(revision) }
                        }
                    }
                case .effectRevision:
                    if let facts = model.effectProposalFacts {
                        EffectProposalRevisionSheet(facts: facts) { revision in
                            Task { await model.submitEffectProposalRevision(revision) }
                        }
                    }
                case .snooze:
                    TaskProposalSnoozeSheet { until in
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
                    ) { message, _ in
                        await model.submitDiscuss(message: message)
                    }
                case .requestChanges:
                    MessageComposerSheet(
                        title: "Request changes",
                        prompt: "Describe the revision the specification needs.",
                        submitLabel: "Request changes",
                        byteLimit: 8192
                    ) { message, _ in
                        await model.submitRequestChanges(message: message)
                    }
                case .answerAndRetry:
                    MessageComposerSheet(
                        title: "Answer and retry",
                        prompt: "Answer the agent's question and choose what to do next.",
                        submitLabel: "Answer and retry", byteLimit: 8192,
                        routeOptions: AgentQuestionPresentation.answerRoutes(for: model.snapshot?.item)
                    ) { message, route in
                        await model.submitAnswer(
                            .answer_and_retry, message: message,
                            answerRoute: route
                                ?? AgentQuestionPresentation.answerRoute(for: model.snapshot?.item))
                    }
                case .answerWithoutRetry:
                    MessageComposerSheet(
                        title: "Answer without retry",
                        prompt: "Record the answer and conclude the question without restarting work.",
                        submitLabel: "Record answer", byteLimit: 8192
                    ) { message, _ in
                        await model.submitAnswer(.answer_without_retry, message: message)
                    }
                case .returnToAgent:
                    MessageComposerSheet(
                        title: "Return to agent",
                        prompt: "Describe what the agent should change before the work returns for review.",
                        submitLabel: "Return to agent", byteLimit: 8192
                    ) { message, _ in
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
            .sheet(item: $pendingConfirmation) { confirmation in
                ConsequenceSheet(
                    action: confirmation.action,
                    item: confirmation.reviewedSnapshot.item,
                    submit: {
                        pendingConfirmation = nil
                        Task {
                            await model.submitConfirmed(
                                confirmation.action,
                                reviewedSnapshot: confirmation.reviewedSnapshot)
                        }
                    },
                    cancel: { pendingConfirmation = nil })
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

    private var detailsRevealRequest: TechnicalDetailsRevealRequest? {
        actionDetailsRevealRequest ?? externalDetailsRevealRequest
    }

    private func consumeDetailsRevealRequest(_ nonce: UUID) {
        if actionDetailsRevealRequest?.nonce == nonce {
            actionDetailsRevealRequest = nil
            Task { await model.recordTrustFailureInspection() }
        } else {
            onConsumeDetailsRevealRequest(nonce)
        }
    }

    private func revealTechnicalDetailsIfRequested(using scrollProxy: ScrollViewProxy) {
        guard let detailsRevealRequest, detailsRevealRequest.itemID == itemID else { return }
        detailsExpanded.wrappedValue = true
        #if os(macOS)
            inspectorBinding.wrappedValue = true
        #else
            withAnimation {
                scrollProxy.scrollTo(ScrollTarget.technicalDetails, anchor: .top)
            }
            model.emitDetailsOpenedBeforeActing()
            consumeDetailsRevealRequest(detailsRevealRequest.nonce)
        #endif
    }

    #if os(macOS)
        private func revealTechnicalDetailsInInspectorIfRequested(
            using scrollProxy: ScrollViewProxy
        ) {
            guard let detailsRevealRequest, detailsRevealRequest.itemID == itemID else { return }
            detailsExpanded.wrappedValue = true
            withAnimation {
                scrollProxy.scrollTo(ScrollTarget.technicalDetails, anchor: .top)
            }
            model.emitDetailsOpenedBeforeActing()
            consumeDetailsRevealRequest(detailsRevealRequest.nonce)
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
                        Group {
                            if let specApprovalReader {
                                VStack(spacing: 0) {
                                    HStack(alignment: .top) {
                                        VStack(alignment: .leading, spacing: 4) {
                                            Text(
                                                specApprovalReader == .specification
                                                    ? "Specification" : "Specification changes"
                                            )
                                            .font(FreesideFont.sectionTitle)
                                            Label("Drag the divider to resize", systemImage: "arrow.left.and.right")
                                                .font(FreesideFont.caption)
                                                .foregroundStyle(Color.inkDim)
                                        }
                                        Spacer()
                                        Button {
                                            self.specApprovalReader = nil
                                        } label: {
                                            Label("Close reader", systemImage: "xmark")
                                                .labelStyle(.iconOnly)
                                        }
                                        .buttonStyle(.plain)
                                        .help("Close reader and show evidence and details")
                                    }
                                    .padding()
                                    Divider()
                                    SpecApprovalReaderViewport {
                                        specApprovalReaderContent(specApprovalReader, item: item)
                                    }
                                }
                            } else {
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
                            }
                        }
                        .background(Color.sidebarGround)
                        .inspectorColumnWidth(
                            min: specApprovalReader == nil ? 280 : 320,
                            ideal: specApprovalReader == nil ? 340 : 480,
                            max: specApprovalReader == nil ? 440 : 720)
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

    private var capabilityRetryIsPresented: Binding<Bool> {
        Binding(
            get: { capabilityRetrySnapshot != nil },
            set: { presented in
                if !presented { capabilityRetrySnapshot = nil }
            })
    }

    @ViewBuilder
    private func card(
        _ item: Components.Schemas.AttentionItem,
        proposalFacts: Components.Schemas.TaskProposalFactsSnapshot?,
        effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot? = nil,
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
            if DecisionCardComposition.rendersAsk(for: item) {
                Text(AttentionDisplay.ask(item))
                    .font(FreesideFont.sectionTitle)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
            }
            // The ask and the daemon's reason are one question and its answer,
            // so nothing renders between them. The reason stays labeled
            // because the daemon writes it as a sentence fragment. A type
            // whose reason is the agent's summary shows it once, under the
            // unverified claim label, and gets no Context section (#1098).
            // A decision-first type moves it: see `reasonPlacement(for:)`.
            let reasonPlacement =
                composition.rendersContext(for: item)
                ? DecisionCardComposition.reasonPlacement(for: item) : nil
            switch reasonPlacement {
            case .context:
                context(item)
            case .underAsk:
                reasonUnderAsk(item)
            case .recordedContext, nil:
                EmptyView()
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
                            effectProposalFacts: effectProposalFacts,
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
                                    effectProposalFacts: effectProposalFacts,
                                    rendersInteractiveControls: rendersInteractiveControls,
                                    accessibilityLayout: accessibilityLayout,
                                    inspectorPresented: inspectorPresented)
                                if index + 1 == composition.reviewingActionInsertionIndex {
                                    reviewingAction(item)
                                }
                            }
                        }
                        .frame(maxWidth: 560, alignment: .topLeading)

                        VStack(alignment: .leading, spacing: 16) {
                            actionRegion(
                                item,
                                stackedLayout: accessibilityLayout || compactLayout,
                                includesReviewing: composition.reviewingActionInsertionIndex
                                    == nil,
                                rendersInteractiveControls: rendersInteractiveControls
                            )
                            if reasonPlacement == .recordedContext {
                                recordedContext(item)
                            }
                        }
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
                            effectProposalFacts: effectProposalFacts,
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
                            if reasonPlacement == .recordedContext {
                                recordedContext(item)
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
                        effectProposalFacts: effectProposalFacts,
                        rendersInteractiveControls: rendersInteractiveControls,
                        accessibilityLayout: accessibilityLayout,
                        inspectorPresented: inspectorPresented)
                    if index + 1 == composition.actionInsertionIndex {
                        actions(
                            item,
                            stackedLayout: accessibilityLayout || compactLayout,
                            includesReviewing: composition.reviewingActionInsertionIndex == nil)
                        if reasonPlacement == .recordedContext {
                            recordedContext(item)
                        }
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
                    recommendationBlock(
                        recommendation,
                        item: item,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
                let actionClaims = item.agent_claims.filter {
                    $0.text != nil && $0.label != AgentClaimLabels.summary
                        && !AgentClaimLabels.isApprovalMaterial($0.label)
                }
                // A card that leads with its claim draws it in the card, so
                // a copy here would print the claim twice.
                if !actionClaims.isEmpty,
                    !DecisionCardComposition.forType(item._type).leadsWithItsClaim
                {
                    let register = unverified(
                        item, rendersInteractiveControls: rendersInteractiveControls)
                    cardSection("Agent claims", unverified: register) {
                        claimRows(actionClaims, unverified: register)
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
        proposalFacts: Components.Schemas.TaskProposalFactsSnapshot?,
        effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot?,
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
            // effectProposalRows is empty for an effect kind this card has
            // no rows for (it draws a closure and a follow-up filing), so
            // the titled section is suppressed rather than drawn empty.
            if let effectProposalFacts,
                case let effectRows = AttentionDisplay.effectProposalRows(effectProposalFacts),
                !effectRows.isEmpty
            {
                cardSection("Authenticated proposal") {
                    ForEach(effectRows) { fact in
                        factRow(fact.label, value: fact.value)
                    }
                }
            }
            // A filing's title and body are the agent's words. They stay in
            // this module, ahead of the actions, so the operator reads all
            // that would be filed before deciding, and each draws in its own
            // unverified section so neither shares one with a daemon fact.
            if let effectProposalFacts,
                let proposed = AttentionDisplay.proposedIssueText(effectProposalFacts)
            {
                let register = unverified(item, rendersInteractiveControls: rendersInteractiveControls)
                proposedIssueTextSection("Proposed title", text: proposed.title, unverified: register)
                proposedIssueTextSection("Proposed body", text: proposed.body, unverified: register)
            }
        case .agentQuestion:
            agentQuestionLead(item, rendersInteractiveControls: rendersInteractiveControls)
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
                    recommendationBlock(
                        recommendation,
                        item: item,
                        rendersInteractiveControls: rendersInteractiveControls)
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
                    showsBars: graphics.diminishingYield != nil,
                    isExpanded: item._type == .ready_for_final_review
                        ? disclosure(.reviewYield) : nil)
            }
        case .stopCause:
            if let presentation = DecisionStopCausePresentation(item) {
                DecisionStopCauseModuleView(presentation: presentation)
            }
        case .findingFacts:
            // The finding cards lead the §9 finding_adjudication card
            // (docs/plan.md §9 revision 78), so this module renders ahead of
            // actionInsertionIndex on every layout; see
            // DecisionCardComposition.forType(.finding_adjudication).
            if let adjudication = item.finding_adjudication?.value1 {
                findingCards(
                    adjudication,
                    rendersInteractiveControls: rendersInteractiveControls)
            }
        case .factBlock:
            factBlocks(item, rendersInteractiveControls: rendersInteractiveControls)
        case .summary:
            if item._type == .ready_for_final_review {
                readySummary(item, rendersInteractiveControls: rendersInteractiveControls)
            } else {
                agentSummary(
                    composition.summaries(from: item.agent_claims),
                    unverified: unverified(
                        item, rendersInteractiveControls: rendersInteractiveControls))
            }
        case .claims:
            if drawsClaimsInCard(composition, at: moduleIndex) {
                claims(
                    composition.claims(
                        from: item.agent_claims,
                        at: moduleIndex,
                        prominentClaimIndex: graphics.prominentClaimIndex),
                    accessibilityLayout: accessibilityLayout,
                    prominent: composition.claimsAreProminent(at: moduleIndex),
                    unverified: unverified(
                        item, rendersInteractiveControls: rendersInteractiveControls))
            }
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
            if item._type == .ready_for_final_review {
                summaryReports(item, rendersInteractiveControls: rendersInteractiveControls)
            }
            #if os(iOS)
                details(
                    item,
                    accessibilityLayout: accessibilityLayout,
                    rendersInteractiveControls: rendersInteractiveControls)
            #endif
        }
    }

    /// macOS lists claims in the action region and the inspector, so a
    /// claims module draws in the card there only where the claim is the
    /// card's own lead (D08). iOS has neither place and draws every module.
    private func drawsClaimsInCard(
        _ composition: DecisionCardComposition, at moduleIndex: Int
    ) -> Bool {
        #if os(macOS)
            composition.leadsWithItsClaim && composition.claimsAreProminent(at: moduleIndex)
        #else
            true
        #endif
    }

    /// One field of the issue a follow-up filing would create, in full and
    /// as plain text: the operator approves the exact text that would be
    /// sent, so nothing is truncated and Markdown is not interpreted.
    private func proposedIssueTextSection(
        _ title: String,
        text: String,
        unverified: UnverifiedRegister
    ) -> some View {
        cardSection(title, unverified: unverified) {
            Text(AttentionDisplay.screenedIssueTextExplanation)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            Text(verbatim: text)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    @ViewBuilder
    private func agentSummary(
        _ claims: [Components.Schemas.AgentClaim],
        unverified: UnverifiedRegister
    ) -> some View {
        if !claims.isEmpty {
            cardSection("Agent summary", unverified: unverified) {
                unverifiedSentence(unverified)
                ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
                    switch unverified.frame {
                    case .dashedCard:
                        Text("Source: agent invocation `\(Self.producerInvocationID(claim))`")
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
                            rendersInteractiveControls: unverified.rendersInteractiveControls)
                    case .spaced:
                        if let text = claim.text {
                            summaryText(text.content, mediaType: text.media_type)
                        }
                        SentenceDisclosure(
                            label: "Source and Original Report",
                            isExpanded: disclosure(.claimSource(claim))
                        ) {
                            fullSummaryReport(
                                claim,
                                rendersInteractiveControls: unverified.rendersInteractiveControls
                            )
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.top, 8)
                        }
                    }
                }
            }
        }
    }

    @ViewBuilder
    private func readySummary(_ item: Components.Schemas.AttentionItem, rendersInteractiveControls: Bool) -> some View {
        let claims = DecisionCardComposition.forType(item._type).summaries(from: item.agent_claims)
        let register = unverified(item, rendersInteractiveControls: rendersInteractiveControls)
        cardSection("Agent summary", unverified: register) {
            unverifiedSentence(register)
            if claims.isEmpty {
                Text("Inline summary unavailable. Any retained report is listed with the claim attachments.")
            }
            ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
                if let text = claim.text {
                    let presentation = DecisionSummaryPresentation(text)
                    if presentation.isExcerpt {
                        Text("Report excerpt (incomplete)").font(FreesideFont.caption)
                    }
                    summaryText(presentation.lead, mediaType: text.media_type)
                    if presentation.isExcerpt { Text("…").accessibilityLabel("Excerpt ends here") }
                    if let concerns = presentation.concerns {
                        Text("Remaining concerns").font(FreesideFont.sans(.callout, weight: .semibold))
                        summaryText(concerns, mediaType: text.media_type)
                    }
                    if presentation.concernsUnknown {
                        Text(
                            "Concerns have not been extracted; read the full report. Concerns may be outside this excerpt."
                        )
                        .foregroundStyle(Color.inkDim)
                    }
                    if rendersInteractiveControls {
                        Button("Read full report") {
                            let identity = DecisionSummaryIdentity(itemID: item.id, claim: claim)
                            expandedSummaryReports.insert(identity)
                            summaryRevealRequest = SummaryRevealRequest(identity: identity)
                        }
                    } else {
                        Text("Read full report ↓").foregroundStyle(Color.inkDim)
                    }
                }
            }
        }
    }

    @ViewBuilder
    private func summaryReports(_ item: Components.Schemas.AttentionItem, rendersInteractiveControls: Bool) -> some View
    {
        let claims = DecisionCardComposition.forType(item._type).summaries(from: item.agent_claims)
        ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
            let identity = DecisionSummaryIdentity(itemID: item.id, claim: claim)
            let expanded = Binding(
                get: { expandedSummaryReports.contains(identity) || expandsSummaryReports },
                set: {
                    if $0 {
                        expandedSummaryReports.insert(identity)
                    } else {
                        expandedSummaryReports.remove(identity)
                        summaryRevealRequest = nil
                    }
                })
            cardSection(
                "Full agent report",
                unverified: unverified(
                    item, rendersInteractiveControls: rendersInteractiveControls)
            ) {
                if rendersInteractiveControls {
                    DisclosureGroup("Complete original report", isExpanded: expanded) {
                        fullSummaryReport(claim, rendersInteractiveControls: true)
                    }
                } else {
                    Text(expanded.wrappedValue ? "▾ Complete original report" : "▸ Complete original report")
                    if expanded.wrappedValue { fullSummaryReport(claim, rendersInteractiveControls: false) }
                }
            }
            .id(ScrollTarget.summaryReport(identity))
        }
    }

    private func fullSummaryReport(
        _ claim: Components.Schemas.AgentClaim, rendersInteractiveControls: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Source: agent invocation `\(Self.producerInvocationID(claim))`")
                .font(FreesideFont.caption).textSelection(.enabled)
            AttachmentRow(
                label: "Original report", digest: claim.digest, metadata: claim.metadata, attachments: attachments,
                loadsAttachments: false, text: claim.text, rendersInteractiveControls: rendersInteractiveControls)
        }
    }

    private func summaryText(_ content: String, mediaType: Components.Schemas.ClaimText.media_typePayload) -> some View
    {
        let attributed =
            mediaType == .text_sol_markdown
            ? try? AttributedString(
                markdown: content, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)) : nil
        return Text(attributed ?? AttributedString(content))
            .font(FreesideFont.callout)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    private static func producerInvocationID(_ claim: Components.Schemas.AgentClaim) -> String {
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

    /// The reason as the ask's own second line: the same daemon sentence the
    /// Context section carries, without the box and its label.
    @ViewBuilder
    private func reasonUnderAsk(_ item: Components.Schemas.AttentionItem) -> some View {
        if !item.reason.isEmpty {
            Text(item.reason)
                .font(FreesideFont.callout)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    /// The reason one disclosure away, below the actions, on a card whose
    /// lead already says what the operator is deciding.
    @ViewBuilder
    private func recordedContext(_ item: Components.Schemas.AttentionItem) -> some View {
        if !item.reason.isEmpty {
            SentenceDisclosure(
                label: "Recorded Context", isExpanded: disclosure(.recordedContext)
            ) {
                Text(item.reason)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.top, 8)
            }
        }
    }

    private func disclosure(_ disclosure: DecisionDisclosure) -> Binding<Bool> {
        Binding(
            get: { expandedDisclosures.contains(disclosure) },
            set: { expanded in
                if expanded {
                    expandedDisclosures.insert(disclosure)
                } else {
                    expandedDisclosures.remove(disclosure)
                }
            })
    }

    /// The Section 9 card facts for this item type, read from its typed fact
    /// fields (#724). Rendered from the `.facts` module, which every
    /// composition places ahead of its action region; a type whose lead is its
    /// own module contributes no rows and the section disappears rather than
    /// rendering an empty container. `DecisionFactPlacement` decides which
    /// rows stay beside the decision and which fold.
    @ViewBuilder
    private func factsSection(
        _ item: Components.Schemas.AttentionItem,
        includesCommitPlan: Bool
    ) -> some View {
        let placement = DecisionFactPlacement(
            item, includesCommitPlan: includesCommitPlan, now: now)
        if !placement.visible.isEmpty {
            cardSection("Facts") {
                ForEach(placement.visible) { fact in
                    factRow(fact.label, value: fact.value)
                }
            }
        }
        if !placement.folded.isEmpty {
            SentenceDisclosure(
                label: DecisionFactPlacement.foldedTitle, isExpanded: disclosure(.runDetails)
            ) {
                VStack(alignment: .leading, spacing: 8) {
                    ForEach(placement.folded) { fact in
                        factRow(fact.label, value: fact.value)
                    }
                }
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
                // A stacked row hugs its text, and a disclosure centers
                // content narrower than itself.
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.top, 8)
            }
        }
    }

    @ViewBuilder
    private func factBlocks(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let changeSummary = graphics.changeSummary {
            cardSection(
                "Change summary",
                unverified: unverified(
                    item, rendersInteractiveControls: rendersInteractiveControls)
            ) {
                Text(changeSummary.text)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Text(changeSummary.summary))
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

    /// The decisions the agent stopped on lead the card, each with its own
    /// question in the serif, so the shell draws no generic ask above them
    /// (visual audit D06). Who stopped and what blocks the run are facts, so
    /// they render once as fact rows below rather than as a preface the
    /// operator reads before reaching anything to answer (#1107).
    ///
    /// The daemon types the decision structure, but the question, the
    /// blocking explanation, the option labels, and the tradeoffs are all
    /// prose from the asking invocation's Question claim. Plan §9 has this
    /// type lead with "the question as a labeled agent claim, self-contained:
    /// what is blocked and any enumerated options", so each decision carries
    /// the unverified register label directly above its question; without it
    /// an operator would read agent prose as a daemon fact. The per-option
    /// marker stays on the recommendation it qualifies; it speaks for one
    /// option, not for the question around it.
    @ViewBuilder
    private func agentQuestionLead(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let presentation = AgentQuestionPresentation(item) {
            if let scope = presentation.scopeConflict {
                VStack(alignment: .leading, spacing: 8) {
                    KeywordLabel(text: "Required work outside scope")
                    Text(scope.paths.joined(separator: ", "))
                        .font(FreesideFont.itemTitle)
                    Text("Allowed paths: \(scope.declared_paths.joined(separator: ", "))")
                    Text("Candidate: \(AttentionDisplay.shortRevision(scope.head_sha))")
                    Text(
                        "Answer to keep scope and record the unmet work. To widen scope, stop and start a new run with a newly approved path policy."
                    )
                }
                .font(FreesideFont.callout)
                .fixedSize(horizontal: false, vertical: true)
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .freesideCard()
            }
            let register = unverified(
                item, rendersInteractiveControls: rendersInteractiveControls)
            ForEach(Array(presentation.decisions.enumerated()), id: \.offset) { _, decision in
                VStack(alignment: .leading, spacing: 8) {
                    sectionTitle("Agent question", unverified: register)
                    Text(decision.question)
                        .font(FreesideFont.sectionTitle)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(decision.whyBlocking)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                    ForEach(Array(decision.options.enumerated()), id: \.offset) { index, option in
                        agentQuestionOption(option, number: index + 1)
                            .padding(.top, index == 0 ? 4 : 0)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// One alternative the agent enumerated, bounded as its own panel so the
    /// label, the recommendation, and the complete tradeoff read as one
    /// option. A panel describes a choice and is not the control that makes
    /// it: the answer still goes through the card's answer actions, so it is
    /// one accessibility element with no tap target.
    private func agentQuestionOption(
        _ option: AgentQuestionPresentation.Option,
        number: Int
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: "Option \(number)")
            Text(option.label)
                .font(FreesideFont.itemTitle)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            if option.recommended {
                Label("Agent recommends (unverified)", systemImage: "quote.bubble")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.accentText)
            }
            Text(option.tradeoffs)
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.vertical, 10)
        .padding(.leading, 13)
        .padding(.trailing, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.neutralWash)
        .overlay(alignment: .leading) {
            Rectangle().fill(Color.rule).frame(width: 3)
        }
        .accessibilityElement(children: .combine)
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
                        detail: "Bound by the daemon to this approval",
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
        rendersScrollableContent: Bool = true,
        expandsTechnicalDetails: Bool = false
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
                    rendersScrollableContent: rendersScrollableContent,
                    expandsTechnicalDetails: expandsTechnicalDetails)
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
            VStack(spacing: 0) {
                FreesideSheetHeader(
                    title: reader == .specification ? "Specification" : "Specification changes")
                SpecApprovalReaderViewport {
                    specApprovalReaderContent(reader, item: item)
                }
                FreesideSheetActionRow.done { specApprovalReader = nil }
            }
            .background(Color.ground2)
            .freesideSheetPresentation()
        }
    #endif

    @ViewBuilder
    private func claims(
        _ claims: [Components.Schemas.AgentClaim],
        accessibilityLayout: Bool,
        prominent: Bool,
        unverified: UnverifiedRegister
    ) -> some View {
        if !claims.isEmpty {
            if prominent {
                cardSection("Agent claims", unverified: unverified) {
                    claimRows(claims, unverified: unverified)
                }
            } else {
                lowerSection(
                    "Agent claims",
                    isExpanded: claimsExpanded,
                    accessibilityLayout: accessibilityLayout,
                    unverified: unverified
                ) {
                    claimRows(claims, unverified: unverified)
                }
            }
        }
    }

    @ViewBuilder
    private func claimRows(
        _ claims: [Components.Schemas.AgentClaim],
        unverified: UnverifiedRegister
    ) -> some View {
        unverifiedSentence(unverified)
        // Position is the only stable identity: two claims may bind the same
        // artifact under different labels and neither field is unique.
        ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
            if unverified.frame == .spaced, let text = claim.text {
                claimProse(
                    claim, text: text,
                    rendersInteractiveControls: unverified.rendersInteractiveControls)
            } else {
                // A claim without inline text keeps the attachment row in
                // every frame: its loading, unavailable, and failed states
                // and their retry are that row's own.
                AttachmentRow(
                    label: claim.label, digest: claim.digest,
                    metadata: claim.metadata,
                    attachments: attachments,
                    loadsAttachments: loadsAttachments,
                    text: claim.text,
                    rendersInteractiveControls: unverified.rendersInteractiveControls)
            }
        }
    }

    /// A text claim on a card whose agent sections are spaced: the claim's
    /// own words lead, and the identifiers that bind them (its label, media
    /// type, producing invocation, and digest) sit one disclosure away with
    /// a copy control each.
    @ViewBuilder
    private func claimProse(
        _ claim: Components.Schemas.AgentClaim,
        text: Components.Schemas.ClaimText,
        rendersInteractiveControls: Bool
    ) -> some View {
        summaryText(text.content, mediaType: text.media_type)
        SentenceDisclosure(
            label: "Source and Supporting Details",
            isExpanded: disclosure(.claimSource(claim))
        ) {
            VStack(alignment: .leading, spacing: 6) {
                ForEach(
                    Array(Self.claimSourceRows(claim, text: text).enumerated()), id: \.offset
                ) { _, row in
                    TechnicalDetailRow(
                        row: row, rendersInteractiveControls: rendersInteractiveControls)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.top, 8)
        }
    }

    static func claimSourceRows(
        _ claim: Components.Schemas.AgentClaim,
        text: Components.Schemas.ClaimText
    ) -> [AttentionDisplay.BindingRow] {
        [
            .init(label: "Label", value: claim.label),
            .init(label: "Media type", value: text.media_type.rawValue),
            .init(label: "Agent invocation", value: producerInvocationID(claim)),
            .init(label: "Claim digest", value: claim.digest),
        ]
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
        accessibilityLayout: Bool,
        rendersInteractiveControls: Bool
    ) -> some View {
        lowerSection(
            "Details",
            isExpanded: detailsExpanded,
            accessibilityLayout: accessibilityLayout
        ) {
            VStack(alignment: .leading, spacing: 6) {
                ForEach(Array(detailRows(item).enumerated()), id: \.offset) { _, row in
                    TechnicalDetailRow(
                        row: row, rendersInteractiveControls: rendersInteractiveControls)
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
                let cardLeadClaims = DecisionCardComposition.forType(item._type)
                    .cardLeadClaims(
                        from: item.agent_claims,
                        prominentClaimIndex: graphics.prominentClaimIndex)
                let attachmentClaims = item.agent_claims.filter {
                    $0.text == nil && !AgentClaimLabels.isApprovalMaterial($0.label)
                        && !cardLeadClaims.contains($0)
                }
                if !attachmentClaims.isEmpty {
                    let register = unverified(
                        item, rendersInteractiveControls: rendersInteractiveControls)
                    inspectorSection(
                        "Agent claims",
                        isExpanded: claimsExpanded,
                        unverified: register
                    ) {
                        claimRows(attachmentClaims, unverified: register)
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
                            TechnicalDetailRow(
                                row: row, rendersInteractiveControls: rendersInteractiveControls)
                        }
                    }
                }
                .id(ScrollTarget.technicalDetails)
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
                .textSelection(.enabled)
            }
            .environment(\.dynamicTypeSize, dynamicTypeSize)
        }
    #endif

    @ViewBuilder
    private func proposalRows(_ facts: Components.Schemas.TaskProposalFactsSnapshot) -> some View {
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
        proposalFacts: Components.Schemas.TaskProposalFactsSnapshot? = nil,
        effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot? = nil,
        compactLayout: Bool = false,
        detailWidth: CGFloat = 560,
        inspectorPresented: Bool = false,
        actionRegionFrameChanged: ((CGRect) -> Void)? = nil
    ) -> some View {
        let wideLayout = detailWidth >= 1_000 && dynamicTypeSize < .accessibility1
        card(
            item,
            proposalFacts: proposalFacts,
            effectProposalFacts: effectProposalFacts,
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
        item: Components.Schemas.AttentionItem,
        expandsTechnicalDetails: Bool = false
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(reader == .specification ? "Specification" : "Specification changes")
                .font(FreesideFont.sectionTitle)
            specApprovalReaderContent(
                reader,
                item: item,
                rendersScrollableContent: false,
                expandsTechnicalDetails: expandsTechnicalDetails)
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

    // One card per finding (plan §9 revision 78, visual audit D09). A card
    // shows its finding's exact message, the proposed route, and who proposed
    // it; everything else about that finding, the alternative-route picker
    // included, sits in the card's own disclosure. Nothing drawn from a
    // proposal renders outside its card, so a picker can only belong to the
    // finding it sits under, and no content spans the action region. A held
    // alternative shows on the face, so closing a card never hides a choice
    // the operator can still send.
    //
    // The registers are told apart by where content stands, not by a border
    // each: the daemon's message under the card's heading and its
    // coordinates under "Daemon facts", the route and the rationale each
    // under the producer label.
    @ViewBuilder
    private func findingCards(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        rendersInteractiveControls: Bool
    ) -> some View {
        ForEach(Array(binding.proposals.enumerated()), id: \.element.finding_id) {
            index, proposal in
            let card = FindingCardPresentation(proposal, number: index + 1, binding: binding)
            VStack(alignment: .leading, spacing: 10) {
                VStack(alignment: .leading, spacing: 10) {
                    KeywordLabel(text: card.heading)
                    if !card.message.isEmpty {
                        Text(card.message)
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text(card.messageAccessibilityLabel))
                .accessibilityAddTraits(.isHeader)
                VStack(alignment: .leading, spacing: 4) {
                    findingProducerLabel(
                        card, rendersInteractiveControls: rendersInteractiveControls)
                    Text(card.route)
                        .font(FreesideFont.itemTitle)
                        .foregroundStyle(Color.accentText)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityLabel(Text(card.routeAccessibilityLabel))
                    if let selected = alternativeSelections[card.id] {
                        Text(FindingCardPresentation.selectionNotice(selected))
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                            .accessibilityLabel(
                                Text(card.selectionAccessibilityLabel(selected)))
                    }
                }
                DisclosureGroup(isExpanded: findingExpansion(card.id)) {
                    findingDetail(
                        card,
                        selection: alternativeSelection(for: proposal),
                        rendersInteractiveControls: rendersInteractiveControls)
                } label: {
                    Text(FindingCardPresentation.disclosureTitle)
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.ink)
                        .accessibilityLabel(Text(card.disclosureAccessibilityLabel))
                }
                .tint(.accentText)
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .freesideCard()
        }
    }

    /// The producer label, with the explanation of "(unverified)" one button
    /// away where the label carries the word (visual audit D03).
    @ViewBuilder
    private func findingProducerLabel(
        _ card: FindingCardPresentation,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let keyword = card.producerUnverifiedKeyword {
            UnverifiedLabel(
                text: keyword, carriesInfo: true,
                rendersInteractiveControls: rendersInteractiveControls)
        } else {
            KeywordLabel(text: card.producerLabel)
        }
    }

    private func findingDetail(
        _ card: FindingCardPresentation,
        selection: Binding<Components.Schemas.AdjudicationRoute?>,
        rendersInteractiveControls: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            // The label repeats here because the rationale below it is the
            // producer's too, and an open disclosure can scroll the card's
            // face out of view.
            findingProducerLabel(card, rendersInteractiveControls: rendersInteractiveControls)
            Text(card.rationale)
                .fixedSize(horizontal: false, vertical: true)
            ForEach(card.proposalRows) { fact in
                factRow(fact.label, value: fact.value)
            }
            if !card.evidence.isEmpty {
                findingList(card.evidenceTitle, values: card.evidence)
            }
            // The finding's coordinates are daemon-authenticated, so they
            // keep their own titled register inside the disclosure, never
            // mixed into the producer's content around them (#892).
            findingSection("Daemon facts") {
                ForEach(card.daemonFacts) { fact in
                    factRow(fact.label, value: fact.value)
                }
            }
            if !card.assumptions.isEmpty {
                findingList("Assumptions", values: card.assumptions)
            }
            if !card.citedRules.isEmpty {
                findingList("Cited repository rules", values: card.citedRules)
            }
            if !card.alternatives.isEmpty {
                findingSection("Viable alternatives") {
                    ForEach(card.alternatives, id: \.route) { alternative in
                        VStack(alignment: .leading, spacing: 3) {
                            Text(alternative.label)
                                .font(FreesideFont.sans(.callout, weight: .semibold))
                            Text(alternative.consequence)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                    if rendersInteractiveControls {
                        Picker("Selected route", selection: selection) {
                            Text("Keep recommendation")
                                .tag(Optional<Components.Schemas.AdjudicationRoute>.none)
                            ForEach(card.alternatives, id: \.route) { alternative in
                                Text(alternative.label)
                                    .tag(Optional(alternative.route))
                            }
                        }
                        .pickerStyle(.menu)
                        .accessibilityLabel(Text("Selected route, \(card.heading)"))
                    } else {
                        factRow("Selected route", value: "Keep recommendation")
                    }
                }
            }
            if !card.gatingQuestions.isEmpty {
                findingList("Gating questions", values: card.gatingQuestions)
            }
        }
        .font(FreesideFont.callout)
        .foregroundStyle(Color.ink)
        // A stacked row hugs its text, and a disclosure centers content
        // narrower than itself.
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.top, 10)
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

    /// A titled group inside a finding's disclosure. The card is the only
    /// frame, so a group is a keyword and its rows.
    private func findingSection(
        _ title: String,
        @ViewBuilder content: () -> some View
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: title)
            content()
        }
    }

    private func findingList(_ title: String, values: [String]) -> some View {
        findingSection(title) {
            ForEach(Array(values.enumerated()), id: \.offset) { _, value in
                Label(value, systemImage: "circle.fill")
                    .labelStyle(FindingListLabelStyle())
            }
        }
    }

    /// What accepting covers, for an action that accepts the routes of the
    /// findings `item` binds; nil for any other action or item.
    private func findingAcceptanceScope(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem
    ) -> String? {
        guard action == .accept_recommended_route,
            let binding = item.finding_adjudication?.value1
        else { return nil }
        return FindingCardPresentation.acceptanceScope(findingCount: binding.proposals.count)
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

    static func taskProposalRevision(
        from facts: Components.Schemas.TaskProposalFactsSnapshot,
        expectedCost: Int,
        componentCount: Int,
        touchesControlPlane: Bool
    ) -> Components.Schemas.TaskProposalRevisionInput? {
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

    /// The one bounded edit a closure proposal allows: whether the PR closes
    /// the issue. The daemon owns the target, provenance, and origin and
    /// rebuilds the proposal, so the client sends only the flipped flag, and
    /// only when it actually differs (the daemon rejects an unchanged
    /// `resolves`).
    static func effectProposalRevision(
        from facts: Components.Schemas.EffectProposalFactsSnapshot,
        resolves: Bool
    ) -> Components.Schemas.EffectProposalRevisionInput? {
        guard let closure = facts.source_issue_closure?.value1,
            resolves != closure.resolves
        else { return nil }
        return .init(source_issue_closure: .init(resolves: resolves))
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
            priorProposalDigest: model.proposalFacts?.supersedes?.value1.proposal_digest
                ?? model.effectProposalFacts?.supersedes?.value1.proposal_digest,
            proposalDigest: model.proposalFacts?.proposal_digest
                ?? model.effectProposalFacts?.proposal_digest
        )
        // The card's "Bound to" row abbreviates the head and base to eight
        // characters, but the approval authorizes this exact prospective
        // merge, so Details keeps the full head, base ref and SHA, and
        // publication identity for audit and copy (two bases sharing an
        // eight-character prefix are otherwise indistinguishable).
        if let merge = model.effectProposalFacts?.source_issue_closure?.value1.merge {
            rows.append(.init(label: "Bound head", value: merge.candidate_head_sha))
            rows.append(.init(label: "Bound base", value: "\(merge.base_ref)@\(merge.base_sha)"))
            rows.append(.init(label: "Publication identity", value: merge.publication_identity))
        }
        if let facts = model.effectProposalFacts {
            rows.append(contentsOf: AttentionDisplay.followUpFilingDetailRows(facts))
        }
        rows.append(
            contentsOf: AttentionDisplay.unavailableActionRows(actionRanking(item).unavailable))
        return rows
    }

    @ViewBuilder
    private func proposalRevisionRows(
        _ prior: Components.Schemas.TaskProposalRevisionFacts
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
                    "Decision applied: \(AttentionDisplay.label(record.action, for: model.snapshot?.item))",
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
        item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        // The block's label names its register without the word, so a card
        // that keeps the repeated sentence draws the label as it is and the
        // sentence under it; on demand, the label itself says "(unverified)".
        let register =
            recommendation.register.isUnverifiedClaim
            ? unverified(item, rendersInteractiveControls: rendersInteractiveControls) : nil
        // On the finding card the recommendation is the batch action's own
        // line under the cards (plan §9 revision 78, visual audit D09): no
        // second frame, its reason, then what accepting covers.
        let acceptanceScope = findingAcceptanceScope(recommendation.action, item: item)
        return cardSection(
            title: Group {
                if let register, register.explanation == .onDemand {
                    UnverifiedLabel(
                        text: recommendation.label, carriesInfo: true,
                        rendersInteractiveControls: register.rendersInteractiveControls)
                } else {
                    KeywordLabel(text: recommendation.label)
                }
            },
            dashed: register != nil,
            boxed: acceptanceScope == nil,
            border: .accentBorder,
            fill: .accentWash
        ) {
            if register?.explanation == .sentence {
                Text("Written by an agent, not checked by the daemon.")
                    .foregroundStyle(Color.inkDim)
            }
            if acceptanceScope == nil {
                KeywordLabel(text: "Why")
            }
            Text(recommendation.reason)
                .fixedSize(horizontal: false, vertical: true)
            if let acceptanceScope {
                Text(acceptanceScope)
                    .fixedSize(horizontal: false, vertical: true)
            }
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
                        factRow(fact.label, value: fact.value)
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

    /// A section's unverified register: set when an agent wrote the section's
    /// content. The "(unverified)" label, the dashed border, and where the
    /// explanation lives all follow from this one value rather than from the
    /// title's text, so a section cannot claim one and draw another.
    struct UnverifiedRegister {
        let explanation: DecisionCardComposition.UnverifiedExplanation
        let frame: DecisionCardComposition.AgentSectionFrame
        let rendersInteractiveControls: Bool
    }

    private func unverified(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> UnverifiedRegister {
        UnverifiedRegister(
            explanation: DecisionCardComposition.unverifiedExplanation(for: item._type),
            frame: DecisionCardComposition.agentSectionFrame(for: item._type),
            rendersInteractiveControls: rendersInteractiveControls)
    }

    /// A title that is a disclosure's own label draws the explanation's
    /// glyph as a mark only. The label is the control that opens the section,
    /// so a second button inside it would hand touch and VoiceOver the
    /// disclosure rather than the explanation; the section carries the
    /// sentence inside instead (`foldedUnverifiedSentence`).
    @ViewBuilder
    private func sectionTitle(
        _ title: String,
        unverified: UnverifiedRegister?,
        isDisclosureLabel: Bool = false
    ) -> some View {
        if let unverified {
            switch unverified.explanation {
            case .sentence:
                KeywordLabel(text: "\(title) (unverified)")
            case .onDemand:
                UnverifiedLabel(
                    text: title, carriesInfo: true,
                    rendersInteractiveControls: unverified.rendersInteractiveControls
                        && !isDisclosureLabel)
            }
        } else {
            KeywordLabel(text: title)
        }
    }

    /// The explanation a section repeats under its title on a card that does
    /// not offer it on demand.
    @ViewBuilder
    private func unverifiedSentence(_ unverified: UnverifiedRegister) -> some View {
        if unverified.explanation == .sentence {
            Text(UnverifiedLabel.explanation)
                .foregroundStyle(Color.inkDim)
        }
    }

    /// The explanation as the first line of a folded section on a card that
    /// otherwise offers it on demand: opening the section is the demand.
    @ViewBuilder
    private func foldedUnverifiedSentence(_ unverified: UnverifiedRegister?) -> some View {
        if unverified?.explanation == .onDemand {
            Text(UnverifiedLabel.explanation)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private func cardSection(
        _ title: String,
        unverified: UnverifiedRegister? = nil,
        border: Color = .rule,
        fill: Color = .ground,
        @ViewBuilder content: () -> some View
    ) -> some View {
        cardSection(
            title: sectionTitle(title, unverified: unverified),
            dashed: unverified != nil,
            boxed: unverified?.frame != .spaced,
            border: border,
            fill: fill,
            content: content)
    }

    @ViewBuilder
    private func cardSection(
        title: some View,
        dashed: Bool,
        boxed: Bool = true,
        border: Color = .rule,
        fill: Color = .ground,
        @ViewBuilder content: () -> some View
    ) -> some View {
        let section = VStack(alignment: .leading, spacing: 6) {
            title
            content()
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
        }
        if boxed {
            section
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 8).fill(fill))
                .overlay(
                    RoundedRectangle(cornerRadius: 8)
                        .strokeBorder(
                            border,
                            style: StrokeStyle(lineWidth: 1, dash: dashed ? [4, 3] : []))
                )
        } else {
            section.frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    @ViewBuilder
    private func lowerSection<Content: View>(
        _ title: String,
        isExpanded: Binding<Bool>,
        accessibilityLayout: Bool,
        unverified: UnverifiedRegister? = nil,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        if accessibilityLayout {
            let disclosure = DisclosureGroup(isExpanded: isExpanded) {
                VStack(alignment: .leading, spacing: 8) {
                    foldedUnverifiedSentence(unverified)
                    content()
                }
                .padding(.top, 8)
            } label: {
                sectionTitle(title, unverified: unverified, isDisclosureLabel: true)
            }
            if unverified?.frame == .spaced {
                disclosure.frame(maxWidth: .infinity, alignment: .leading)
            } else {
                disclosure
                    .padding(12)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .freesideCard(dashed: unverified != nil)
            }
        } else {
            cardSection(title, unverified: unverified) {
                content()
            }
        }
    }

    private func inspectorSection<Content: View>(
        _ title: String,
        isExpanded: Binding<Bool>,
        unverified: UnverifiedRegister? = nil,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        DisclosureGroup(isExpanded: isExpanded) {
            VStack(alignment: .leading, spacing: 8) {
                foldedUnverifiedSentence(unverified)
                content()
            }
            .padding(.top, 8)
        } label: {
            sectionTitle(title, unverified: unverified, isDisclosureLabel: true)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .freesideCard(dashed: unverified != nil)
    }

    private func factRow(_ label: String, value: String) -> some View {
        FactRow(label: label, value: value)
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
        @State private var isHoveringDigest = false

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

        @ViewBuilder private var digestCaption: some View {
            let caption = HStack(spacing: 8) {
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
                    .opacity(isHoveringDigest ? 1 : 0)
                }
            }
            if rendersInteractiveControls {
                caption
                    .contentShape(Rectangle())
                    .onHover { isHoveringDigest = $0 }
                    .contextMenu {
                        Button("Copy digest", action: copyDigest)
                    }
            } else {
                caption
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
            VStack(spacing: 0) {
                FreesideSheetHeader(title: label)
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
                FreesideSheetActionRow.done { dismiss() }
            }
            .background(Color.ground2)
            .freesideSheetPresentation()
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

    struct NonImageAttachmentSheet: View {
        let label: String
        let preview: NonImagePreview
        /// The screenshot composition lays the text out in place of the
        /// scroll view, whose height ImageRenderer cannot settle.
        var rendersScrollableContent = true
        @Environment(\.dismiss) private var dismiss

        var body: some View {
            VStack(spacing: 0) {
                FreesideSheetHeader(title: label)
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
                            if rendersScrollableContent {
                                ScrollView {
                                    previewText(text)
                                }
                            } else {
                                previewText(text)
                            }
                        }
                    } else {
                        UnavailableStateView(
                            title: "Preview unavailable",
                            systemImage: "doc",
                            description: "This \(byteCount(preview.byteCount)) attachment is not text.")
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
                FreesideSheetActionRow.done { dismiss() }
            }
            .background(Color.ground2)
            .freesideSheetPresentation()
            #if os(macOS)
                .frame(
                    minWidth: 480, idealWidth: 720,
                    minHeight: 360, idealHeight: 600)
            #endif
        }

        private func previewText(_ text: String) -> some View {
            Text(text)
                .font(.system(.body, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding()
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

            // The recommendation block states what accepting covers. An item
            // whose recommendation did not revalidate has no block, and
            // offers the same action here, so the sentence comes with it.
            if (ranking.principal + ranking.overflow).contains(.accept_recommended_route),
                let scope = findingAcceptanceScope(.accept_recommended_route, item: item)
            {
                Text(scope)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
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
        let ranking = actionRanking(item)
        if let reviewing = ranking.reviewing {
            actionButton(
                reviewing,
                item: item,
                tone: DecisionCardComposition.reviewingActionIsFilled(ranking)
                    ? .primary : .secondary)
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
                        actionLabel(action, item: item)
                    }
                }
                if !ordinary.isEmpty, !consequential.isEmpty {
                    Divider()
                }
                ForEach(Array(consequential.enumerated()), id: \.offset) { _, action in
                    Button(role: .destructive) {
                        trigger(action, item: item)
                    } label: {
                        actionLabel(action, item: item)
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
                actionLabel(action, item: item, showsIcon: showsIcon)
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
        item: Components.Schemas.AttentionItem,
        showsIcon: Bool = true
    ) -> some View {
        if showsIcon, let systemImage = AttentionDisplay.systemImage(action) {
            Label(AttentionDisplay.label(action, for: item), systemImage: systemImage)
        } else {
            Text(AttentionDisplay.label(action, for: item))
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
        case .inspect_trust_failure:
            detailsExpanded.wrappedValue = true
            actionDetailsRevealRequest = .init(itemID: itemID, nonce: UUID())
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
        case .approve_with_changes:
            proposalEditor = .effectRevision
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
func screenshotMetricBase(
    _ value: CGFloat,
    relativeTo style: Font.TextStyle
) -> CGFloat {
    #if os(macOS)
        guard FreesideFont.screenshotDynamicTypeSize != nil else { return value }
        let defaultSize = FreesideFont.$screenshotDynamicTypeSize.withValue(.large) {
            FreesideFont.size(of: style)
        }
        return value * FreesideFont.size(of: style) / defaultSize
    #else
        return value
    #endif
}

struct TaskProposalRevisionSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var expectedCostText: String
    @State private var componentCount: Int
    @State private var touchesControlPlane: Bool
    private let originalFacts: Components.Schemas.TaskProposalFactsSnapshot
    let submit: (Components.Schemas.TaskProposalRevisionInput) -> Void

    init(
        facts: Components.Schemas.TaskProposalFactsSnapshot,
        submit: @escaping (Components.Schemas.TaskProposalRevisionInput) -> Void
    ) {
        _expectedCostText = State(initialValue: String(facts.expected_cost_units))
        _componentCount = State(initialValue: facts.scope.component_count)
        _touchesControlPlane = State(initialValue: facts.scope.touches_control_plane)
        originalFacts = facts
        self.submit = submit
    }

    private var revision: Components.Schemas.TaskProposalRevisionInput? {
        guard let expectedCost = DecisionDetailView.parseExpectedCost(expectedCostText) else {
            return nil
        }
        return DecisionDetailView.taskProposalRevision(
            from: originalFacts,
            expectedCost: expectedCost,
            componentCount: componentCount,
            touchesControlPlane: touchesControlPlane)
    }

    var body: some View {
        VStack(spacing: 0) {
            FreesideSheetHeader(title: "Start with changes")
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
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 280)
    }

    /// The project-owned revision composition without Form and TextField,
    /// whose AppKit-backed controls ImageRenderer cannot draw off-screen.
    func screenshotContent() -> some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Start with changes")
                .font(FreesideFont.sectionTitle)
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
        // The sheet's own ground, so the dusk composition reads dusk ink on
        // dusk ground rather than on the harness's light canvas.
        .background(Color.ground)
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

/// The single-control edit sheet for a source-issue-closure proposal: one
/// toggle for whether the PR closes the issue. The daemon owns the target,
/// provenance, and origin, so nothing else is editable here (issue #1443).
struct EffectProposalRevisionSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var resolves: Bool
    private let originalFacts: Components.Schemas.EffectProposalFactsSnapshot
    let submit: (Components.Schemas.EffectProposalRevisionInput) -> Void

    init(
        facts: Components.Schemas.EffectProposalFactsSnapshot,
        submit: @escaping (Components.Schemas.EffectProposalRevisionInput) -> Void
    ) {
        _resolves = State(initialValue: facts.source_issue_closure?.value1.resolves ?? false)
        originalFacts = facts
        self.submit = submit
    }

    private var toggleTitle: String {
        guard let closure = originalFacts.source_issue_closure?.value1 else {
            return "Close the issue when this PR merges"
        }
        return "Close #\(closure.target.issue_number) when this PR merges"
    }

    private var revision: Components.Schemas.EffectProposalRevisionInput? {
        DecisionDetailView.effectProposalRevision(from: originalFacts, resolves: resolves)
    }

    var body: some View {
        VStack(spacing: 0) {
            FreesideSheetHeader(title: "Approve with changes")
            Form {
                Toggle(toggleTitle, isOn: $resolves)
                    .listRowBackground(Color.ground2)
            }
            .formStyle(.grouped)
            .font(FreesideFont.body)
            .foregroundStyle(Color.ink)
            .tint(.accentText)
            .scrollContentBackground(.hidden)

            // The submit is a body control in the primary recipe, not a
            // toolbar item; Return and Escape still reach it. It stays
            // disabled until `resolves` differs, because the daemon rejects an
            // unchanged revision.
            FreesideSheetActionRow(
                submitLabel: "Approve",
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
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 220)
    }

    /// The project-owned composition without the AppKit-backed Toggle, which
    /// ImageRenderer cannot draw off-screen on macOS; the state renders as
    /// text instead.
    func screenshotContent() -> some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Approve with changes")
                .font(FreesideFont.sectionTitle)
            VStack(alignment: .leading, spacing: 8) {
                KeywordLabel(text: "On merge")
                Text(resolves ? "Closes the issue" : "Doesn't close the issue")
                Divider()
                Text(toggleTitle)
            }
            .padding(14)
            .freesideCard()
            Text("Approving with changes flips only whether the pull request closes the issue.")
                .font(FreesideFont.caption)
                .foregroundStyle(Color.inkDim)
            FreesideSheetActionRow(
                submitLabel: "Approve",
                isSubmitEnabled: revision != nil,
                submit: {}, cancel: {})
        }
        .padding(24)
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
        // The sheet's own ground, so the dusk composition reads dusk ink on
        // dusk ground rather than on the harness's light canvas.
        .background(Color.ground)
    }
}

struct TaskProposalSnoozeSheet: View {
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
        VStack(spacing: 0) {
            FreesideSheetHeader(title: "Snooze proposal")
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
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 220)
    }

    /// The project-owned snooze composition without Form and DatePicker,
    /// whose AppKit-backed controls ImageRenderer cannot draw off-screen.
    func screenshotContent() -> some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Snooze proposal")
                .font(FreesideFont.sectionTitle)
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
        // The sheet's own ground, so the dusk composition reads dusk ink on
        // dusk ground rather than on the harness's light canvas.
        .background(Color.ground)
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
