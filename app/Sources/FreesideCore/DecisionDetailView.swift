import Foundation
import FreesideAPI
import SwiftUI

private typealias CardScale = DecisionCardComposition.Scale

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

/// Whether a launch may present a composer: exactly when a click on that
/// action's button could, so `-FreesideComposer` never opens a sheet the
/// operator couldn't reach.
enum DecisionLaunchComposerGate {
    static func canPresent(
        _ action: Components.Schemas.Action,
        requested: [Components.Schemas.Action],
        unavailable: [Components.Schemas.Action],
        actionsEnabled: Bool,
        isSubmittable: Bool
    ) -> Bool {
        requested.contains(action) && !unavailable.contains(action)
            && actionsEnabled && isSubmittable
    }
}

/// One item's self-contained decision card: header, reason, evidence,
/// labeled agent claims, the bindings the decision will commit against,
/// and exactly the item's requested actions. Actions stay disabled until
/// the model's revalidation of current state succeeds.
struct DecisionDetailView: View {
    private enum ScrollTarget: Hashable {
        case technicalDetails
        case evidence
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
    @Environment(\.openURL) private var openURL
    @State private var model: DecisionModel
    @State private var proposalEditor: ProposalEditor?
    @State private var messageEditor: MessageEditor?
    @State private var specApprovalReader: SpecApprovalReader?
    @State private var pendingConfirmation: PendingConfirmation?
    @State private var capabilityRetrySnapshot: Components.Schemas.AttentionItemSnapshot?
    @State private var actionDetailsRevealRequest: TechnicalDetailsRevealRequest?
    @State private var sectionPreferences: DecisionSectionPreferences
    @State private var inspectorPresented: Bool
    /// Set by the card's Evidence pointer and cleared by the inspector once
    /// it has scrolled to its Evidence section.
    @State private var revealsInspectorEvidence = false
    @State private var recommendationVisible = true
    @State private var provenanceExpanded = false
    @State private var lostResponseExpanded = false
    @State private var alternativeSelections: [String: Components.Schemas.AdjudicationRoute]
    /// The finding rows the operator opened, by finding id. Empty by default:
    /// a finding_adjudication card leads with collapsed rows so its actions
    /// stay in the first viewport (#1107).
    @State private var expandedFindings: Set<String>
    @State private var expandedSummaryReports: Set<DecisionSummaryIdentity> = []
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
    private let launchComposer: LaunchInputs.Composer?
    private let onConsumeLaunchComposer: () -> Void
    private let externalInspectorPresented: Binding<Bool>?
    /// The standing notices this detail draws over its card on the Mac,
    /// where it holds them in place of the detail column so its inspector
    /// stands beside them. Nil where they draw in the window's slot.
    private let standingNotices: StandingNotices?
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
        launchComposer: LaunchInputs.Composer? = nil,
        onConsumeLaunchComposer: @escaping () -> Void = {},
        graphics: DecisionGraphicPresentations = .init(),
        loadsAttachments: Bool = true,
        showsValidationProgress: Bool = true,
        now: Date = .now,
        sectionPreferences: DecisionSectionPreferences? = nil,
        inspectorPresented: Binding<Bool>? = nil,
        standingNotices: StandingNotices? = nil,
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
        self.launchComposer = launchComposer
        self.onConsumeLaunchComposer = onConsumeLaunchComposer
        externalInspectorPresented = inspectorPresented
        self.standingNotices = standingNotices
        self.onSelectItem = onSelectItem
        self.graphics = graphics
        self.loadsAttachments = loadsAttachments
        self.showsValidationProgress = showsValidationProgress
        self.now = now
    }

    var body: some View {
        platformBody { topMargin in
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
                                inspectorPresented: inspectorBinding.wrappedValue
                            )
                            .detailCard(
                                compact: horizontalSizeClass == .compact, topMargin: topMargin)
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
                    UnavailableStateView(
                        glyph: .inbox, title: "Item unavailable",
                        description: "This attention item is not in the inbox.",
                        seat: .detailColumn)
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
            // Not from `init`: the sheet would open before validation, a
            // state a click can't reach.
            .onChange(of: canPresentLaunchComposer, initial: true) {
                presentLaunchComposerIfReady()
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
                        eyebrow: AttentionDisplay.label(.discuss),
                        ask: "What do you want to ask the agent?",
                        consequence: "The item stays open while the agent replies.",
                        submitLabel: "Send",
                        thread: model.conversation.map {
                            .init(
                                snapshot: $0, attachments: attachments,
                                loadsAttachments: loadsAttachments, now: now)
                        }
                    ) { message, _ in
                        await model.submitDiscuss(message: message)
                    }
                case .requestChanges:
                    MessageComposerSheet(
                        eyebrow: AttentionDisplay.label(.request_changes),
                        ask: "What should the specification change?",
                        submitLabel: AttentionDisplay.label(.request_changes),
                        byteLimit: 8192
                    ) { message, _ in
                        await model.submitRequestChanges(message: message)
                    }
                case .answerAndRetry:
                    MessageComposerSheet(
                        eyebrow: AttentionDisplay.label(.answer_and_retry),
                        ask: "What is your answer?",
                        submitLabel: AttentionDisplay.label(.answer_and_retry), byteLimit: 8192,
                        routeOptions: AgentQuestionPresentation.answerRoutes(for: model.snapshot?.item)
                    ) { message, route in
                        await model.submitAnswer(
                            .answer_and_retry, message: message,
                            answerRoute: route
                                ?? AgentQuestionPresentation.answerRoute(for: model.snapshot?.item))
                    }
                case .answerWithoutRetry:
                    MessageComposerSheet(
                        eyebrow: AttentionDisplay.label(.answer_without_retry),
                        ask: "What is your answer?",
                        consequence: "The question concludes without restarting work.",
                        submitLabel: "Record Answer", byteLimit: 8192
                    ) { message, _ in
                        await model.submitAnswer(.answer_without_retry, message: message)
                    }
                case .returnToAgent:
                    MessageComposerSheet(
                        eyebrow: AttentionDisplay.label(.return_to_agent),
                        ask: "What should the agent change?",
                        consequence: "The work returns for review after the agent changes it.",
                        submitLabel: AttentionDisplay.label(.return_to_agent), byteLimit: 8192
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
        }
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

        /// The card's Evidence pointer is navigation between two panes of
        /// one card (R3, R18): it opens the section and scrolls to it, and
        /// sends and records nothing.
        private func showEvidenceInInspector() {
            // The reader and the sections take turns in the inspector, so
            // the sections come back first.
            specApprovalReader = nil
            evidenceExpanded.wrappedValue = true
            revealsInspectorEvidence = true
        }

        private func revealEvidenceInInspectorIfRequested(using scrollProxy: ScrollViewProxy) {
            guard revealsInspectorEvidence else { return }
            revealsInspectorEvidence = false
            withAnimation {
                scrollProxy.scrollTo(ScrollTarget.evidence, anchor: .top)
            }
        }
    #endif

    /// Hosts the detail's content, which takes the top margin its card
    /// draws under: a module gap below a standing notice, nil for the
    /// pane's own.
    @ViewBuilder
    private func platformBody<Content: View>(
        @ViewBuilder _ content: @escaping (CGFloat?) -> Content
    ) -> some View {
        #if os(iOS)
            content(nil).safeAreaInset(edge: .bottom, spacing: 0) {
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
            // The notices stack over the card inside the view the inspector
            // attaches to. An inspector spans only the view it modifies, so
            // notices stacked above this detail left a strip of the pane's
            // ground over the inspector; here it is a column beside both for
            // the pane's whole height, and narrows a notice with its card.
            StandingDetailColumn(notices: standingNotices, content: content)
                .inspector(isPresented: inspectorBinding) {
                    if let item = model.snapshot?.item {
                        Group {
                            if let specApprovalReader {
                                VStack(spacing: 0) {
                                    specApprovalReaderHeaderRow(specApprovalReader, item: item) {
                                        self.specApprovalReader = nil
                                    }
                                    SpecApprovalReaderViewport {
                                        specApprovalReaderContent(specApprovalReader, item: item)
                                    }
                                }
                            } else {
                                ScrollViewReader { scrollProxy in
                                    ScrollView {
                                        inspectorContent(item)
                                            .padding(.vertical, InspectorScale.verticalPadding)
                                            .padding(.horizontal, InspectorScale.horizontalPadding)
                                    }
                                    .onChange(of: detailsRevealRequest) {
                                        revealTechnicalDetailsInInspectorIfRequested(using: scrollProxy)
                                    }
                                    .onChange(of: revealsInspectorEvidence) {
                                        revealEvidenceInInspectorIfRequested(using: scrollProxy)
                                    }
                                    .onAppear {
                                        revealTechnicalDetailsInInspectorIfRequested(using: scrollProxy)
                                        revealEvidenceInInspectorIfRequested(using: scrollProxy)
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
                            glyph: .inbox, title: "No decision selected",
                            description: "Select an item to inspect its facts.")
                    }
                }
        #endif
    }

    private var isAccessibilityLayout: Bool {
        dynamicTypeSize >= .accessibility1
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
        inspectorPresented: Bool = false,
        actionRegionFrameChanged: ((CGRect) -> Void)? = nil
    ) -> some View {
        let composition = DecisionCardComposition.forType(item._type)
        let register = unverified(
            item,
            effectProposalFacts: effectProposalFacts,
            accessibilityLayout: accessibilityLayout,
            rendersInteractiveControls: rendersInteractiveControls)
        let modules = CardModules(
            item: item,
            composition: composition,
            proposalFacts: proposalFacts,
            effectProposalFacts: effectProposalFacts,
            register: register,
            accessibilityLayout: accessibilityLayout,
            inspectorPresented: inspectorPresented)
        VStack(alignment: .leading, spacing: CardScale.sectionGap) {
            VStack(alignment: .leading, spacing: CardScale.headGap) {
                eyebrow(item, register: register, accessibilityLayout: accessibilityLayout)
                banner()
                if DecisionCardComposition.rendersAsk(for: item) {
                    Text(AttentionDisplay.ask(item))
                        .font(FreesideFont.ask)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            // The ask and the reason are one question and its answer, so
            // nothing renders between them (R0). A type whose reason is the
            // agent's summary shows it once, under the unverified claim
            // label, and draws no reason here (#1098). A decision-first type
            // folds it: see `reasonPlacement(for:)`.
            let reasonPlacement =
                composition.drawsReason(for: item)
                ? DecisionCardComposition.reasonPlacement(for: item) : nil
            if reasonPlacement == .underAsk {
                reasonUnderAsk(item, register: register.at(.reason))
            } else if let lead = DecisionCardComposition.reasonLead(for: item) {
                Text(lead)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            // A stale final review says what moved before anything else on
            // the card (frame 7.3). The notice carries no action: the
            // actions stay in the control group where every card keeps them.
            if let stale = DecisionCardComposition.staleNotice(for: item) {
                Notice(tone: .wax, keyword: "Stale", sentence: stale)
            }

            if let conversation = model.conversation,
                !DecisionCardComposition.placesConversationWithSpecification(for: item._type)
            {
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

            let stackedLayout = accessibilityLayout || compactLayout
            let foldsReason = reasonPlacement == .recordedContext
            cardColumn(
                modules,
                stackedLayout: stackedLayout,
                foldsReason: foldsReason,
                actionRegionFrameChanged: actionRegionFrameChanged)
        }
        // The card's own space, so a measurement reads from the card's top
        // edge rather than the scroll view's (#1107).
        .coordinateSpace(name: Self.cardCoordinateSpace)
    }

    /// What every module of one card draws from.
    private struct CardModules {
        let item: Components.Schemas.AttentionItem
        let composition: DecisionCardComposition
        let proposalFacts: Components.Schemas.TaskProposalFactsSnapshot?
        let effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot?
        let register: UnverifiedRegister
        let accessibilityLayout: Bool
        let inspectorPresented: Bool
    }

    private func cardModules(_ range: Range<Int>, _ modules: CardModules) -> some View {
        ForEach(Array(modules.composition.modules.enumerated())[range], id: \.offset) {
            index, module in
            cardModule(
                module,
                moduleIndex: index,
                item: modules.item,
                composition: modules.composition,
                proposalFacts: modules.proposalFacts,
                effectProposalFacts: modules.effectProposalFacts,
                register: modules.register.at(.module(index)),
                accessibilityLayout: modules.accessibilityLayout,
                inspectorPresented: modules.inspectorPresented)
        }
    }

    /// The card's one column, at any pane width: the modules a decision
    /// rests on, the control group, the folds, and then the supporting
    /// modules.
    @ViewBuilder
    private func cardColumn(
        _ modules: CardModules,
        stackedLayout: Bool,
        foldsReason: Bool,
        actionRegionFrameChanged: ((CGRect) -> Void)?
    ) -> some View {
        cardModules(modules.composition.leadModules, modules)
        controlGroup(
            modules,
            stackedLayout: stackedLayout,
            actionRegionFrameChanged: actionRegionFrameChanged)
        folds(modules, foldsReason: foldsReason)
        cardModules(modules.composition.supportingModules, modules)
    }

    /// The reviewing action and the action region are one control group
    /// (R10), whatever a composition draws between them. A read-only item
    /// requests no decision, so its card draws no group at all (survey frame
    /// 5.5) rather than an empty one that still takes a section's gap.
    @ViewBuilder
    private func controlGroup(
        _ modules: CardModules,
        stackedLayout: Bool,
        actionRegionFrameChanged: ((CGRect) -> Void)?
    ) -> some View {
        let item = modules.item
        let composition = modules.composition
        let hasReviewingAction = composition.reviewingActionInsertionIndex != nil
        // The reviewing action leads its group as the card's forward step.
        // On a stale review it is not one, so it follows the action that
        // recovers (frame 7.3) inside the action region.
        let reviewingLeads = !DecisionCardComposition.isStale(item)
        if Self.drawsControlGroup(item) {
            VStack(alignment: .leading, spacing: CardScale.controlGap) {
                if offersLostResponseRetry {
                    lostResponseRetry
                }
                if hasReviewingAction {
                    if reviewingLeads {
                        reviewingAction(item)
                    }
                    cardModules(composition.controlGroupModules, modules)
                }
                #if os(macOS)
                    actionRegion(
                        item,
                        stackedLayout: stackedLayout,
                        includesReviewing: !hasReviewingAction || !reviewingLeads,
                        register: modules.register.at(.actionRegion)
                    )
                    .onGeometryChange(for: CGRect.self) { geometry in
                        geometry.frame(in: .named(Self.cardCoordinateSpace))
                    } action: { frame in
                        actionRegionFrameChanged?(frame)
                    }
                #else
                    actions(
                        item,
                        stackedLayout: stackedLayout,
                        includesReviewing: !hasReviewingAction || !reviewingLeads)
                #endif
            }
        }
    }

    /// The card's one hairline (R26) and the folds under it: the folded
    /// modules, then Recorded Context. A card with nothing folded draws no
    /// hairline.
    @ViewBuilder
    private func folds(_ modules: CardModules, foldsReason: Bool) -> some View {
        let foldsReason = foldsReason && DecisionCardComposition.reason(for: modules.item) != nil
        if foldsReason || drawsFoldedModule(modules.item, modules.composition) {
            VStack(alignment: .leading, spacing: CardScale.foldGap) {
                cardModules(modules.composition.foldedModules, modules)
                if foldsReason {
                    recordedContext(modules.item, register: modules.register)
                }
            }
            .padding(.top, CardScale.foldLead)
            .overlay(alignment: .top) {
                Color.rule.frame(height: 1)
            }
        }
    }

    /// Whether the card has a control group to draw: an action
    /// the item requests, or the agent claims the Mac sets beside them.
    static func drawsControlGroup(_ item: Components.Schemas.AttentionItem) -> Bool {
        !item.requested_decision.isEmpty
            || (!DecisionCardComposition.forType(item._type).drawsLeadClaimsInCard
                && !DecisionCardComposition.actionRegionClaims(item.agent_claims).isEmpty)
    }

    /// Whether any folded module has something to draw, which is what
    /// decides if the folds' hairline has a fold under it.
    private func drawsFoldedModule(
        _ item: Components.Schemas.AttentionItem, _ composition: DecisionCardComposition
    ) -> Bool {
        composition.foldedModules.contains { foldDraws(composition.modules[$0], item) }
    }

    private func foldDraws(
        _ module: DecisionCardModule, _ item: Components.Schemas.AttentionItem
    ) -> Bool {
        switch module {
        case .yieldChart:
            (graphics.diminishingYield ?? DecisionYieldPresentation(item)) != nil
        case .foldedFacts:
            !DecisionFactPlacement(item, includesCommitPlan: false, now: now).folded.isEmpty
        case .facts, .agentQuestion, .specRevision, .specification, .recommendation, .checklist,
            .stageRail, .comparison, .stopCause, .findingFacts, .factBlock, .summary, .claims,
            .evidence, .details:
            true
        }
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
            register: UnverifiedRegister
        ) -> some View {
            VStack(alignment: .leading, spacing: CardScale.sectionGap) {
                let recommendation = drawnRecommendation(item)
                if let recommendation {
                    recommendationBlock(recommendation, item: item, register: register)
                }
                let actionClaims = DecisionCardComposition.actionRegionClaims(item.agent_claims)
                // A card that leads with its claim draws it in the card, so
                // a copy here would print the claim twice.
                if !actionClaims.isEmpty,
                    !DecisionCardComposition.forType(item._type).drawsLeadClaimsInCard
                {
                    // The recommendation's label comes first in this region,
                    // so it carries the control when it is unverified too.
                    let claimsRegister =
                        recommendation?.register.isUnverifiedClaim == true
                        ? register.withoutInfo : register
                    cardSection("Agent claims", unverified: claimsRegister) {
                        claimRows(actionClaims, unverified: claimsRegister)
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
        register: UnverifiedRegister,
        accessibilityLayout: Bool,
        inspectorPresented: Bool
    ) -> some View {
        let rendersInteractiveControls = register.rendersInteractiveControls
        switch module {
        case .foldedFacts:
            foldedFacts(
                DecisionFactPlacement(item, includesCommitPlan: false, now: now), summarized: true)
        case .facts:
            factsSection(
                item,
                includesCommitPlan: !composition.modules.contains(.checklist),
                drawsFold: !composition.modules.contains(.foldedFacts))
            if let proposalFacts {
                keywordSection("Facts") {
                    proposalRows(proposalFacts)
                }
                if let prior = proposalFacts.supersedes?.value1 {
                    keywordSection("Revision Context") {
                        proposalRevisionRows(prior)
                    }
                }
            }
            // A closure states what approving binds as the card's statement
            // (frame 7.9), ahead of the facts that qualify it.
            if let effectProposalFacts,
                let binding = AttentionDisplay.effectBinding(effectProposalFacts)
            {
                Text(
                    "\(binding.lead)\(Text(binding.head).font(FreesideFont.monoValue))\(binding.outcome)"
                )
                .font(FreesideFont.statement)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
                .accessibilityLabel(Text(binding.plain))
            }
            // effectProposalCardRows is empty for an effect kind this card
            // has no rows for (it draws a closure and a follow-up filing),
            // so the titled section is suppressed rather than drawn empty.
            if let effectProposalFacts,
                case let effectRows = AttentionDisplay.effectProposalCardRows(effectProposalFacts),
                !effectRows.isEmpty
            {
                keywordSection("Facts") {
                    ForEach(effectRows) { fact in
                        factRow(fact)
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
                proposedIssueTextSection("Proposed title", text: proposed.title, unverified: register)
                proposedIssueTextSection(
                    "Proposed body", text: proposed.body, unverified: register.withoutInfo)
            }
        case .agentQuestion:
            agentQuestionLead(item, register: register)
        case .specRevision:
            specRevisionLead(item)
        case .specification:
            specificationMaterial(
                item,
                rendersInteractiveControls: rendersInteractiveControls)
            specificationConversation(
                item, rendersInteractiveControls: rendersInteractiveControls)
        case .recommendation:
            #if os(iOS)
                if let recommendation = drawnRecommendation(item) {
                    recommendationBlock(recommendation, item: item, register: register)
                }
            #endif
        case .checklist:
            if let presentation = DecisionChecklistPresentation(item) {
                DecisionChecklistModuleView(presentation: presentation)
            }
        case .stageRail:
            if let presentation = graphics.stageRail {
                keywordSection("Stages") {
                    StageRail(
                        title: nil,
                        presentation: presentation.reachedNewestFirst,
                        axis: .vertical,
                        showsSummaryText: false)
                }
            }
        case .comparison:
            if let presentation = graphics.comparison {
                DecisionComparisonModuleView(
                    presentation: presentation,
                    carriesInfo: register.carriesInfo,
                    rendersInteractiveControls: rendersInteractiveControls)
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
                DecisionStopCauseModuleView(
                    presentation: presentation,
                    carriesInfo: register.carriesInfo,
                    rendersInteractiveControls: rendersInteractiveControls)
            }
        case .findingFacts:
            // The finding cards lead the §9 finding_adjudication card
            // (docs/plan.md §9 revision 78), so this module renders ahead of
            // actionInsertionIndex on every layout; see
            // DecisionCardComposition.forType(.finding_adjudication).
            if let adjudication = item.finding_adjudication?.value1 {
                findingCards(adjudication, register: register)
            }
        case .factBlock:
            factBlocks(item, register: register)
        case .summary:
            agentSummary(item, register: register)
        case .claims:
            if composition.drawsClaimsInCard(
                at: moduleIndex, on: DecisionCardComposition.UnverifiedContext.currentPlatform)
            {
                claims(
                    composition.claims(
                        from: item.agent_claims,
                        at: moduleIndex,
                        prominentClaimIndex: graphics.prominentClaimIndex),
                    title: composition.claimsAreProminent(at: moduleIndex)
                        ? DecisionCardComposition.leadClaimsKeyword(for: item._type) : "Agent Claims",
                    accessibilityLayout: accessibilityLayout,
                    prominent: composition.claimsAreProminent(at: moduleIndex),
                    unverified: register)
            }
        case .evidence:
            #if os(macOS)
                // The open inspector holds the attachments, so the card
                // points at them rather than drawing them twice (R18). The
                // pointer's link opens the inspector's Evidence section, so
                // it no longer waits for that section to be open (#1107).
                switch composition.macEvidence(
                    inspectorPresented: inspectorPresented,
                    attachmentCount: item.evidence_snapshot.count)
                {
                case .pointer:
                    evidencePointer(
                        count: item.evidence_snapshot.count,
                        rendersInteractiveControls: rendersInteractiveControls)
                case .rows:
                    evidence(
                        item,
                        accessibilityLayout: accessibilityLayout,
                        rendersInteractiveControls: rendersInteractiveControls)
                case .nothing:
                    EmptyView()
                }
            #else
                evidence(
                    item,
                    accessibilityLayout: accessibilityLayout,
                    rendersInteractiveControls: rendersInteractiveControls)
            #endif
        case .details:
            #if os(iOS)
                details(item, accessibilityLayout: accessibilityLayout, register: register)
            #endif
        }
    }

    /// One field of the issue a follow-up filing would create, in full and
    /// as plain text: the operator approves the exact text that would be
    /// sent, so nothing is truncated and Markdown is not interpreted.
    private func proposedIssueTextSection(
        _ title: String,
        text: String,
        unverified: UnverifiedRegister
    ) -> some View {
        // The text that would be published keeps the dashed frame on every
        // card: it is set apart as an exhibit, not quoted as an account.
        cardSection(title: sectionTitle(title, unverified: unverified), dashed: true) {
            Text(AttentionDisplay.screenedIssueTextExplanation)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            Text(verbatim: text)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// The agent's summary on every card that carries one (#1378, #1461):
    /// a bounded lead in the agent's own voice, the concerns its report
    /// marks, and the whole report one disclosure away in place (R3). The
    /// final review states a missing inline summary; another card without
    /// one has no summary section.
    @ViewBuilder
    private func agentSummary(
        _ item: Components.Schemas.AttentionItem, register: UnverifiedRegister
    ) -> some View {
        let claims = DecisionCardComposition.forType(item._type).summaries(from: item.agent_claims)
        let isFinalReview = item._type == .ready_for_final_review
        if isFinalReview || !claims.isEmpty {
            VStack(alignment: .leading, spacing: CardScale.controlGap) {
                sectionTitle("Agent summary", unverified: register)
                if claims.isEmpty {
                    Text("Inline summary unavailable. Any retained report is listed with the claim attachments.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                }
                ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
                    if let text = claim.text {
                        let presentation = DecisionSummaryPresentation(text)
                        if presentation.isExcerpt {
                            Text("Report excerpt (incomplete)")
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.ink)
                        }
                        QuoteBlock {
                            VStack(alignment: .leading, spacing: CardScale.moduleGap) {
                                DecisionSummaryText(blocks: presentation.leadBlocks)
                                if presentation.isExcerpt {
                                    Text("…")
                                        .font(FreesideFont.summary())
                                        .accessibilityLabel("Excerpt ends here")
                                }
                                if let concerns = presentation.concernBlocks {
                                    KeywordLabel(text: "Remaining concerns")
                                    DecisionSummaryText(blocks: concerns)
                                }
                            }
                        }
                        if presentation.concernsUnknown {
                            Text(
                                "Concerns have not been extracted; read the full report. Concerns may be outside this excerpt."
                            )
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                            .fixedSize(horizontal: false, vertical: true)
                        }
                        SentenceDisclosure(
                            label: isFinalReview ? "Full Report" : "Source and Original Report",
                            summary: Self.fullReportSummary(claim, presentation: presentation),
                            isExpanded: summaryReportExpanded(
                                DecisionSummaryIdentity(itemID: item.id, claim: claim))
                        ) {
                            fullSummaryReport(
                                claim, rendersInteractiveControls: register.rendersInteractiveControls
                            )
                            .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func summaryReportExpanded(_ identity: DecisionSummaryIdentity) -> Binding<Bool> {
        Binding(
            get: { expandedSummaryReports.contains(identity) || expandsSummaryReports },
            set: {
                if $0 {
                    expandedSummaryReports.insert(identity)
                } else {
                    expandedSummaryReports.remove(identity)
                }
            })
    }

    /// What the closed report says about itself: who wrote it, and how many
    /// concerns it lists when the report counts them itself.
    static func fullReportSummary(
        _ claim: Components.Schemas.AgentClaim, presentation: DecisionSummaryPresentation
    ) -> String {
        let concerns = presentation.concernCount.map { $0 == 1 ? "1 concern" : "\($0) concerns" }
        return [producerInvocationID(claim), concerns].compactMap { $0 }.joined(separator: " · ")
    }

    private func fullSummaryReport(
        _ claim: Components.Schemas.AgentClaim, rendersInteractiveControls: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            // A fixed face: the platform caption draws under the 11.5pt
            // floor on macOS.
            Text("Source: agent invocation `\(Self.producerInvocationID(claim))`")
                .font(FreesideFont.cardBody).textSelection(.enabled)
            AttachmentRow(
                label: "Original report", digest: claim.digest, metadata: claim.metadata, attachments: attachments,
                loadsAttachments: false, text: claim.text, drawsTextAsSummary: true,
                rendersInteractiveControls: rendersInteractiveControls)
        }
    }

    private func claimText(
        _ content: String, mediaType: Components.Schemas.ClaimText.media_typePayload
    ) -> some View {
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

    /// The reason as the ask's own second line (R0). An agent-written
    /// reason is quoted under its producer label (plan §9, Summary
    /// Provenance).
    @ViewBuilder
    private func reasonUnderAsk(
        _ item: Components.Schemas.AttentionItem, register: UnverifiedRegister
    ) -> some View {
        if let reason = DecisionCardComposition.reason(for: item) {
            switch DecisionCardComposition.reasonFace(for: item._type) {
            case .secondLine:
                reasonText(reason, color: .inkDim, register: register)
            case .statement:
                Text(reason.text)
                    .font(FreesideFont.statement)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// The reason one disclosure away, below the actions, on a card whose
    /// lead already says what the operator is deciding.
    @ViewBuilder
    private func recordedContext(
        _ item: Components.Schemas.AttentionItem, register: UnverifiedRegister
    ) -> some View {
        if let reason = DecisionCardComposition.reason(for: item) {
            SentenceDisclosure(
                label: "Recorded Context", isExpanded: disclosure(.recordedContext)
            ) {
                reasonText(reason, color: .ink, register: register.withoutInfo)
                    .padding(.top, 8)
            }
        }
    }

    /// The reason in Details, which carry it in full on every type whether
    /// or not the card draws it anywhere else (plan §9 revision 82).
    @ViewBuilder
    private func detailsReason(
        _ item: Components.Schemas.AttentionItem, register: UnverifiedRegister
    ) -> some View {
        if let reason = DecisionCardComposition.reason(for: item) {
            VStack(alignment: .leading, spacing: 4) {
                if !reason.isAgentWritten {
                    KeywordLabel(text: reason.label)
                }
                reasonText(
                    reason, color: .inkDim, register: register,
                    isSummary: DecisionCardComposition.reasonIsAgentSummary(item._type))
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// `isSummary` marks a reason that is the agent's Markdown summary (a
    /// specification approval's), which Details draw as the card draws the
    /// summary and in full; every other reason is a sentence as written.
    @ViewBuilder
    private func reasonText(
        _ reason: DecisionCardComposition.Reason, color: Color, register: UnverifiedRegister,
        isSummary: Bool = false
    ) -> some View {
        let text = Group {
            if isSummary {
                DecisionSummaryText(
                    blocks: DecisionSummaryPresentation.blocks(
                        .init(media_type: .text_sol_markdown, content: reason.text)),
                    color: color)
            } else {
                Text(reason.text)
                    .font(FreesideFont.callout)
                    .foregroundStyle(color)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        if reason.isAgentWritten {
            QuoteBlock(
                producer: reason.label, carriesInfo: register.carriesInfo,
                rendersInteractiveControls: register.rendersInteractiveControls
            ) { text }
        } else {
            text
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
    /// rows stay beside the decision and which fold. The fold draws here,
    /// above the actions, unless the composition places it as its own
    /// `.foldedFacts` module.
    @ViewBuilder
    private func factsSection(
        _ item: Components.Schemas.AttentionItem,
        includesCommitPlan: Bool,
        drawsFold: Bool
    ) -> some View {
        let placement = DecisionFactPlacement(
            item, includesCommitPlan: includesCommitPlan, now: now)
        // The diff draws as the Change row (R28); the rows around it keep
        // the Facts section.
        let change = item.diff_stats?.value1
        let rows = placement.visible.filter {
            change == nil || $0.label != AttentionDisplay.diffFactLabel
        }
        if let change, placement.visible.count != rows.count {
            DecisionChangeRow(diff: change)
        }
        if !rows.isEmpty {
            keywordSection(DecisionCardComposition.factsKeyword(for: item._type)) {
                ForEach(rows) { fact in
                    factRow(fact)
                }
            }
        }
        if drawsFold {
            foldedFacts(placement, summarized: false)
        }
    }

    /// The routine facts behind their disclosure. Under the actions the
    /// closed fold names what it holds (R2); above them it stays the bare
    /// label.
    @ViewBuilder
    private func foldedFacts(_ placement: DecisionFactPlacement, summarized: Bool) -> some View {
        if !placement.folded.isEmpty {
            SentenceDisclosure(
                label: DecisionFactPlacement.foldedTitle,
                summary: summarized ? placement.foldedSummary : nil,
                isExpanded: disclosure(.runDetails)
            ) {
                VStack(alignment: .leading, spacing: 8) {
                    ForEach(placement.folded) { fact in
                        factRow(fact)
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
        register: UnverifiedRegister
    ) -> some View {
        if let changeSummary = graphics.changeSummary {
            cardSection("Change summary", unverified: register) {
                Text(changeSummary.text)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(Text(changeSummary.summary))
        }

        if let comparison = graphics.comparison, !comparison.verifiableFacts.isEmpty {
            keywordSection("Daemon Facts") {
                ForEach(comparison.verifiableFacts) { fact in
                    FactRow(label: fact.label, value: fact.value)
                }
            }
        }

        if let attemptTimings = graphics.attemptTimings {
            keywordSection(attemptTimings.title) {
                ForEach(attemptTimings.facts) { fact in
                    FactRow(label: fact.label, value: fact.value)
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
        register: UnverifiedRegister
    ) -> some View {
        if let presentation = AgentQuestionPresentation(item) {
            if let scope = presentation.scopeConflict {
                // The daemon's own statement inside the agent's question
                // (R5): the accent bar, never the quote.
                SystemCallout {
                    KeywordLabel(text: "Required work outside scope")
                    Text(scope.paths.joined(separator: ", "))
                        .font(FreesideFont.statement)
                    Group {
                        Text("Allowed paths: \(scope.declared_paths.joined(separator: ", "))")
                        Text("Candidate: \(AttentionDisplay.shortRevision(scope.head_sha))")
                        Text(
                            "Answer to keep scope and record the unmet work. To widen scope, stop and start a new run with a newly approved path policy."
                        )
                    }
                    .font(FreesideFont.cardBody)
                }
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            }
            // The eyebrow names the first question's register directly
            // above it (R27), so only a later question repeats the label.
            let eyebrowLabelsLead = DecisionCardComposition.eyebrow(for: item).carriesInfo
            // With nothing drawn between them, the first question is the
            // card's ask and sits the head's gap under the eyebrow.
            let leadFollowsEyebrow =
                eyebrowLabelsLead && presentation.scopeConflict == nil && model.conversation == nil
                && !drawsRecommendationModule(item)
            ForEach(Array(presentation.decisions.enumerated()), id: \.offset) { index, decision in
                VStack(alignment: .leading, spacing: CardScale.sectionGap) {
                    VStack(alignment: .leading, spacing: CardScale.headGap) {
                        if index > 0 || !eyebrowLabelsLead {
                            sectionTitle(
                                "Agent question",
                                unverified: index == 0 ? register : register.withoutInfo)
                        }
                        VStack(alignment: .leading, spacing: 6) {
                            Text(decision.question)
                                .font(FreesideFont.ask)
                                .foregroundStyle(Color.ink)
                                .fixedSize(horizontal: false, vertical: true)
                            Text(decision.whyBlocking)
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.inkDim)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                    if !decision.options.isEmpty {
                        VStack(alignment: .leading, spacing: CardScale.moduleGap) {
                            ForEach(Array(decision.options.enumerated()), id: \.offset) { optionIndex, option in
                                agentQuestionOption(option, number: optionIndex + 1)
                            }
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.top, index == 0 && leadFollowsEyebrow ? CardScale.headGap - CardScale.sectionGap : 0)
            }
        }
    }

    /// One alternative the agent enumerated, drawn as a quote (R5) so the
    /// label and the complete tradeoff read as the agent's words. The
    /// recommended one carries the compact mark trailing its keyword (R21),
    /// with no glyph and no explanation control: the card's one control sits
    /// on the eyebrow (R25). A quote describes a choice and is not the
    /// control that makes it: the answer still goes through the card's
    /// answer actions, so it is one accessibility element with no tap target.
    private func agentQuestionOption(
        _ option: AgentQuestionPresentation.Option,
        number: Int
    ) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            if option.recommended {
                // The mark trails the keyword while the line holds both and
                // drops under it at a text size where it does not.
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        KeywordLabel(text: "Option \(number)")
                        Spacer(minLength: 0)
                        CompactMark(text: Self.recommendedOptionMark)
                    }
                    VStack(alignment: .leading, spacing: 4) {
                        KeywordLabel(text: "Option \(number)")
                        CompactMark(text: Self.recommendedOptionMark)
                    }
                }
            } else {
                KeywordLabel(text: "Option \(number)")
            }
            Text(option.label)
                .font(FreesideFont.optionLabel)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            Text(option.tradeoffs)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .quoteSurface()
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Self.agentQuestionOptionAccessibilityLabel(option, number: number))
    }

    static let recommendedOptionMark = "Agent recommends"

    /// What VoiceOver reads for one option. The compact mark prints no
    /// "(unverified)" (R21), so the spoken label keeps it, in the order the
    /// option has always been read: number, label, recommendation, tradeoffs.
    static func agentQuestionOptionAccessibilityLabel(
        _ option: AgentQuestionPresentation.Option, number: Int
    ) -> String {
        let recommendation = option.recommended ? ["\(recommendedOptionMark) (unverified)"] : []
        return (["Option \(number)", option.label] + recommendation + [option.tradeoffs])
            .joined(separator: ", ")
    }

    /// What changed since the revision the operator last read: one row,
    /// the keyword on the left and the counts on the right (frame 7.2).
    @ViewBuilder
    private func specRevisionLead(_ item: Components.Schemas.AttentionItem) -> some View {
        if let revision = item.spec_revision?.value1,
            let priorIteration = Self.priorSpecRevisionIteration(in: item)
        {
            DecisionChangeRow(specification: revision.diff, sinceRevision: priorIteration)
        }
    }

    /// The operator's comments on earlier revisions, each with the agent's
    /// addressal claim, in the order the daemon lists them.
    static func priorExchanges(
        in item: Components.Schemas.AttentionItem
    ) -> [ConversationView.PriorExchange] {
        guard let revision = item.spec_revision?.value1 else { return [] }
        return revision.prior_comments.map { comment in
            .init(
                id: comment.comment_id,
                iteration: comment.iteration,
                comment: comment.body,
                response: revision.claimed_addressals.first {
                    $0.comment_id == comment.comment_id
                }?.response)
        }
    }

    /// The thread about the specification: earlier comments with their
    /// addressal, the live conversation, and the link that replies to it.
    /// The link opens the composer `Discuss` opens, under the gate that
    /// button is under, and draws only where there is a thread to reply to.
    @ViewBuilder
    private func specificationConversation(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        let priorExchanges = Self.priorExchanges(in: item)
        if DecisionCardComposition.placesConversationWithSpecification(for: item._type),
            model.conversation != nil || !priorExchanges.isEmpty
        {
            let offersReply =
                item.requested_decision.contains(.discuss)
                && !actionRanking(item).unavailable.contains(.discuss)
            ConversationView(
                snapshot: model.conversation,
                priorExchanges: priorExchanges,
                reply: offersReply
                    ? .init(isEnabled: model.actionsEnabled && model.isSubmittable(.discuss)) {
                        trigger(.discuss, item: item)
                    } : nil,
                attachments: attachments,
                loadsAttachments: loadsAttachments,
                now: now,
                rendersInteractiveControls: rendersInteractiveControls)
        }
    }

    /// The specification as one bordered item under its keyword (frame
    /// 5.1): which revision the daemon bound to this approval, and the
    /// links that open it. A revision's diff is a second link on the same
    /// item, not a second item (frame 7.2).
    @ViewBuilder
    private func specificationMaterial(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let specification = Self.specificationClaim(in: item) {
            let title =
                Self.specificationRevisionIteration(in: item).map { "Revision \($0)" }
                ?? "Specification"
            let priorIteration = Self.priorSpecRevisionIteration(in: item)
            keywordSection("Specification") {
                VStack(alignment: .leading, spacing: 11) {
                    if specification.text != nil {
                        ViewThatFits(in: .horizontal) {
                            HStack(alignment: .center, spacing: 12) {
                                specificationItemTitle(title)
                                Spacer(minLength: 8)
                                specificationLinks(
                                    priorIteration: priorIteration,
                                    rendersInteractiveControls: rendersInteractiveControls)
                            }
                            VStack(alignment: .leading, spacing: 11) {
                                specificationItemTitle(title)
                                specificationLinks(
                                    priorIteration: priorIteration,
                                    rendersInteractiveControls: rendersInteractiveControls)
                            }
                        }
                    } else {
                        AttachmentRow(
                            label: title,
                            digest: specification.digest,
                            metadata: specification.metadata,
                            attachments: attachments,
                            loadsAttachments: loadsAttachments,
                            rendersInteractiveControls: rendersInteractiveControls)
                        if let priorIteration {
                            readerLink(
                                "Open Diff", reader: .diff,
                                accessibilityLabel: "Open diff from revision \(priorIteration)",
                                rendersInteractiveControls: rendersInteractiveControls)
                        }
                    }
                }
                .padding(.horizontal, 18)
                .padding(.vertical, 14)
                .frame(maxWidth: .infinity, alignment: .leading)
                .overlay(
                    RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
            }
        }
    }

    private func specificationItemTitle(_ title: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title)
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.ink)
            Text("Bound by the daemon to this approval")
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private func specificationLinks(
        priorIteration: Int?,
        rendersInteractiveControls: Bool
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 16) {
            readerLink(
                "Open Reader", reader: .specification,
                accessibilityLabel: "Open specification reader",
                rendersInteractiveControls: rendersInteractiveControls)
            if let priorIteration {
                readerLink(
                    "Open Diff", reader: .diff,
                    accessibilityLabel: "Open diff from revision \(priorIteration)",
                    rendersInteractiveControls: rendersInteractiveControls)
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

    /// A link that opens one of the approval's readers (R3: away is a
    /// link). A real `Button` where the surface is interactive.
    @ViewBuilder
    private func readerLink(
        _ title: String,
        reader: SpecApprovalReader,
        accessibilityLabel: String,
        rendersInteractiveControls: Bool
    ) -> some View {
        let link = FreesideLink(title: title, face: FreesideFont.noticeAction)
        if rendersInteractiveControls {
            Button {
                openSpecApprovalReader(reader)
            } label: {
                link.contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(accessibilityLabel)
        } else {
            link
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
        expandsTechnicalDetails: Bool = false,
        expandsLaterHunks: Bool = false
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
                    glyph: .inbox, title: "Specification unavailable",
                    description: "This approval does not carry a readable specification.")
            }
        case .diff:
            if let revision = item.spec_revision?.value1 {
                UnifiedDiffView(
                    unified: revision.diff.unified,
                    linesAdded: revision.diff.lines_added,
                    linesRemoved: revision.diff.lines_removed,
                    truncated: revision.diff.truncated,
                    rendersScrollableContent: rendersScrollableContent,
                    expandsLaterHunks: expandsLaterHunks)
            } else {
                UnavailableStateView(
                    glyph: .inbox, title: "Diff unavailable",
                    description: "This is the first specification revision.")
            }
        }
    }

    /// The Mac inspector's reader header: the row and the rule under it.
    @ViewBuilder
    private func specApprovalReaderHeaderRow(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool = true,
        close: @escaping () -> Void
    ) -> some View {
        SpecApprovalReaderHeader(
            reader: reader,
            revision: Self.specificationRevisionIteration(in: item),
            rendersInteractiveControls: rendersInteractiveControls,
            close: close
        )
        .padding(.horizontal)
        .padding(.vertical, 14)
        Divider()
    }

    /// The iPhone reader sheet's header: the same keyword and revision chip
    /// over the reader's title.
    private func specApprovalReaderSheetHeader(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem
    ) -> some View {
        FreesideSheetHeader(
            eyebrow: reader.keyword.full,
            chip: SpecApprovalReader.revisionChip(Self.specificationRevisionIteration(in: item)),
            ask: reader == .specification ? "Specification" : "Specification changes")
    }

    #if os(iOS)
        @ViewBuilder
        private func specApprovalReaderSheet(
            _ reader: SpecApprovalReader,
            item: Components.Schemas.AttentionItem
        ) -> some View {
            VStack(spacing: 0) {
                specApprovalReaderSheetHeader(reader, item: item)
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
        title: String,
        accessibilityLayout: Bool,
        prominent: Bool,
        unverified: UnverifiedRegister
    ) -> some View {
        if !claims.isEmpty {
            if prominent {
                cardSection(title, unverified: unverified) {
                    claimRows(claims, unverified: unverified)
                }
            } else {
                lowerSection(
                    title,
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
        unverified: UnverifiedRegister,
        boxed: Bool = false
    ) -> some View {
        // Position is the only stable identity: two claims may bind the same
        // artifact under different labels and neither field is unique.
        ForEach(Array(claims.enumerated()), id: \.offset) { _, claim in
            if let text = claim.text {
                claimProse(
                    claim, text: text,
                    rendersInteractiveControls: unverified.rendersInteractiveControls)
            } else {
                // A claim without inline text keeps the attachment row: its
                // loading, unavailable, and failed states
                // and their retry are that row's own.
                AttachmentRow(
                    label: claim.label, digest: claim.digest,
                    metadata: claim.metadata,
                    attachments: attachments,
                    loadsAttachments: loadsAttachments,
                    text: claim.text,
                    rendersInteractiveControls: unverified.rendersInteractiveControls,
                    boxed: boxed)
            }
        }
    }

    /// A text claim: its own words lead in the quote (R5), and the
    /// identifiers that bind them (its label, media type, producing
    /// invocation, and digest) sit one disclosure away with a copy control
    /// each.
    @ViewBuilder
    private func claimProse(
        _ claim: Components.Schemas.AgentClaim,
        text: Components.Schemas.ClaimText,
        rendersInteractiveControls: Bool
    ) -> some View {
        QuoteBlock {
            claimText(text.content, mediaType: text.media_type)
        }
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
            .init(label: "Media Type", value: text.media_type.rawValue),
            .init(label: "Agent Invocation", value: producerInvocationID(claim)),
            .init(label: "Claim Digest", value: claim.digest),
        ]
    }

    /// What the card's Evidence pointer counts, in the operator's words.
    static func evidencePointerCount(_ count: Int) -> String {
        count == 1 ? "1 attachment" : "\(count) attachments"
    }

    static let evidencePointerLinkTitle = "In inspector"
    /// The link spoken: the visible title leans on the row it sits in, and
    /// VoiceOver may reach the link alone.
    static let evidencePointerLinkAccessibilityLabel = "Show the attachments in the inspector"

    #if os(macOS)
        /// The card's Evidence module while the inspector holds the
        /// attachments (frame 6.9): how many there are, then a link to them
        /// (R3: away is a link), never a second copy of the rows.
        private func evidencePointer(count: Int, rendersInteractiveControls: Bool) -> some View {
            let countText = Text(Self.evidencePointerCount(count))
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.ink)
            let link = evidencePointerLink(rendersInteractiveControls: rendersInteractiveControls)
            return keywordSection("Evidence") {
                // One row where it fits, the link at its trailing edge as
                // a fact row's value is; the link drops under the count at
                // a size where it does not.
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        countText
                        Spacer(minLength: 0)
                        link
                    }
                    VStack(alignment: .leading, spacing: 4) {
                        countText
                        link
                    }
                }
            }
        }

        @ViewBuilder
        private func evidencePointerLink(rendersInteractiveControls: Bool) -> some View {
            let link = FreesideLink(
                title: Self.evidencePointerLinkTitle, face: FreesideFont.noticeAction)
            if rendersInteractiveControls {
                Button {
                    showEvidenceInInspector()
                } label: {
                    link.contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(Self.evidencePointerLinkAccessibilityLabel)
            } else {
                link
            }
        }
    #endif

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
        register: UnverifiedRegister
    ) -> some View {
        lowerSection(
            "Details",
            isExpanded: detailsExpanded,
            accessibilityLayout: accessibilityLayout
        ) {
            VStack(alignment: .leading, spacing: 6) {
                // The section is a disclosure at an accessibility size, and
                // a control inside it is not the card's always-reachable one.
                detailsReason(item, register: accessibilityLayout ? register.withoutInfo : register)
                ForEach(Array(detailRows(item).enumerated()), id: \.offset) { _, row in
                    TechnicalDetailRow(
                        row: row,
                        rendersInteractiveControls: register.rendersInteractiveControls)
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
            VStack(alignment: .leading, spacing: InspectorScale.sectionGap) {
                KeywordLabel(text: "Inspector")
                    .accessibilityAddTraits(.isHeader)
                let cardLeadClaims = DecisionCardComposition.forType(item._type)
                    .cardLeadClaims(
                        from: item.agent_claims,
                        prominentClaimIndex: graphics.prominentClaimIndex)
                let attachmentClaims = item.agent_claims.filter {
                    $0.text == nil && !AgentClaimLabels.isApprovalMaterial($0.label)
                        && !cardLeadClaims.contains($0)
                }
                // Every inspector section is a disclosure, so none of its
                // labels carries the card's explanation control.
                let register = unverified(
                    item,
                    effectProposalFacts: model.effectProposalFacts,
                    accessibilityLayout: isAccessibilityLayout,
                    rendersInteractiveControls: rendersInteractiveControls
                ).withoutInfo
                if !attachmentClaims.isEmpty {
                    inspectorSection(
                        "Claims",
                        count: attachmentClaims.count,
                        isExpanded: claimsExpanded,
                        unverified: register
                    ) {
                        claimRows(attachmentClaims, unverified: register, boxed: true)
                    }
                }
                if !item.evidence_snapshot.isEmpty {
                    inspectorSection(
                        "Evidence",
                        count: item.evidence_snapshot.count,
                        isExpanded: evidenceExpanded
                    ) {
                        ForEach(item.evidence_snapshot, id: \.id) { artifact in
                            AttachmentRow(
                                label: artifact._type.rawValue,
                                digest: artifact.digest,
                                metadata: artifact.metadata,
                                attachments: attachments,
                                loadsAttachments: loadsAttachments,
                                rendersInteractiveControls: rendersInteractiveControls,
                                boxed: true)
                        }
                    }
                    .id(ScrollTarget.evidence)
                }
                inspectorSection("Technical Bindings", isExpanded: detailsExpanded) {
                    detailsReason(item, register: register)
                    ForEach(Array(detailRows(item).enumerated()), id: \.offset) { _, row in
                        TechnicalDetailRow(
                            row: row, rendersInteractiveControls: rendersInteractiveControls,
                            stacksAlways: true)
                    }
                }
                .id(ScrollTarget.technicalDetails)
                .foregroundStyle(Color.inkDim)
                .textSelection(.enabled)
            }
            .environment(\.dynamicTypeSize, dynamicTypeSize)
        }
    #endif

    @ViewBuilder
    private func proposalRows(_ facts: Components.Schemas.TaskProposalFactsSnapshot) -> some View {
        factRow("Intent", value: facts.intent.rawValue)
        factRow("Expected Cost", value: "\(facts.expected_cost_units) units")
        factRow("Components", value: "\(facts.scope.component_count)")
        factRow("Declared Paths", value: "\(facts.scope.declared_path_count)")
        factRow("Control Plane", value: facts.scope.touches_control_plane ? "Yes" : "No")
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
        inspectorPresented: Bool = false,
        topMargin: CGFloat? = nil,
        actionRegionFrameChanged: ((CGRect) -> Void)? = nil
    ) -> some View {
        card(
            item,
            proposalFacts: proposalFacts,
            effectProposalFacts: effectProposalFacts,
            rendersInteractiveControls: false,
            accessibilityLayout: dynamicTypeSize >= .accessibility1,
            compactLayout: compactLayout,
            inspectorPresented: inspectorPresented,
            actionRegionFrameChanged: actionRegionFrameChanged
        )
        .detailCard(compact: compactLayout, topMargin: topMargin)
    }

    func screenshotBanner() -> some View {
        receipt(.wax, "Failed", "Submission failed: the daemon rejected the command.")
            .padding()
    }

    /// The uncertain receipt over the Retry its card's control group leads
    /// with.
    func screenshotRetryableReceipt(expanded: Bool) -> some View {
        VStack(alignment: .leading, spacing: CardScale.sectionGap) {
            RetryableReceipt(
                isExpanded: .constant(expanded),
                failureMessage: "the daemon did not answer")
            lostResponseRetry
        }
        .padding()
        // The card's own ground: the disclosure under the notice has no
        // wash of its own to carry its ink over the harness's light canvas.
        .background(Color.ground)
    }

    /// A reader as its host draws it: the Mac inspector's header row over
    /// the content, or, with `asSheet`, the iPhone sheet's header and fixed
    /// Done footer.
    func screenshotSpecApprovalReader(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem,
        expandsTechnicalDetails: Bool = false,
        expandsLaterHunks: Bool = false,
        asSheet: Bool = false
    ) -> some View {
        VStack(spacing: 0) {
            if asSheet {
                specApprovalReaderSheetHeader(reader, item: item)
            } else {
                specApprovalReaderHeaderRow(reader, item: item, rendersInteractiveControls: false) {}
            }
            specApprovalReaderContent(
                reader,
                item: item,
                rendersScrollableContent: false,
                expandsTechnicalDetails: expandsTechnicalDetails,
                expandsLaterHunks: expandsLaterHunks
            )
            .padding()
            .frame(maxWidth: .infinity, alignment: .topLeading)
            if asSheet {
                FreesideSheetActionRow.done {}
            }
        }
        .background(asSheet ? Color.ground2 : Color.sidebarGround)
    }

    #if os(macOS)
        func screenshotInspector(
            _ item: Components.Schemas.AttentionItem,
            at dynamicTypeSize: DynamicTypeSize
        ) -> some View {
            inspectorContent(item, rendersInteractiveControls: false)
                .padding(.vertical, InspectorScale.verticalPadding)
                .padding(.horizontal, InspectorScale.horizontalPadding)
                .frame(width: 360, alignment: .topLeading)
                .background(Color.sidebarGround)
        }
    #endif

    // One item per finding (plan §9 revision 78, visual audit D09, handoff
    // frame 7.1). An item shows its finding's exact message, who proposed
    // the route, and the route; everything else about that finding, the
    // route list included, sits in the item's own disclosure. Nothing drawn
    // from a proposal renders outside its item, so a route list can only
    // belong to the finding it sits under, and no content spans the action
    // region. A held alternative shows on the face, so closing an item
    // never hides a choice the operator can still send.
    //
    // The registers are told apart by shape: the daemon's message and its
    // coordinates are plain text and fact rows, and a model's route,
    // rationale, and supporting statements are quotes under unverified
    // keywords.
    @ViewBuilder
    private func findingCards(
        _ binding: Components.Schemas.FindingAdjudicationBinding,
        register: UnverifiedRegister
    ) -> some View {
        // The first producer label that says "(unverified)" carries the
        // card's explanation control; the rest draw the keyword alone.
        let infoCardID = FindingCardPresentation.cards(binding)
            .first { $0.producerUnverifiedKeyword != nil }?.id
        // The findings are one module, so they stand a module gap apart
        // rather than a section gap, which keeps two realistic findings
        // above the actions in the first viewport (#1141).
        VStack(alignment: .leading, spacing: CardScale.moduleGap) {
            ForEach(Array(binding.proposals.enumerated()), id: \.element.finding_id) {
                index, proposal in
                let card = FindingCardPresentation(proposal, number: index + 1, binding: binding)
                let isExpanded = expandedFindings.contains(card.id)
                VStack(alignment: .leading, spacing: 8) {
                    // The heading runs in with the message instead of standing
                    // on a line of its own: a line per finding is what two
                    // realistic findings have to spare inside the first
                    // viewport (#1141).
                    findingHead(card)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityElement(children: .ignore)
                        .accessibilityLabel(Text(card.messageAccessibilityLabel))
                        .accessibilityAddTraits(.isHeader)
                    VStack(alignment: .leading, spacing: 4) {
                        findingProducerLabel(
                            card, register: card.id == infoCardID ? register : register.withoutInfo)
                        // An open disclosure joins the rationale and its
                        // qualities to the route: one statement by one producer.
                        findingVoice(card) {
                            Text(card.route)
                                .font(FreesideFont.optionLabel)
                                .foregroundStyle(Color.ink)
                                .fixedSize(horizontal: false, vertical: true)
                                .accessibilityLabel(Text(card.routeAccessibilityLabel))
                            if isExpanded {
                                Text(card.rationale)
                                    .font(FreesideFont.cardBody)
                                    .foregroundStyle(Color.ink)
                                    .fixedSize(horizontal: false, vertical: true)
                                Text(card.qualities)
                                    .font(FreesideFont.cardBody)
                                    .foregroundStyle(Color.inkDim)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                        if let selected = alternativeSelections[card.id] {
                            Text(FindingCardPresentation.selectionNotice(selected))
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.ink)
                                .fixedSize(horizontal: false, vertical: true)
                                .accessibilityLabel(
                                    Text(card.selectionAccessibilityLabel(selected)))
                        }
                    }
                    SentenceDisclosure(
                        label: FindingCardPresentation.disclosureTitle,
                        spokenLabel: card.disclosureAccessibilityLabel,
                        isExpanded: findingExpansion(card.id)
                    ) {
                        findingDetail(
                            card,
                            selection: routeSelection(for: proposal),
                            rendersInteractiveControls: register.rendersInteractiveControls)
                    }
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .overlay(
                    RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
            }
        }
    }

    private func findingHead(_ card: FindingCardPresentation) -> Text {
        let heading = Text(card.heading.uppercased())
            .font(FreesideFont.keyword)
            .tracking(FreesideFont.keywordTracking)
            .foregroundStyle(Color.inkDim)
        guard !card.message.isEmpty else { return heading }
        return heading
            + Text("  \(card.message)")
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.ink)
    }

    /// The producer label, in the unverified register where the label
    /// carries the word (visual audit D03).
    @ViewBuilder
    private func findingProducerLabel(
        _ card: FindingCardPresentation,
        register: UnverifiedRegister
    ) -> some View {
        if let keyword = card.producerUnverifiedKeyword {
            UnverifiedLabel(
                text: keyword, carriesInfo: register.carriesInfo,
                rendersInteractiveControls: register.rendersInteractiveControls)
        } else {
            KeywordLabel(text: card.producerLabel)
        }
    }

    /// A proposal's own words in their producer's shape: a model's as a
    /// quote, the daemon fast path's as the daemon's text.
    @ViewBuilder
    private func findingVoice(
        _ card: FindingCardPresentation,
        @ViewBuilder content: () -> some View
    ) -> some View {
        let words = VStack(alignment: .leading, spacing: 4) { content() }
            .frame(maxWidth: .infinity, alignment: .leading)
        if card.modelBacked {
            // The padding a choice-list option takes, so the proposed route
            // reads the same on the face and in the route list.
            words
                .padding(.horizontal, 14)
                .padding(.vertical, 9)
                .quoteSurface()
        } else {
            words
        }
    }

    private func findingDetail(
        _ card: FindingCardPresentation,
        selection: Binding<Components.Schemas.AdjudicationRoute>,
        rendersInteractiveControls: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            findingStatements(
                "Evidence", card.evidence, card: card,
                rendersInteractiveControls: rendersInteractiveControls)
            // The finding's coordinates are daemon-authenticated, so they
            // keep their own register inside the disclosure, never mixed
            // into the producer's content around them (#892).
            keywordSection("Daemon Facts") {
                ForEach(card.daemonFacts) { factRow($0) }
            }
            findingStatements(
                card.citedRulesKeyword, card.citedRules, card: card,
                rendersInteractiveControls: rendersInteractiveControls)
            findingStatements(
                "Assumes", card.assumptions, card: card,
                rendersInteractiveControls: rendersInteractiveControls)
            if !card.routeOptions.isEmpty {
                keywordSection("Route") {
                    ChoiceList(
                        accessibilityLabel: "Route for \(card.heading)",
                        options: card.routeOptions.map {
                            .init(
                                value: $0.route, label: $0.label, consequence: $0.consequence,
                                mark: $0.isProposed ? "Proposed" : nil,
                                register: card.modelBacked ? .quote : .item)
                        },
                        selection: selection)
                }
            }
            findingStatements(
                card.gatingQuestionsKeyword, card.gatingQuestions, card: card,
                rendersInteractiveControls: rendersInteractiveControls)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// What a proposal rests on, in its producer's shape: each of a model's
    /// statements a quote under an unverified keyword, the daemon fast
    /// path's as the daemon's text under a plain one. The card's one
    /// explanation control stays on its face, so these labels draw none.
    @ViewBuilder
    private func findingStatements(
        _ keyword: String,
        _ statements: [String],
        card: FindingCardPresentation,
        rendersInteractiveControls: Bool
    ) -> some View {
        if !statements.isEmpty {
            VStack(alignment: .leading, spacing: 6) {
                if card.modelBacked {
                    UnverifiedLabel(
                        text: keyword, carriesInfo: false,
                        rendersInteractiveControls: rendersInteractiveControls)
                } else {
                    KeywordLabel(text: keyword)
                }
                ForEach(Array(statements.enumerated()), id: \.offset) { _, statement in
                    findingVoice(card) {
                        Text(statement)
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
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

    /// The route picked for a finding: its proposed route until the operator
    /// picks another, and again once they pick it back, so a held pick is
    /// always one the daemon accepts as an alternative.
    private func routeSelection(
        for proposal: Components.Schemas.FindingAdjudicationProposal
    ) -> Binding<Components.Schemas.AdjudicationRoute> {
        Binding(
            get: { alternativeSelections[proposal.finding_id] ?? proposal.route },
            set: { route in
                if route == proposal.route {
                    alternativeSelections.removeValue(forKey: proposal.finding_id)
                } else {
                    alternativeSelections[proposal.finding_id] = route
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
            rows.append(.init(label: "Bound Head", value: merge.candidate_head_sha))
            rows.append(.init(label: "Bound Base", value: "\(merge.base_ref)@\(merge.base_sha)"))
            rows.append(.init(label: "Publication Identity", value: merge.publication_identity))
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
        factRow("Prior Intent", value: prior.intent.rawValue)
        factRow("Prior Cost", value: "\(prior.expected_cost_units) units")
        factRow(
            "Prior Scope",
            value: "\(prior.scope.component_count) components, \(prior.scope.declared_path_count) paths")
        factRow(
            "Prior Control Plane", value: prior.scope.touches_control_plane ? "Yes" : "No")
    }

    /// The type eyebrow (R27): the type's name, with the item's badges in
    /// the trailing slot. On a card that leads with the agent's own prose
    /// the eyebrow names that register and carries the explanation control.
    private func eyebrow(
        _ item: Components.Schemas.AttentionItem,
        register: UnverifiedRegister,
        accessibilityLayout: Bool
    ) -> some View {
        let eyebrow = DecisionCardComposition.eyebrow(for: item)
        return CardEyebrow(
            keyword: eyebrow.keyword,
            carriesInfo: eyebrow.carriesInfo,
            rendersInteractiveControls: register.rendersInteractiveControls
        ) {
            headerBadges(item, accessibilityLayout: accessibilityLayout)
        }
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
            // A revised specification names its revision beside the type
            // (frame 7.2); a first revision has nothing to tell apart.
            if item._type == .spec_approval, Self.priorSpecRevisionIteration(in: item) != nil,
                let iteration = Self.specificationRevisionIteration(in: item)
            {
                StateChip(label: "Revision \(iteration)", color: .inkDim)
            }
            if AttentionDisplay.showsPriorityBadge(item.priority) {
                PriorityBadge(priority: item.priority)
            }
            if AttentionDisplay.showsLifecycleBadge(item.status) {
                StatusBadge(status: item.status)
            }
        }
    }

    @ViewBuilder
    private func banner() -> some View {
        if model.phase == .superseded {
            receipt(
                .accent, "Superseded",
                "This item changed before your decision applied. Nothing was committed; re-review the replacement below."
            )
        } else {
            // An applied record persists even when the item stays open
            // (a non-resolving action such as acknowledge or open_pr).
            if let record = model.appliedRecord {
                // Success is quiet: a neutral wash, never green and never
                // the accent.
                receipt(
                    .neutral, "Recorded",
                    "Decision applied: \(AttentionDisplay.label(record.action, for: model.snapshot?.item))")
            }
            // The uncertain receipt outranks a failure: when a preserved
            // command may hold a recorded result, resending it is the
            // actionable step, whatever else failed.
            if offersLostResponseRetry {
                RetryableReceipt(
                    isExpanded: $lostResponseExpanded,
                    failureMessage: model.submissionError)
            } else if case .failed(let message) = model.validation {
                receipt(.wax, "Failed", "Couldn't validate current state: \(message)")
            } else if let message = model.submissionError {
                receipt(.wax, "Failed", "Submission failed: \(message)")
            }
        }
    }

    /// Whether a command's answer was lost and the card offers to resend
    /// it: the receipt states it, and the control group holds the Retry.
    private var offersLostResponseRetry: Bool {
        model.phase != .superseded && model.canRetryLostResponse
    }

    /// The uncertain receipt's act (R14). It leads the control group, and
    /// a card that can retry always draws one: every command a card sends
    /// is one of its item's requested decisions.
    private var lostResponseRetry: some View {
        Button("Retry") {
            Task { await model.retryLostResponse() }
        }
        .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
        .accessibilityLabel("Retry the lost decision")
    }

    /// The recommendation in the register its revalidated provenance
    /// supports (plan §9), drawn as R20 sets it: the keyword, with the
    /// unverified word when an agent judged; one sentence naming who
    /// recommends, the action, and the confidence; the recommendation's own
    /// reason, which plan §9 requires; then the filled action. It argues
    /// before it acts: the block led with the act until 2026-09-03, on the
    /// reading that an operator decides rather than audits; the owner
    /// reversed that after the September UI audit (#1104, #1107), so the
    /// button is the conclusion of the argument above it rather than a
    /// control the reason trails. The block draws no frame and no wash,
    /// because accent means the fill, a link, or the attention mark (R23).
    private func recommendationBlock(
        _ recommendation: DecisionRecommendationPresentation,
        item: Components.Schemas.AttentionItem,
        register: UnverifiedRegister
    ) -> some View {
        VStack(alignment: .leading, spacing: CardScale.moduleGap) {
            if recommendation.register.isUnverifiedClaim {
                UnverifiedLabel(
                    text: DecisionRecommendationPresentation.keyword,
                    carriesInfo: register.carriesInfo,
                    rendersInteractiveControls: register.rendersInteractiveControls)
            } else {
                KeywordLabel(text: DecisionRecommendationPresentation.keyword)
            }
            Text(recommendation.sentence(for: item))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
            Text(recommendation.reason)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
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
            SentenceDisclosure(label: "Provenance", isExpanded: $provenanceExpanded) {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(recommendation.sourceFacts) { fact in
                        factRow(fact.label, value: fact.value)
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
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
    /// content. The "(unverified)" label and whether the label carries the
    /// card's one explanation control both follow from this one value
    /// rather than from the title's text, so a section cannot claim one and
    /// draw another.
    struct UnverifiedRegister {
        let rendersInteractiveControls: Bool
        /// The slot whose first label carries the explanation control (R25).
        /// Nil when every unverified keyword on the card is a disclosure's
        /// own label, which has no room for a second control.
        let infoSlot: DecisionCardComposition.UnverifiedSlot?
        /// Whether the label drawn with this value is that first label.
        var carriesInfo = false

        func at(_ slot: DecisionCardComposition.UnverifiedSlot) -> UnverifiedRegister {
            var register = self
            register.carriesInfo = infoSlot == slot
            return register
        }

        var withoutInfo: UnverifiedRegister {
            var register = self
            register.carriesInfo = false
            return register
        }
    }

    private func unverified(
        _ item: Components.Schemas.AttentionItem,
        effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot?,
        accessibilityLayout: Bool,
        rendersInteractiveControls: Bool
    ) -> UnverifiedRegister {
        let context = DecisionCardComposition.UnverifiedContext(
            accessibilityLayout: accessibilityLayout,
            drawsUnverifiedRecommendation: drawnRecommendation(item)?.register.isUnverifiedClaim
                == true,
            hasChangeSummary: graphics.changeSummary != nil,
            hasComparison: graphics.comparison != nil,
            hasProposedIssueText: effectProposalFacts.flatMap(AttentionDisplay.proposedIssueText)
                != nil,
            prominentClaimIndex: graphics.prominentClaimIndex)
        return UnverifiedRegister(
            rendersInteractiveControls: rendersInteractiveControls,
            infoSlot: DecisionCardComposition.forType(item._type)
                .infoSlot(for: item, in: context))
    }

    /// The recommendation the card draws: the revalidated one, while its
    /// action is still the one the surface ranks first.
    private func drawnRecommendation(
        _ item: Components.Schemas.AttentionItem
    ) -> DecisionRecommendationPresentation? {
        guard let recommendation = DecisionRecommendationPresentation.of(item),
            actionRanking(item).recommended == recommendation.action
        else { return nil }
        return recommendation
    }

    /// Whether the card draws the recommendation as a module ahead of its
    /// lead. Only iPhone does; the Mac draws it in the action region.
    private func drawsRecommendationModule(_ item: Components.Schemas.AttentionItem) -> Bool {
        #if os(iOS)
            drawnRecommendation(item) != nil
        #else
            false
        #endif
    }

    /// A disclosure's own label draws the keyword without the control: the
    /// label is what opens the section, so a second button inside it would
    /// hand touch and VoiceOver the disclosure rather than the explanation.
    @ViewBuilder
    private func sectionTitle(
        _ title: String,
        unverified: UnverifiedRegister?,
        isDisclosureLabel: Bool = false
    ) -> some View {
        if let unverified {
            UnverifiedLabel(
                text: title, carriesInfo: unverified.carriesInfo && !isDisclosureLabel,
                rendersInteractiveControls: unverified.rendersInteractiveControls)
        } else {
            KeywordLabel(text: title)
        }
    }

    /// The explanation as the first line of a folded section, on a card
    /// that has no visible label to carry the control: opening the section
    /// is the demand, and plan §9 still has the explanation reachable.
    @ViewBuilder
    private func foldedUnverifiedSentence(_ unverified: UnverifiedRegister?) -> some View {
        if let unverified, unverified.infoSlot == nil {
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
            dashed: false,
            boxed: unverified == nil,
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

    /// A section set apart by spacing alone (R1, R26):
    /// its keyword, then its content on the module gap, with no box.
    private func keywordSection(
        _ title: String, @ViewBuilder content: () -> some View
    ) -> some View {
        VStack(alignment: .leading, spacing: CardScale.moduleGap) {
            KeywordLabel(text: title)
            content()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
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
            // The sentence disclosure every other fold on the card and the
            // inspector's sections use (R2). Its label carries no unverified
            // register, so a section of agent claims draws the register as
            // its first line and says it aloud on the disclosure.
            let disclosure = SentenceDisclosure(
                label: title,
                spokenLabel: unverified == nil ? nil : Self.unverifiedDisclosureSpokenLabel(title),
                isExpanded: isExpanded
            ) {
                VStack(alignment: .leading, spacing: 8) {
                    if let unverified {
                        UnverifiedLabel(
                            text: title, carriesInfo: false,
                            rendersInteractiveControls: unverified.rendersInteractiveControls)
                    }
                    foldedUnverifiedSentence(unverified)
                    content()
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            if unverified != nil {
                disclosure.frame(maxWidth: .infinity, alignment: .leading)
            } else {
                disclosure
                    .padding(12)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .freesideCard()
            }
        } else {
            cardSection(title, unverified: unverified) {
                content()
            }
        }
    }

    /// The inspector pane's measures (frame 6.9).
    private enum InspectorScale {
        static let sectionGap: CGFloat = 16
        static let verticalPadding: CGFloat = 20
        static let horizontalPadding: CGFloat = 18
        /// A section's rows start under its label's text, past the glyph.
        static let rowIndent: CGFloat = 19
        static let rowGap: CGFloat = 8
    }

    /// What VoiceOver calls a one-pane card's folded section of agent
    /// claims, whose drawn label is the section's title alone.
    static func unverifiedDisclosureSpokenLabel(_ title: String) -> String {
        "\(title), unverified"
    }

    /// What VoiceOver calls the inspector's claims disclosure. The drawn
    /// label is `Claims` and the unverified mark draws inside the section,
    /// so the closed disclosure has to say whose claims they are.
    static func inspectorClaimsSpokenLabel(count: Int) -> String {
        "Claims, \(count), unverified agent claims"
    }

    /// One inspector section (R2, frame 6.9): a sentence disclosure with its
    /// count as the trailing summary and its rows indented under the label,
    /// set apart by spacing alone. A section of agent claims keeps the
    /// unverified register as its first line, since the sentence label
    /// carries none.
    private func inspectorSection<Content: View>(
        _ title: String,
        count: Int? = nil,
        isExpanded: Binding<Bool>,
        unverified: UnverifiedRegister? = nil,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        SentenceDisclosure(
            label: title,
            summary: count.map { "\($0)" },
            spokenLabel: unverified == nil
                ? nil : Self.inspectorClaimsSpokenLabel(count: count ?? 0),
            isExpanded: isExpanded
        ) {
            VStack(alignment: .leading, spacing: InspectorScale.rowGap) {
                if let unverified {
                    UnverifiedLabel(
                        text: "Agent claims", carriesInfo: false,
                        rendersInteractiveControls: unverified.rendersInteractiveControls)
                }
                foldedUnverifiedSentence(unverified)
                content()
            }
            .padding(.leading, InspectorScale.rowIndent)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func factRow(_ label: String, value: String) -> some View {
        FactRow(label: label, value: value)
    }

    /// A card fact in the form its value takes. A diff fact draws its counts
    /// in the diff cuts (R28) and reads them aloud in words, since the signs
    /// carry the meaning only on screen.
    @ViewBuilder
    private func factRow(_ fact: AttentionDisplay.FactRow) -> some View {
        switch fact.form {
        case .plain:
            factRow(fact.label, value: fact.value)
        case .diffs(let diffs):
            FactRow(label: fact.label, value: fact.value, drawn: DiffCounts.text(diffs))
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(Text("\(fact.label): \(DiffCounts.spoken(diffs))"))
        case .posture(let posture):
            FactRow(label: fact.label, chip: StateChip(posture: posture))
        case .item(let id):
            FactLinkRow(label: fact.label, value: fact.value) { onSelectItem(id) }
        case .link(let url):
            FactLinkRow(label: fact.label, value: fact.value) { openURL(url) }
        }
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
        /// Draws `text` as the card draws an agent summary, its Markdown as
        /// blocks, where the row is that summary's original report.
        var drawsTextAsSummary = false
        var rendersInteractiveControls = true
        /// Draws the row in its own 1pt box, as the inspector lists its
        /// attachments (frame 6.9). A row inside a card section or a
        /// message is already set apart and draws none.
        var boxed = false

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
                if let text, drawsTextAsSummary {
                    DecisionSummaryText(blocks: DecisionSummaryPresentation.blocks(text))
                } else if let text {
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
            .padding(.vertical, boxed ? 8 : 0)
            .padding(.horizontal, boxed ? 10 : 0)
            .overlay {
                if boxed {
                    RoundedRectangle(cornerRadius: 6).strokeBorder(Color.itemBorder, lineWidth: 1)
                }
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
                .font(FreesideFont.attachmentState())
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
                        .font(FreesideFont.attachmentState(emphasized: true))
                    Text("The daemon reports the attachment bytes are not available")
                        .font(FreesideFont.attachmentState())
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
                        .font(FreesideFont.attachmentState(emphasized: true))
                    Text("The fetch failed. Try again.")
                        .font(FreesideFont.attachmentState())
                    if rendersInteractiveControls, loadsAttachments {
                        Button("Retry") { retryFetch() }
                            .buttonStyle(.bordered)
                            .controlSize(.small)
                            // A small control's own face is 11pt on macOS.
                            .font(FreesideFont.attachmentState())
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
                            .font(FreesideFont.attachmentState(emphasized: true))
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
                .font(FreesideFont.attachmentState())
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
                .font(FreesideFont.attachmentState())
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
                .font(FreesideFont.attachmentFact)
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
                    .font(FreesideFont.attachmentFact)
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
                FreesideSheetHeader(ask: label)
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
                FreesideSheetHeader(ask: label)
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
                            glyph: .inbox, title: "Preview unavailable",
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

    /// The uncertain receipt (R12): the accent notice, and the explanation
    /// one disclosure away. Its Retry is a button in the card's control
    /// group (R14).
    private struct RetryableReceipt: View {
        @Binding var isExpanded: Bool
        let failureMessage: String?

        var body: some View {
            VStack(alignment: .leading, spacing: 8) {
                Notice(
                    tone: .accent, keyword: "Unconfirmed",
                    sentence: "The daemon did not answer. Nothing is assumed."
                )
                .accessibilityElement(children: .combine)
                SentenceDisclosure(label: "What Happened", isExpanded: $isExpanded) {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(
                            "The decision may already be recorded. Retry resends the same command and returns the original result."
                        )
                        if let failureMessage {
                            Text(failureMessage)
                        }
                    }
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .textSelection(.enabled)
        }
    }

    /// One receipt row (R12): a notice whose keyword names the state.
    private func receipt(_ tone: Notice.Tone, _ keyword: String, _ sentence: String) -> some View {
        Notice(tone: tone, keyword: keyword, sentence: sentence)
            .textSelection(.enabled)
            .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private func actions(
        _ item: Components.Schemas.AttentionItem,
        stackedLayout: Bool,
        includesReviewing: Bool
    ) -> some View {
        let ranking = actionRanking(item)
        let controlGap = CardScale.controlGap
        let filledAction = DecisionCardComposition.filledAction(for: item, ranking: ranking)
        // Position, because a repeated action fills only its first button.
        let filled = filledAction.flatMap(ranking.principal.firstIndex(of:))
        VStack(alignment: .leading, spacing: controlGap) {
            if showsValidationProgress && model.validation == .pending {
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small).tint(.waterText)
                    // The label reads as the disabled state it describes;
                    // the spinner keeps its water tint because the work is
                    // still in progress. A fixed face, because the platform
                    // mono caption draws under the 11.5pt floor on macOS.
                    Text("Validating current state…")
                        .font(FreesideFont.trailingSummary)
                        .foregroundStyle(Color.inkFaint)
                }
            }

            // The recommendation's sentence states what accepting covers. An
            // item whose recommendation did not revalidate has no block, and
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
                    VStack(alignment: .leading, spacing: controlGap) {
                        ForEach(Array(ranking.principal.enumerated()), id: \.offset) { index, action in
                            actionButton(action, item: item, tone: index == filled ? .primary : .secondary)
                        }
                    }
                } else {
                    HStack(alignment: .top, spacing: controlGap) {
                        ForEach(Array(ranking.principal.enumerated()), id: \.offset) { index, action in
                            actionButton(action, item: item, tone: index == filled ? .primary : .secondary)
                        }
                    }
                }
            }

            if includesReviewing, let reviewing = ranking.reviewing {
                actionButton(
                    reviewing, item: item, tone: filledAction == reviewing ? .primary : .secondary)
            }

            overflowMenu(ranking.overflow, item: item)

            if ranking.notDecidableHere {
                receipt(
                    .accent, "Unsupported",
                    "This decision needs a written answer, and this build cannot carry one. Nothing is blocked by opening it; the item stays open until answered."
                )
                .onAppear { model.emitNotDecidableHereShown() }
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
                tone: DecisionCardComposition.filledAction(for: item, ranking: ranking) == reviewing
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
            servedActions: model.actionSurface?.actions,
            alsoOverflowing: DecisionCardComposition.overflowActions(for: item._type))
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
                Text("More Actions \u{25BE}")
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
            // The style sets the height, from the platform's ladder.
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
        item: Components.Schemas.AttentionItem,
        showsIcon: Bool = true
    ) -> some View {
        if showsIcon, let systemImage = AttentionDisplay.systemImage(action) {
            Label(AttentionDisplay.label(action, for: item), systemImage: systemImage)
        } else {
            Text(AttentionDisplay.label(action, for: item))
        }
    }

    private var canPresentLaunchComposer: Bool {
        guard let launchComposer, let item = model.snapshot?.item else { return false }
        return DecisionLaunchComposerGate.canPresent(
            launchComposer.action,
            requested: item.requested_decision,
            unavailable: actionRanking(item).unavailable,
            actionsEnabled: model.actionsEnabled,
            isSubmittable: model.isSubmittable(launchComposer.action))
    }

    /// Opens the launch's composer through the path its button takes, then
    /// consumes the request so reselecting the item doesn't reopen it.
    private func presentLaunchComposerIfReady() {
        guard canPresentLaunchComposer, let launchComposer, let item = model.snapshot?.item
        else { return }
        trigger(launchComposer.action, item: item)
        onConsumeLaunchComposer()
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
    /// The header the sheet and its screenshot composition share (R11).
    private static var header: FreesideSheetHeader {
        FreesideSheetHeader(eyebrow: "Start with changes", ask: "What should change before it starts?")
    }

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
            Self.header
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
        VStack(alignment: .leading, spacing: 0) {
            Self.header
            VStack(alignment: .leading, spacing: 18) {
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
                .font(FreesideFont.body)
                .padding(14)
                .freesideCard()
                Text("Expected cost must be a whole number from 1 to 1,000,000 units.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
            FreesideSheetActionRow(
                submitLabel: "Submit",
                isSubmitEnabled: revision != nil,
                submit: {}, cancel: {})
        }
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
    /// The header the sheet and its screenshot composition share (R11).
    private static var header: FreesideSheetHeader {
        FreesideSheetHeader(eyebrow: "Approve with changes", ask: "Should merging close the issue?")
    }

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
            Self.header
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
        VStack(alignment: .leading, spacing: 0) {
            Self.header
            VStack(alignment: .leading, spacing: 18) {
                VStack(alignment: .leading, spacing: 8) {
                    KeywordLabel(text: "On merge")
                    Text(resolves ? "Closes the issue" : "Doesn't close the issue")
                    Divider()
                    Text(toggleTitle)
                }
                .font(FreesideFont.body)
                .padding(14)
                .freesideCard()
                Text("Approving with changes flips only whether the pull request closes the issue.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
            FreesideSheetActionRow(
                submitLabel: "Approve",
                isSubmitEnabled: revision != nil,
                submit: {}, cancel: {})
        }
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
        // The sheet's own ground, so the dusk composition reads dusk ink on
        // dusk ground rather than on the harness's light canvas.
        .background(Color.ground)
    }
}

struct TaskProposalSnoozeSheet: View {
    /// The header the sheet and its screenshot composition share (R11).
    private static var header: FreesideSheetHeader {
        FreesideSheetHeader(eyebrow: "Snooze proposal", ask: "When should this proposal return?")
    }

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
            Self.header
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
        VStack(alignment: .leading, spacing: 0) {
            Self.header
            VStack(alignment: .leading, spacing: 18) {
                VStack(alignment: .leading, spacing: 8) {
                    KeywordLabel(text: "Snooze until")
                    Text(formattedScreenshotUntil)
                }
                .font(FreesideFont.body)
                .padding(14)
                .freesideCard()
                Text("The proposal returns to the inbox at this date and time.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
            FreesideSheetActionRow(
                submitLabel: "Snooze",
                isSubmitEnabled: Self.isValidSnooze(until: until, now: now),
                submit: {}, cancel: {})
        }
        .frame(maxWidth: 560, alignment: .leading)
        .foregroundStyle(Color.ink)
        // The sheet's own ground, so the dusk composition reads dusk ink on
        // dusk ground rather than on the harness's light canvas.
        .background(Color.ground)
    }
}

extension StateChip {
    /// A system-health item's admission posture: wax where the finding
    /// gates unattended admission, dim where it only advises.
    init(posture: Components.Schemas.HealthPosture) {
        let color: Color =
            switch posture {
            case .blocking: .waxText
            case .advisory: .inkDim
            }
        self.init(label: AttentionDisplay.label(posture), color: color)
    }
}

extension View {
    /// The detail column's card (R18): the card's padding, ground, and
    /// border, top-leading inside the pane's margin and no wider than its
    /// cap at any pane width. The decision card, the operational summary,
    /// and both timelines draw on it, so every detail surface starts at one
    /// x and one y. `topMargin` replaces the pane's top margin for a card
    /// that sits under a row of its own.
    func detailCard(compact: Bool, topMargin: CGFloat? = nil) -> some View {
        var margin = CardScale.paneMargin(compact: compact)
        if let topMargin { margin.top = topMargin }
        return padding(CardScale.padding(compact: compact))
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .freesideCard(cornerRadius: CardScale.cornerRadius)
            .frame(maxWidth: CardScale.cardWidth, alignment: .topLeading)
            .padding(margin)
            .frame(maxWidth: .infinity, alignment: .topLeading)
    }

    /// A timeline's page. At regular width it is the detail column's card,
    /// seated as `detailCard` seats it; at compact width the timeline is
    /// the pushed screen itself and draws on the page's ground, as the
    /// 8 Oct phone frame has it. One chain serves both, with the width
    /// choosing its values: a branch would rebuild an open timeline when
    /// its size class changes (a resized iPad window, a rotated phone) and
    /// drop its folds and loaded evidence.
    func timelinePage(compact: Bool, topMargin: CGFloat? = nil) -> some View {
        var margin = compact ? EdgeInsets() : CardScale.paneMargin(compact: false)
        if !compact, let topMargin { margin.top = topMargin }
        let inset =
            compact
            ? EdgeInsets(top: 24, leading: 24, bottom: 24, trailing: 24)
            : CardScale.padding(compact: false)
        return padding(inset)
            .frame(maxWidth: compact ? 820 : .infinity, alignment: compact ? .leading : .topLeading)
            .background {
                if !compact { Color.clear.freesideCard(cornerRadius: CardScale.cornerRadius) }
            }
            .frame(maxWidth: compact ? nil : CardScale.cardWidth, alignment: .topLeading)
            .padding(margin)
            .frame(maxWidth: compact ? nil : .infinity, alignment: .topLeading)
    }
}
