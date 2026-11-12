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
    /// The card's seat in the task page, which owns the scroll view: the
    /// card draws none of its own, and on iOS hands its sticky recommended
    /// action up to the page (`TaskPageFooterKey`). Its identifier rows join
    /// the page's Identifiers footer rather than a fold of its own (#2607).
    struct PageSeat {
        let scrollProxy: ScrollViewProxy
        /// Hands the card's identifier rows, keyed by item id, to the page.
        /// Only the card can build them, from facts its model fetches, and
        /// the page's footer is drawn by the page, not the card.
        var publishIdentifiers: (String, [AttentionDisplay.BindingRow]) -> Void = { _, _ in }
        /// Opens the page's Identifiers footer and scrolls to it.
        var revealIdentifiers: () -> Void = {}
    }

    /// The scroll view the recommendation's visibility is measured against:
    /// the card's own, or the task page's when the card is seated there.
    static let scrollSpace = "decision-card-scroll"

    private enum ScrollTarget: Hashable {
        case identifiers
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
    @Environment(\.taskPageHoldMarker) private var pageHoldMarker
    /// The system italic's size for emphasis in a finding's title, which runs
    /// in at the card body's size (`DecisionSummaryText.inline`).
    @ScaledMetric(relativeTo: .body) private var findingEmphasisSize = FreesideLadder.current.body
    @State private var model: DecisionModel
    @State private var proposalEditor: ProposalEditor?
    @State private var messageEditor: MessageEditor?
    /// The files the open Discuss sheet will send, on a spec approval
    /// (#2995). Emptied whenever a message sheet closes, sent or not.
    @State private var discussAttachments = TaskAttachmentDraft(for: .message)
    @State private var specApprovalReader: SpecApprovalReader?
    @State private var pendingConfirmation: PendingConfirmation?
    @State private var capabilityRetrySnapshot: Components.Schemas.AttentionItemSnapshot?
    @State private var alternateAgentRetrySnapshot: Components.Schemas.AttentionItemSnapshot?
    @State private var actionDetailsRevealRequest: TechnicalDetailsRevealRequest?
    @State private var sectionPreferences: DecisionSectionPreferences
    @State private var recommendationVisible = true
    @State private var provenanceExpanded = false
    @State private var lostResponseExpanded = false
    @State private var alternativeSelections: [String: Components.Schemas.AdjudicationRoute]
    /// The reply reason the operator typed for each external finding, by
    /// finding id. Kept when the route changes, so picking Decline again
    /// brings the draft back; only a route that replies sends it.
    @State private var findingReasons: [String: String]
    /// The finding rows the operator opened, by finding id. Empty by default:
    /// a finding_adjudication card leads with collapsed rows so its actions
    /// stay in the first viewport (#1107).
    @State private var expandedFindings: Set<String>
    /// Which findings' clamped reasons are expanded, remembered on this
    /// device per item and finding id (#2608).
    @State private var findingReasonPreferences: FindingReasonPreferences
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
    /// The gap above the card where a standing notice sits over it. Nil
    /// keeps the pane's own margin.
    private let topMargin: CGFloat?
    /// Seats the card in the task page. Nil is the card alone in the pane.
    private let seat: PageSeat?
    private let onSelectItem: (String) -> Void
    /// The tasks a closed card's timeline link, and a fact's task link,
    /// resolve against.
    private let tasks: [Components.Schemas.TaskSnapshot]
    private let onSelectTask: (String) -> Void
    /// The synced usage pools the Choose Another Agent dialog reads each
    /// offer's reading from. The card is built from an `InboxStore`, not
    /// the sync coordinator, so the pools come in as a value.
    private let usagePools: [Components.Schemas.UsagePoolSnapshot]

    @MainActor
    init(
        store: InboxStore,
        itemID: String,
        detailsExpanded: Bool = false,
        expandedFindings: Set<String> = [],
        alternativeSelections: [String: Components.Schemas.AdjudicationRoute] = [:],
        findingReasons: [String: String] = [:],
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
        findingReasonPreferences: FindingReasonPreferences? = nil,
        topMargin: CGFloat? = nil,
        seat: PageSeat? = nil,
        onSelectItem: @escaping (String) -> Void = { _ in },
        tasks: [Components.Schemas.TaskSnapshot] = [],
        onSelectTask: @escaping (String) -> Void = { _ in },
        usagePools: [Components.Schemas.UsagePoolSnapshot] = [],
        onConclusion: @escaping @MainActor (DecisionConclusion) -> Void = { _ in },
        model: DecisionModel? = nil
    ) {
        // `model` is for a test or screenshot that needs the card after a
        // command: it passes the model it already drove, which keeps its own
        // `onConclusion`. The app passes none and the card builds its own.
        _model = State(
            initialValue: model
                ?? DecisionModel(
                    store: store, itemID: itemID, onConclusion: onConclusion))
        let revealsTechnicalDetails =
            detailsExpanded || detailsRevealRequest?.itemID == itemID
        _sectionPreferences = State(
            initialValue: sectionPreferences
                ?? DecisionSectionPreferences(
                    detailsExpandedOverride: revealsTechnicalDetails ? true : nil))
        _expandedFindings = State(initialValue: expandedFindings)
        // Nil keeps the reasons in memory, so a screenshot or test neither
        // reads this device's state nor leaves any behind; the app passes
        // `.shared`.
        _findingReasonPreferences = State(
            initialValue: findingReasonPreferences ?? FindingReasonPreferences(defaults: nil))
        _alternativeSelections = State(initialValue: alternativeSelections)
        _findingReasons = State(initialValue: findingReasons)
        _expandedDisclosures = State(initialValue: expandedDisclosures)
        attachments = store.attachments
        self.itemID = itemID
        self.expandsSummaryReports = expandsSummaryReports
        self.externalDetailsRevealRequest = detailsRevealRequest
        self.onConsumeDetailsRevealRequest = onConsumeDetailsRevealRequest
        self.launchComposer = launchComposer
        self.onConsumeLaunchComposer = onConsumeLaunchComposer
        self.topMargin = topMargin
        self.seat = seat
        self.onSelectItem = onSelectItem
        self.tasks = tasks
        self.onSelectTask = onSelectTask
        self.usagePools = usagePools
        self.graphics = graphics
        self.loadsAttachments = loadsAttachments
        self.showsValidationProgress = showsValidationProgress
        self.now = now
    }

    var body: some View {
        platformBody {
            Group {
                if let snapshot = model.snapshot {
                    let drawn = card(
                        snapshot.item,
                        proposalFacts: model.proposalFacts,
                        effectProposalFacts: model.effectProposalFacts,
                        accessibilityLayout: isAccessibilityLayout,
                        compactLayout: horizontalSizeClass == .compact,
                        onPage: seat != nil
                    )
                    if let seat {
                        // The page draws the scroll view and its title.
                        let identifiers = Self.identifierRows(
                            snapshot.item, proposalFacts: model.proposalFacts,
                            effectProposalFacts: model.effectProposalFacts)
                        takesRecommendationOnReturn(drawn.pageCard(compact: horizontalSizeClass == .compact))
                            // A late facts fetch or a new item version
                            // changes the rows, so the footer follows them.
                            .onChange(of: identifiers, initial: true) {
                                seat.publishIdentifiers(itemID, identifiers)
                            }
                            .onChange(of: detailsRevealRequest) {
                                revealIdentifiersIfRequested(using: seat.scrollProxy)
                            }
                            .onAppear {
                                revealIdentifiersIfRequested(using: seat.scrollProxy)
                            }
                    } else {
                        ScrollViewReader { scrollProxy in
                            ScrollView {
                                drawn.detailCard(
                                    compact: horizontalSizeClass == .compact, topMargin: topMargin)
                            }
                            .coordinateSpace(name: Self.scrollSpace)
                            .onChange(of: detailsRevealRequest) {
                                revealIdentifiersIfRequested(using: scrollProxy)
                            }
                            .onAppear {
                                revealIdentifiersIfRequested(using: scrollProxy)
                            }
                        }
                        .navigationTitle(AttentionDisplay.title(snapshot.item))
                    }
                } else if seat == nil {
                    UnavailableStateView(
                        glyph: .inbox, title: "Item unavailable",
                        description: "This attention item is not in the list.",
                        seat: .detailColumn
                    )
                    .navigationTitle("Decision")
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
            .sheet(
                item: $messageEditor, onDismiss: { discussAttachments = TaskAttachmentDraft(for: .message) }
            ) { editor in
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
                        },
                        attachments: model.acceptsDiscussAttachments ? $discussAttachments : nil
                    ) { message, _ in
                        await model.submitDiscuss(message: message, attachments: discussAttachments.items)
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
            .confirmationDialog(
                "Choose another agent",
                isPresented: alternateAgentRetryIsPresented,
                titleVisibility: .visible
            ) {
                if let reviewedSnapshot = alternateAgentRetrySnapshot {
                    ForEach(
                        AgentChoiceDisplay.offerOptions(
                            reviewedSnapshot.item.alternate_agents ?? [], pools: usagePools, now: Date())
                    ) { option in
                        Button(option.label) {
                            alternateAgentRetrySnapshot = nil
                            Task {
                                await model.submitAlternateAgentRetry(
                                    choice: option.choice, reviewedSnapshot: reviewedSnapshot)
                            }
                        }
                    }
                }
                Button("Cancel", role: .cancel) { alternateAgentRetrySnapshot = nil }
            } message: {
                Text("The daemon checks the agent again before the retry runs.")
            }
            .sheet(item: $specApprovalReader) { reader in
                if let item = model.snapshot?.item {
                    specApprovalReaderSheet(reader, item: item)
                }
            }
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
                alternateAgentRetrySnapshot = nil
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
                //
                // On a task page the handler sits on the card alone: Return
                // on a one-line row or a task control must not decide this
                // card.
                .onKeyPress(.return) { seat == nil ? takeRecommendationOnReturn() : .ignored }
            #endif
        }
    }

    @ViewBuilder
    private func takesRecommendationOnReturn<Content: View>(_ content: Content) -> some View {
        #if os(macOS)
            content.onKeyPress(.return) { takeRecommendationOnReturn() }
        #else
            content
        #endif
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

    /// Opens the card's identifiers where they draw: the page's footer for
    /// a seated card, the card's own Identifiers fold otherwise.
    private func revealIdentifiersIfRequested(using scrollProxy: ScrollViewProxy) {
        guard let detailsRevealRequest, detailsRevealRequest.itemID == itemID else { return }
        if let seat {
            seat.revealIdentifiers()
        } else {
            detailsExpanded.wrappedValue = true
            withAnimation {
                scrollProxy.scrollTo(ScrollTarget.identifiers, anchor: .top)
            }
        }
        model.emitDetailsOpenedBeforeActing()
        consumeDetailsRevealRequest(detailsRevealRequest.nonce)
    }

    /// Hosts the detail's content; on iOS, with the sticky recommended
    /// action under it.
    @ViewBuilder
    private func platformBody<Content: View>(
        @ViewBuilder _ content: @escaping () -> Content
    ) -> some View {
        #if os(iOS)
            if seat == nil {
                content().safeAreaInset(edge: .bottom, spacing: 0) { stickyRecommendation }
            } else {
                content().preference(key: TaskPageFooterKey.self, value: stickyRecommendation)
            }
        #else
            content()
        #endif
    }

    #if os(iOS)
        /// The recommended action pinned under the scroll view while its own
        /// button is scrolled away. The same ranking gate the card's own
        /// recommendation block uses: the served action surface decides what
        /// this client may submit, so a stored recommendation the surface no
        /// longer ranks must not reappear as a footer button once the block
        /// itself has stopped rendering (#1107 review).
        private var stickyRecommendation: AnyView? {
            guard let item = model.snapshot?.item,
                let recommendation = DecisionRecommendationPresentation.of(item),
                actionRanking(item).recommended == recommendation.action,
                !recommendationVisible
            else { return nil }
            return AnyView(
                actionButton(
                    recommendation.action,
                    item: item,
                    tone: recommendationTone(recommendation.action, item: item),
                    showsIcon: false
                )
                .padding(.horizontal)
                .padding(.vertical, 10)
                .background(.bar))
        }
    #endif

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

    private var alternateAgentRetryIsPresented: Binding<Bool> {
        Binding(
            get: { alternateAgentRetrySnapshot != nil },
            set: { presented in
                if !presented { alternateAgentRetrySnapshot = nil }
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
        onPage: Bool,
        actionRegionFrameChanged: ((CGRect) -> Void)? = nil
    ) -> some View {
        let composition = DecisionCardComposition.forType(item._type)
        let register = unverified(
            item,
            effectProposalFacts: effectProposalFacts,
            rendersInteractiveControls: rendersInteractiveControls)
        let modules = CardModules(
            item: item,
            composition: composition,
            proposalFacts: proposalFacts,
            effectProposalFacts: effectProposalFacts,
            register: register,
            accessibilityLayout: accessibilityLayout,
            onPage: onPage)
        VStack(alignment: .leading, spacing: CardScale.sectionGap) {
            VStack(alignment: .leading, spacing: CardScale.headGap) {
                eyebrow(item, register: register, accessibilityLayout: accessibilityLayout)
                banner(item)
                // A closed card asks nothing: the ask's face says what
                // happened instead (#2097).
                if let lead = ConclusionDisplay.leadSentence(item) {
                    Text(lead)
                        .font(FreesideFont.ask)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                        .proseMeasure()
                } else if DecisionCardComposition.rendersAsk(for: item) {
                    Text(AttentionDisplay.ask(item))
                        .font(FreesideFont.ask)
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                        .proseMeasure()
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
                    .proseMeasure()
                    .frame(maxWidth: .infinity, alignment: .leading)
            }

            outcome(item, rendersInteractiveControls: rendersInteractiveControls)

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
                    rendersInteractiveControls: rendersInteractiveControls
                )
                // Seated in a task page, the thread is where the
                // outline's Conversation chip lands, and a message arriving
                // in it moves the controls under it.
                .background(alignment: .top) { TaskPageConversationAnchor() }
                .background(alignment: .bottom) { pageHoldMarker?("thread") }
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
            cardColumn(
                modules,
                stackedLayout: stackedLayout,
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
        /// Whether the card sits on a task page, whose Identifiers footer
        /// takes the card's identifier rows. Explicit rather than read from
        /// the seat, because a screenshot draws the page's card with none.
        let onPage: Bool
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
                accessibilityLayout: modules.accessibilityLayout)
        }
    }

    /// The card's one column, at any pane width: the modules a decision
    /// rests on, the Evidence fold, the control group, and the hairline's
    /// folds (#2607).
    @ViewBuilder
    private func cardColumn(
        _ modules: CardModules,
        stackedLayout: Bool,
        actionRegionFrameChanged: ((CGRect) -> Void)?
    ) -> some View {
        let item = modules.item
        let evidenceClaims = modules.composition.evidenceClaims(
            from: DecisionCardComposition.listedClaims(item), prominentClaimIndex: graphics.prominentClaimIndex,
            on: DecisionCardComposition.UnverifiedContext.currentPlatform)
        let identifierRows = Self.identifierRows(
            item, proposalFacts: modules.proposalFacts,
            effectProposalFacts: modules.effectProposalFacts)
        let recordedContext = recordedContext(modules)
        let foot = DecisionCardFoot(
            evidenceClaims: evidenceClaims,
            attachments: item.evidence_snapshot.count,
            recordedContext: recordedContext,
            identifierRows: identifierRows,
            onPage: modules.onPage)
        cardModules(modules.composition.leadModules, modules)
        if foot.aboveActions.contains(.evidence) {
            evidenceFold(modules, claims: evidenceClaims)
        }
        controlGroup(
            modules,
            stackedLayout: stackedLayout,
            actionRegionFrameChanged: actionRegionFrameChanged)
        if !foot.underHairline.isEmpty {
            hairlineFolds(
                modules, foot: foot, recordedContext: recordedContext, identifierRows: identifierRows)
        }
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
                if let sentence = controlGroupReceipt(item) {
                    receipt(.neutral, "Recorded", sentence)
                }
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
            // Under the validation row, which every sync round draws and
            // takes away again, so a page reader below it holds still.
            .background(alignment: .bottom) { pageHoldMarker?("controls") }
        } else if hasReviewingAction {
            // What the group holds between its controls is content, such as
            // a change summary, and outlives the controls on a closed card.
            cardModules(composition.controlGroupModules, modules)
        }
    }

    /// A closed card's Outcome facts and the link to the task timeline,
    /// where what followed the close is told (#2097). An item closed
    /// before conclusions existed has no facts and draws only the link.
    @ViewBuilder
    private func outcome(
        _ item: Components.Schemas.AttentionItem, rendersInteractiveControls: Bool
    ) -> some View {
        TimeFormatEnvironment { locale, timeZone in
            let facts = ConclusionDisplay.facts(
                item, thisDeviceID: model.deviceID, now: now, locale: locale, timeZone: timeZone)
            if !facts.isEmpty {
                factSection("Outcome") {
                    ForEach(facts) { fact in
                        factRow(fact)
                    }
                }
            }
        }
        if item.status != .open, let taskID = ConclusionDisplay.timelineTaskID(for: item, in: tasks) {
            let link = FreesideLink(title: "Open task page", face: FreesideFont.noticeAction)
            if rendersInteractiveControls {
                Button {
                    onSelectTask(taskID)
                } label: {
                    link.contentShape(Rectangle())
                }
                .buttonStyle(.plain)
            } else {
                link
            }
        }
    }

    /// The item when it is closed, whose route lists draw inert with the
    /// applied route outlined (#2097).
    private var closedItem: Components.Schemas.AttentionItem? {
        guard let item = model.snapshot?.item, item.status != .open else { return nil }
        return item
    }

    /// The card's one hairline (R26) and the folds under it: Recorded
    /// Context and, on a card with no page, its own Identifiers.
    private func hairlineFolds(
        _ modules: CardModules,
        foot: DecisionCardFoot,
        recordedContext: DecisionRecordedContext,
        identifierRows: [AttentionDisplay.BindingRow]
    ) -> some View {
        VStack(alignment: .leading, spacing: CardScale.foldGap) {
            if foot.underHairline.contains(.recordedContext) {
                recordedContextFold(modules, content: recordedContext)
            }
            if foot.underHairline.contains(.identifiers) {
                TechnicalDetailsSection(
                    rows: identifierRows,
                    summary: Self.identifiersSummary(modules.item, rows: identifierRows),
                    rendersInteractiveControls: modules.register.rendersInteractiveControls,
                    isExpanded: detailsExpanded,
                    rowIndent: FootScale.rowIndent
                )
                .id(ScrollTarget.identifiers)
            }
        }
        .padding(.top, CardScale.foldLead)
        .overlay(alignment: .top) {
            Color.rule.frame(height: 1)
        }
    }

    /// The agent's claims and the evidence packet, closed by default,
    /// directly above the control group: what the decision rests on, one
    /// disclosure away. A fold that holds claims keeps the unverified
    /// register as its first line and says it aloud, since the sentence
    /// label carries none.
    private func evidenceFold(
        _ modules: CardModules, claims: [Components.Schemas.AgentClaim]
    ) -> some View {
        let item = modules.item
        let register = modules.register.withoutInfo
        let attachmentCount = item.evidence_snapshot.count
        return footFold(
            "Evidence",
            summary: Self.evidenceSummary(claims: claims.count, attachments: attachmentCount),
            spokenLabel: Self.evidenceSpokenLabel(claims: claims.count, attachments: attachmentCount),
            isExpanded: evidenceExpanded,
            unverified: claims.isEmpty ? nil : register
        ) {
            claimRows(claims, unverified: register, boxed: true)
            ForEach(item.evidence_snapshot, id: \.id) { artifact in
                AttachmentRow(
                    label: artifact._type.rawValue,
                    digest: artifact.digest,
                    reference: AttachmentReference(artifact.metadata),
                    attachments: attachments,
                    loadsAttachments: loadsAttachments,
                    rendersInteractiveControls: register.rendersInteractiveControls,
                    boxed: true)
            }
        }
        .foregroundStyle(Color.inkDim)
        .textSelection(.enabled)
    }

    /// What the card's Recorded Context holds, read from the same inputs
    /// its modules draw from.
    private func recordedContext(_ modules: CardModules) -> DecisionRecordedContext {
        let item = modules.item
        let composition = modules.composition
        func places(_ module: DecisionCardModule, in range: Range<Int>) -> Bool {
            composition.modules[range].contains(module)
        }
        let summaries = composition.summaries(from: item.agent_claims)
        let summaryFolds = places(.summary, in: composition.supportingModules)
        return DecisionRecordedContext(
            hasReason: DecisionCardComposition.reason(for: item) != nil,
            hasRunDetails: places(.foldedFacts, in: composition.foldedModules)
                // Only which rows fold is read here, which no reading's
                // wording changes, so the process locale and zone serve.
                && !DecisionFactPlacement(
                    item, includesCommitPlan: false, now: now, pools: usagePools, locale: .current,
                    timeZone: .current
                ).folded.isEmpty,
            hasReviewYield: places(.yieldChart, in: composition.foldedModules)
                && (graphics.diminishingYield != nil || DecisionYieldPresentation(item) != nil),
            readingRows: Self.readingRows(item, unavailable: actionRanking(item).unavailable),
            hasFactBlock: places(.factBlock, in: composition.supportingModules) && drawsFactBlock,
            hasAgentSummary: summaryFolds
                && (item._type == .ready_for_final_review || !summaries.isEmpty),
            hasReport: summaryFolds && !summaries.isEmpty)
    }

    /// Whether the fact block module has anything to draw.
    private var drawsFactBlock: Bool {
        graphics.changeSummary != nil || graphics.attemptTimings != nil
            || graphics.comparison?.verifiableFacts.isEmpty == false
    }

    /// The card's one reading fold under the hairline, flat, with no fold
    /// inside it but a summary's report: the daemon's reason in full on
    /// every type (plan §9 has the card's details carry it, wherever the
    /// face draws it), the folded modules, the reading rows, and the
    /// modules that support the decision from below the actions.
    private func recordedContextFold(
        _ modules: CardModules, content: DecisionRecordedContext
    ) -> some View {
        let item = modules.item
        let register = modules.register.withoutInfo
        // No module in here takes the card's explanation control, so on a
        // card with nowhere else to carry it the fold explains itself when
        // it holds agent words.
        let holdsAgentWords =
            DecisionCardComposition.reason(for: item)?.isAgentWritten == true
            || content.hasAgentSummary
            || (content.hasFactBlock && graphics.changeSummary != nil)
        return footFold(
            "Recorded Context",
            summary: content.summary,
            isExpanded: disclosure(.recordedContext),
            spacing: CardScale.moduleGap
        ) {
            if holdsAgentWords {
                foldedUnverifiedSentence(register)
            }
            detailsReason(item, register: register)
            cardModules(modules.composition.foldedModules, modules)
            if !content.readingRows.isEmpty {
                FactGroup(spacing: 6) {
                    ForEach(Array(content.readingRows.enumerated()), id: \.offset) { _, row in
                        factRow(row.label, value: row.value)
                    }
                }
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
                .textSelection(.enabled)
            }
            cardModules(modules.composition.supportingModules, modules)
        }
    }

    /// Whether the card has a control group to draw: an action
    /// the item requests, or the agent claims the Mac sets beside them.
    static func drawsControlGroup(_ item: Components.Schemas.AttentionItem) -> Bool {
        // A closed item takes no command, so its card keeps no group of
        // disabled actions (#2097).
        guard item.status == .open else { return false }
        return !item.requested_decision.isEmpty
            || (!DecisionCardComposition.forType(item._type).drawsLeadClaimsInCard
                && !DecisionCardComposition.actionRegionClaims(item.agent_claims).isEmpty)
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
        accessibilityLayout: Bool
    ) -> some View {
        let rendersInteractiveControls = register.rendersInteractiveControls
        switch module {
        case .foldedFacts:
            // Drawn inside Recorded Context, which is already the fold, so
            // the rows sit under their keyword rather than a fold of their
            // own (#2607).
            TimeFormatEnvironment { locale, timeZone in
                let placement = DecisionFactPlacement(
                    item, includesCommitPlan: false, now: now, pools: usagePools, locale: locale,
                    timeZone: timeZone)
                if !placement.folded.isEmpty {
                    factSection(DecisionFactPlacement.foldedTitle) {
                        ForEach(placement.folded) { fact in
                            factRow(fact)
                        }
                    }
                }
            }
        case .facts:
            factsSection(
                item,
                includesCommitPlan: !composition.modules.contains(.checklist),
                drawsFold: !composition.modules.contains(.foldedFacts))
            if let proposalFacts {
                factSection("Facts") {
                    proposalRows(proposalFacts)
                }
                if let prior = proposalFacts.supersedes?.value1 {
                    factSection("Revision Context") {
                        proposalRevisionRows(prior)
                    }
                }
            }
            // A closure or an adoption states what approving binds as the
            // card's statement (frame 7.9), ahead of the facts that qualify
            // it.
            if let effectProposalFacts,
                let binding = AttentionDisplay.effectBinding(effectProposalFacts)
            {
                Text(
                    "\(binding.lead)\(Text(binding.head).font(FreesideFont.monoValue))\(binding.outcome)"
                )
                .font(FreesideFont.statement)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
                .proseMeasure()
                .accessibilityElement(children: .ignore)
                .accessibilityAddTraits(.isStaticText)
                .accessibilityLabel(Text(binding.plain))
            }
            // effectProposalCardRows is empty for an effect kind this card
            // has no rows for (it draws a closure, a follow-up filing, and an
            // image-policy adoption),
            // so the titled section is suppressed rather than drawn empty.
            if let effectProposalFacts,
                case let effectRows = AttentionDisplay.effectProposalCardRows(effectProposalFacts),
                !effectRows.isEmpty
            {
                factSection("Facts") {
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
                    showsBars: graphics.diminishingYield != nil)
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
            if let findings = item.review_dispute?.value1.findings, !findings.isEmpty {
                disputedFindingCards(findings, register: register)
            }
        case .factBlock:
            factBlocks(item, register: register)
        case .summary:
            agentSummary(item, register: register)
        case .reviewGuide:
            if let guide = item.review_guide?.value1 {
                DecisionReviewGuideView(
                    guide: guide,
                    producerInvocationID: DecisionCardComposition.reviewGuideClaim(item)
                        .map(Self.producerInvocationID),
                    carriesInfo: register.carriesInfo,
                    rendersInteractiveControls: rendersInteractiveControls,
                    openAtStop: DecisionCardComposition.opensReviewStops(
                        item, ranking: actionRanking(item))
                        ? { path, line in Task { await model.openPullRequest(at: path, line: line) } }
                        : nil,
                    actionsEnabled: model.actionsEnabled && model.isSubmittable(.open_pr))
            }
        case .claims:
            // A claims module under the actions lists its claims in the
            // Evidence fold instead (`evidenceClaims`).
            if composition.claimsAreProminent(at: moduleIndex) {
                claims(
                    composition.claimsInCard(
                        from: DecisionCardComposition.listedClaims(item),
                        at: moduleIndex,
                        prominentClaimIndex: graphics.prominentClaimIndex,
                        on: DecisionCardComposition.UnverifiedContext.currentPlatform),
                    title: DecisionCardComposition.leadClaimsKeyword(for: item._type),
                    unverified: register)
            }
        case .evidence, .details:
            // The card's foot draws these on both platforms: Evidence above
            // the actions, the identifiers in Identifiers.
            EmptyView()
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
                                        .accessibilityElement(children: .ignore)
                                        .accessibilityAddTraits(.isStaticText)
                                        .accessibilityLabel("Excerpt ends here")
                                }
                                // Headings never repeat in a recognized report.
                                ForEach(presentation.concerns ?? [], id: \.label) { part in
                                    KeywordLabel(text: part.label)
                                    DecisionSummaryText(blocks: part.blocks)
                                }
                            }
                            .proseMeasure()
                        }
                        if presentation.concernsUnknown {
                            Text(
                                "Concerns have not been extracted; read the full report. Concerns may be outside this excerpt."
                            )
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.inkDim)
                            .fixedSize(horizontal: false, vertical: true)
                            .proseMeasure()
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
                label: "Original report", digest: claim.digest, reference: AttachmentReference(claim.metadata),
                attachments: attachments,
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
                    .proseMeasure()
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// The reason in Recorded Context, which carries it in full on every
    /// type whether or not the card draws it anywhere else (plan §9
    /// revision 82).
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
    /// specification approval's), which Recorded Context draws as the card
    /// draws the summary and in full; every other reason is a sentence as
    /// written.
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
        .proseMeasure()
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
        // The Other Agents row prints reading times, so the facts are built
        // with the locale and zone installed where they draw.
        TimeFormatEnvironment { locale, timeZone in
            let placement = DecisionFactPlacement(
                item, includesCommitPlan: includesCommitPlan, now: now, pools: usagePools,
                locale: locale, timeZone: timeZone)
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
                factSection(DecisionCardComposition.factsKeyword(for: item._type)) {
                    ForEach(rows) { fact in
                        factRow(fact)
                    }
                }
            }
            if drawsFold {
                foldedFacts(placement)
            }
        }
    }

    /// The routine facts behind their disclosure, above the actions.
    @ViewBuilder
    private func foldedFacts(_ placement: DecisionFactPlacement) -> some View {
        if !placement.folded.isEmpty {
            SentenceDisclosure(
                label: DecisionFactPlacement.foldedTitle,
                isExpanded: disclosure(.runDetails)
            ) {
                FactGroup(spacing: 8) {
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
            factSection("Daemon Facts") {
                ForEach(comparison.verifiableFacts) { fact in
                    FactRow(label: fact.label, value: fact.value)
                }
            }
        }

        if let attemptTimings = graphics.attemptTimings {
            factSection(attemptTimings.title) {
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
                        .proseMeasure()
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
            let priorVersion = Self.priorSpecificationVersion(in: item)
        {
            DecisionChangeRow(specification: revision.diff, sinceVersion: priorVersion)
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
            // A closed item takes no reply, so its thread reads without one.
            let offersReply =
                item.status == .open
                && item.requested_decision.contains(.discuss)
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
                rendersInteractiveControls: rendersInteractiveControls
            )
            .background(alignment: .top) { TaskPageConversationAnchor() }
            .background(alignment: .bottom) { pageHoldMarker?("thread") }
        }
    }

    /// The specification as one bordered item under its keyword (frame
    /// 5.1): which version the daemon bound to this approval, and the
    /// links that open it. A revision's diff is a second link on the same
    /// item, not a second item (frame 7.2).
    @ViewBuilder
    private func specificationMaterial(
        _ item: Components.Schemas.AttentionItem,
        rendersInteractiveControls: Bool
    ) -> some View {
        if let specification = Self.specificationClaim(in: item) {
            let title =
                Self.specificationVersion(in: item).map { "Version \($0)" }
                ?? "Specification"
            let priorVersion = Self.priorSpecificationVersion(in: item)
            keywordSection("Specification") {
                VStack(alignment: .leading, spacing: 11) {
                    if specification.text != nil {
                        ViewThatFits(in: .horizontal) {
                            HStack(alignment: .center, spacing: 12) {
                                specificationItemTitle(title)
                                Spacer(minLength: 8)
                                specificationLinks(
                                    priorVersion: priorVersion,
                                    rendersInteractiveControls: rendersInteractiveControls)
                            }
                            VStack(alignment: .leading, spacing: 11) {
                                specificationItemTitle(title)
                                specificationLinks(
                                    priorVersion: priorVersion,
                                    rendersInteractiveControls: rendersInteractiveControls)
                            }
                        }
                    } else {
                        AttachmentRow(
                            label: title,
                            digest: specification.digest,
                            reference: AttachmentReference(specification.metadata),
                            attachments: attachments,
                            loadsAttachments: loadsAttachments,
                            rendersInteractiveControls: rendersInteractiveControls)
                        if let priorVersion {
                            readerLink(
                                "Open Diff", reader: .diff,
                                accessibilityLabel: "Open diff from version \(priorVersion)",
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
        Text(title)
            .font(FreesideFont.factLabel)
            .foregroundStyle(Color.ink)
    }

    private func specificationLinks(
        priorVersion: Int?,
        rendersInteractiveControls: Bool
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 16) {
            readerLink(
                "Open Reader", reader: .specification,
                accessibilityLabel: "Open specification reader",
                rendersInteractiveControls: rendersInteractiveControls)
            if let priorVersion {
                readerLink(
                    "Open Diff", reader: .diff,
                    accessibilityLabel: "Open diff from version \(priorVersion)",
                    rendersInteractiveControls: rendersInteractiveControls)
            }
        }
    }

    static func specificationVersion(
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

    static func priorSpecificationVersion(
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
                    description: "This is the first version of the specification.")
            }
        }
    }

    /// The reader sheet's header: the keyword and revision chip over the
    /// reader's title.
    private func specApprovalReaderSheetHeader(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem
    ) -> some View {
        FreesideSheetHeader(
            eyebrow: reader.keyword,
            chip: SpecApprovalReader.versionChip(Self.specificationVersion(in: item)),
            ask: reader == .specification ? "Specification" : "Specification changes")
    }

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
        #if os(macOS)
            // A Mac sheet takes its content's ideal size, and the reader's
            // scroll view has none. The width the card's other long sheets
            // take, and the reader's own 72-character measure fits in it.
            .frame(minWidth: 480, idealWidth: 720, minHeight: 420, idealHeight: 640)
        #endif
    }

    @ViewBuilder
    private func claims(
        _ claims: [Components.Schemas.AgentClaim],
        title: String,
        unverified: UnverifiedRegister
    ) -> some View {
        if !claims.isEmpty {
            cardSection(title, unverified: unverified) {
                claimRows(claims, unverified: unverified)
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
                    reference: AttachmentReference(claim.metadata),
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
                .proseMeasure()
        }
        SentenceDisclosure(
            label: "Source and Supporting Details",
            isExpanded: disclosure(.claimSource(claim))
        ) {
            FactGroup(spacing: 6) {
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

    @ViewBuilder
    private func proposalRows(_ facts: Components.Schemas.TaskProposalFactsSnapshot) -> some View {
        factRow("Intent", value: facts.intent.rawValue)
        factRow("Expected Cost", value: "\(facts.expected_cost_units) units")
        factRow("Components", value: "\(facts.scope.component_count)")
        factRow("Declared Paths", value: "\(facts.scope.declared_path_count)")
        factRow("Control Plane", value: facts.scope.touches_control_plane ? "Yes" : "No")
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
    /// The card as the task page seats it, for a screenshot of the page.
    func screenshotPageCard(
        _ item: Components.Schemas.AttentionItem,
        at dynamicTypeSize: DynamicTypeSize,
        compactLayout: Bool = false
    ) -> some View {
        card(
            item,
            proposalFacts: nil,
            effectProposalFacts: nil,
            rendersInteractiveControls: false,
            accessibilityLayout: dynamicTypeSize >= .accessibility1,
            compactLayout: compactLayout,
            onPage: true
        )
        .pageCard(compact: compactLayout)
    }

    func screenshotCard(
        _ item: Components.Schemas.AttentionItem,
        at dynamicTypeSize: DynamicTypeSize,
        proposalFacts: Components.Schemas.TaskProposalFactsSnapshot? = nil,
        effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot? = nil,
        compactLayout: Bool = false,
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
            onPage: false,
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
                failureMessage: "the daemon did not answer", isUnsent: false)
            lostResponseRetry
        }
        .padding()
        // The card's own ground: the disclosure under the notice has no
        // wash of its own to carry its ink over the harness's light canvas.
        .background(Color.ground)
    }

    /// A reader's content as its sheet draws it, with the sheet's header
    /// and fixed Done footer.
    func screenshotSpecApprovalReader(
        _ reader: SpecApprovalReader,
        item: Components.Schemas.AttentionItem,
        expandsTechnicalDetails: Bool = false,
        expandsLaterHunks: Bool = false
    ) -> some View {
        VStack(spacing: 0) {
            specApprovalReaderSheetHeader(reader, item: item)
            specApprovalReaderContent(
                reader,
                item: item,
                rendersScrollableContent: false,
                expandsTechnicalDetails: expandsTechnicalDetails,
                expandsLaterHunks: expandsLaterHunks
            )
            .padding()
            .frame(maxWidth: .infinity, alignment: .topLeading)
            FreesideSheetActionRow.done {}
        }
        .background(Color.ground2)
    }

    // One item per finding (plan §9 revision 78, visual audit D09, handoff
    // frame 7.1). An item shows its finding's exact message, the route
    // under its keyword with who proposed it, and the reason, clamped
    // (#2608); everything else about that finding, the route list
    // included, sits in the item's own disclosure. Nothing drawn
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
        // The first route keyword that says "(unverified)" carries the
        // card's explanation control; the rest draw the keyword alone.
        let infoCardID = FindingCardPresentation.cards(binding).first(where: \.modelBacked)?.id
        let reasonLineLimit = FindingCardPresentation.reasonLineLimit(
            findingCount: binding.proposals.count)
        // The findings are one module, so they stand a module gap apart
        // rather than a section gap, which keeps two realistic findings
        // above the actions in the first viewport (#1141).
        VStack(alignment: .leading, spacing: CardScale.moduleGap) {
            ForEach(Array(binding.proposals.enumerated()), id: \.element.finding_id) {
                index, proposal in
                let card = FindingCardPresentation(proposal, number: index + 1, binding: binding)
                VStack(alignment: .leading, spacing: 8) {
                    // The heading runs in with the message's title instead of
                    // standing on a line of its own: a line per finding is
                    // what two realistic findings have to spare inside the
                    // first viewport (#1141). The head speaks the whole
                    // message, so the body under it is not read twice.
                    findingHead(card)
                        .fixedSize(horizontal: false, vertical: true)
                        .proseMeasure()
                        .accessibilityElement(children: .ignore)
                        .accessibilityLabel(Text(card.messageAccessibilityLabel))
                        .accessibilityAddTraits(.isHeader)
                    if !card.body.isEmpty {
                        DecisionSummaryText(blocks: card.body, size: .body)
                            .proseMeasure()
                            .accessibilityHidden(true)
                    }
                    VStack(alignment: .leading, spacing: 4) {
                        // The keyword, the route, and its reason are one
                        // statement by one producer, so they share its voice.
                        findingVoice(card) {
                            findingRouteLabel(
                                card,
                                register: card.id == infoCardID ? register : register.withoutInfo)
                            Text(card.route)
                                .font(FreesideFont.optionLabel)
                                .foregroundStyle(Color.ink)
                                .fixedSize(horizontal: false, vertical: true)
                                .accessibilityElement(children: .ignore)
                                .accessibilityAddTraits(.isStaticText)
                                .accessibilityLabel(Text(card.routeAccessibilityLabel))
                            ClampedText(
                                text: card.rationale, font: FreesideFont.cardBody,
                                lineLimit: reasonLineLimit,
                                isExpanded: findingReasonExpansion(card.id, itemID: itemID),
                                spokenName: card.reasonSpokenName
                            )
                            .proseMeasure()
                        }
                        if let selected = alternativeSelections[card.id] {
                            Text(FindingCardPresentation.selectionNotice(selected))
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.ink)
                                .fixedSize(horizontal: false, vertical: true)
                                .accessibilityElement(children: .ignore)
                                .accessibilityAddTraits(.isStaticText)
                                .accessibilityLabel(
                                    Text(card.selectionAccessibilityLabel(selected)))
                        }
                    }
                    SentenceDisclosure(
                        label: FindingCardPresentation.disclosureTitle,
                        summary: card.disclosureSummary,
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
        guard let title = card.title else { return heading }
        return heading
            + Text(
                AttributedString("  ")
                    + DecisionSummaryText.inline(
                        title, size: .body, emphasisSize: findingEmphasisSize)
            )
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.ink)
    }

    /// The route's fixed keyword, in the unverified register where a model
    /// judged (visual audit D03), with who proposed it trailing as a compact
    /// mark (R21) while the line holds both, and under it where it does not.
    /// The mark's hover help is the full producer sentence, which the route
    /// also speaks.
    @ViewBuilder
    private func findingRouteLabel(
        _ card: FindingCardPresentation,
        register: UnverifiedRegister
    ) -> some View {
        let keyword = Group {
            if card.modelBacked {
                UnverifiedLabel(
                    text: AttentionDisplay.AdjudicationProducerPresentation.keyword,
                    carriesInfo: register.carriesInfo,
                    rendersInteractiveControls: register.rendersInteractiveControls)
            } else {
                KeywordLabel(text: AttentionDisplay.AdjudicationProducerPresentation.keyword)
            }
        }
        let mark = CompactMark(text: card.producer.mark)
            .help(card.producer.sentence)
            .accessibilityHidden(true)
        ViewThatFits(in: .horizontal) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                keyword
                Spacer(minLength: 0)
                mark
            }
            VStack(alignment: .leading, spacing: 4) {
                keyword
                mark
            }
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
            // The proposal's own assessment of itself, so a model's stays in
            // its quote, as it did beside the rationale before the rationale
            // moved to the face.
            findingVoice(card) {
                Text(card.qualities)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
                    .proseMeasure()
            }
            findingStatements(
                "Evidence", card.evidence, card: card,
                rendersInteractiveControls: rendersInteractiveControls)
            // The finding's coordinates are daemon-authenticated, so they
            // keep their own register inside the disclosure, never mixed
            // into the producer's content around them (#892).
            factSection("Daemon Facts") {
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
                    if let closed = closedItem {
                        // The list's values are optional so a close no
                        // decision routed outlines none.
                        ChoiceList<Components.Schemas.AdjudicationRoute?>(
                            accessibilityLabel: "Route for \(card.heading)",
                            options: card.routeOptions.map {
                                .init(
                                    value: $0.route, label: $0.label, consequence: $0.consequence,
                                    mark: $0.isProposed ? "Proposed" : nil,
                                    register: card.modelBacked ? .quote : .item)
                            },
                            selection: .constant(
                                ConclusionDisplay.appliedRoute(
                                    for: card.id, proposed: card.routeOptions.first { $0.isProposed }?.route,
                                    in: closed)),
                            isInert: true)
                    } else {
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
                            .proseMeasure()
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func findingReasonExpansion(_ findingID: String, itemID: String) -> Binding<Bool> {
        Binding(
            get: { findingReasonPreferences.isExpanded(findingID: findingID, itemID: itemID) },
            set: { findingReasonPreferences.setExpanded($0, findingID: findingID, itemID: itemID) })
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
                alternativeSelections = Self.selections(
                    alternativeSelections, picking: route, for: proposal)
            }
        )
    }

    /// The held picks after the operator picks `route` for a finding: a
    /// route other than the proposed one is held, and the proposed route
    /// clears the pick, so the fill returns to the acceptance (R6).
    static func selections(
        _ selections: [String: Components.Schemas.AdjudicationRoute],
        picking route: Components.Schemas.AdjudicationRoute,
        for proposal: Components.Schemas.FindingAdjudicationProposal
    ) -> [String: Components.Schemas.AdjudicationRoute] {
        var next = selections
        next[proposal.finding_id] = route == proposal.route ? nil : route
        return next
    }

    private func selectedAlternatives(
        _ binding: Components.Schemas.FindingAdjudicationBinding
    ) -> [Components.Schemas.AlternativeChoice] {
        Self.selectedAlternatives(binding, selections: alternativeSelections)
    }

    /// Whether the operator holds a route pick on the item's findings, which
    /// moves the card's fill to Choose Another Route (R6). It counts only
    /// picks on findings the binding lists, as the command sends them.
    private func holdsPick(_ item: Components.Schemas.AttentionItem) -> Bool {
        guard let binding = item.finding_adjudication?.value1 else { return false }
        return !selectedAlternatives(binding).isEmpty
    }

    // A routed dispute draws each finding in the shape of findingCards: a
    // bordered item whose face holds the severity and title, where the
    // finding is and who raised it, and the routes the operator picks from,
    // each with its consequence (R29). The model's reason and the review
    // identifiers fold away under the item (#2068). One keyword heads the
    // module, the card's one unverified marker outside the folds (R7) and,
    // by reading order, its one info control (R25).
    private func disputedFindingCards(
        _ findings: [Components.Schemas.DisputedFinding],
        register: UnverifiedRegister
    ) -> some View {
        VStack(alignment: .leading, spacing: CardScale.moduleGap) {
            UnverifiedLabel(
                text: "Findings", carriesInfo: register.carriesInfo,
                rendersInteractiveControls: register.rendersInteractiveControls)
            ForEach(Array(findings.enumerated()), id: \.element.finding_id) { index, finding in
                disputedFindingItem(
                    finding, number: index + 1, of: findings.count,
                    rendersInteractiveControls: register.rendersInteractiveControls)
            }
        }
    }

    private func disputedFindingItem(
        _ finding: Components.Schemas.DisputedFinding,
        number: Int,
        of total: Int,
        rendersInteractiveControls: Bool
    ) -> some View {
        let presentation = FindingPresentation(message: finding.message)
        let external = finding.external != nil
        return VStack(alignment: .leading, spacing: 8) {
            // The head speaks the whole message, so the body under it is
            // not read twice.
            disputedFindingHead(finding, title: presentation.title)
                .fixedSize(horizontal: false, vertical: true)
                .proseMeasure()
                .accessibilityElement(children: .ignore)
                .accessibilityLabel(
                    Text("\(FindingPresentation.severityLabel(finding.severity.rawValue)), \(finding.message)")
                )
                .accessibilityAddTraits(.isHeader)
            if !presentation.body.isEmpty {
                DecisionSummaryText(blocks: presentation.body, size: .body)
                    .proseMeasure()
                    .accessibilityHidden(true)
            }
            if let source = AttentionDisplay.disputedFindingSource(finding) {
                Text(source)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
            let routes: [ChoiceList<Components.Schemas.AdjudicationRoute?>.Option] =
                finding.offered_routes.map {
                    .init(
                        value: $0,
                        label: AttentionDisplay.disputeRouteLabel($0, external: external),
                        consequence: AttentionDisplay.disputeRouteConsequence($0, external: external),
                        mark: $0 == finding.adjudication?.lean?.value1
                            ? AttentionDisplay.disputedFindingFavoredMark : nil,
                        register: .item)
                }
            if let closed = closedItem {
                ChoiceList(
                    accessibilityLabel: "Route for \(finding.message)",
                    options: routes,
                    selection: .constant(
                        ConclusionDisplay.appliedRoute(for: finding.finding_id, proposed: nil, in: closed)),
                    isInert: true)
            } else {
                ChoiceList(
                    accessibilityLabel: "Route for \(finding.message)",
                    options: routes,
                    selection: Binding(
                        get: { alternativeSelections[finding.finding_id] },
                        set: { alternativeSelections[finding.finding_id] = $0 })
                )
                .disabled(!model.actionsEnabled)
                if let route = alternativeSelections[finding.finding_id],
                    AttentionDisplay.disputeRouteRepliesExternally(route, external: external)
                {
                    replyReasonField(
                        finding.finding_id, number: number,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
            }
            SentenceDisclosure(
                label: "Reason and Source",
                summary: AttentionDisplay.disputedFindingReasonSummary(finding),
                spokenLabel: "Finding \(number) reason and source",
                isExpanded: findingExpansion(finding.finding_id)
            ) {
                disputedFindingReason(finding, rendersInteractiveControls: rendersInteractiveControls)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
        // The ordinal left the face; VoiceOver still names the item.
        .accessibilityElement(children: .contain)
        .accessibilityLabel(Text("Finding \(number) of \(total)"))
    }

    /// The reason a Decline and Reply or Defer and Reply posts in the
    /// reviewer's thread. The daemon refuses either route without one, so
    /// Apply waits for it (`findingRoutesReady`).
    @ViewBuilder private func replyReasonField(
        _ findingID: String,
        number: Int,
        rendersInteractiveControls: Bool
    ) -> some View {
        let draft = findingReasons[findingID] ?? ""
        let placeholder = AttentionDisplay.disputeReplyReasonPlaceholder
        VStack(alignment: .leading, spacing: 4) {
            Group {
                if rendersInteractiveControls {
                    TextField(
                        placeholder,
                        text: Binding(
                            get: { findingReasons[findingID] ?? "" },
                            set: { findingReasons[findingID] = $0 }),
                        axis: .vertical
                    )
                    .lineLimit(2...6)
                    .textFieldStyle(.plain)
                    .disabled(!model.actionsEnabled)
                    .accessibilityLabel("Finding \(number) reply reason")
                } else {
                    // ImageRenderer cannot draw the AppKit-backed field, so the
                    // screenshot draws its text or prompt in the same frame.
                    Text(draft.isEmpty ? placeholder : draft)
                        .foregroundStyle(draft.isEmpty ? Color.inkDim : Color.ink)
                        .frame(maxWidth: .infinity, minHeight: 36, alignment: .topLeading)
                }
            }
            .font(FreesideFont.callout)
            .padding(8)
            .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
            Text(AttentionDisplay.disputeReplyReasonHint(draft))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    /// The severity chip with the message's title running in beside it.
    private func disputedFindingHead(
        _ finding: Components.Schemas.DisputedFinding,
        title: AttributedString?
    ) -> some View {
        let severity = finding.severity.rawValue
        let chip = FindingPresentation.severityCut(severity)
        return HStack(alignment: .firstTextBaseline, spacing: 8) {
            StateChip(
                label: FindingPresentation.severityLabel(severity), color: chip.cut.text,
                dashed: chip.dashed, cut: chip.cut)
            if let title {
                Text(DecisionSummaryText.inline(title, size: .body, emphasisSize: findingEmphasisSize, heading: true))
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.ink)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// The adjudicator's view of one disputed finding, in the model's
    /// unverified register, and the review identifiers: what the model
    /// favored and why, so the operator does not decide blind. It never
    /// selects a route.
    private func disputedFindingReason(
        _ finding: Components.Schemas.DisputedFinding,
        rendersInteractiveControls: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            if let adjudication = finding.adjudication {
                UnverifiedLabel(
                    text: "Adjudicator", carriesInfo: false,
                    rendersInteractiveControls: rendersInteractiveControls)
                QuoteBlock {
                    Text(adjudication.rationale)
                        .font(FreesideFont.cardBody)
                        .fixedSize(horizontal: false, vertical: true)
                        .proseMeasure()
                }
                ForEach(
                    [
                        AttentionDisplay.disputedFindingThreshold(adjudication),
                        AttentionDisplay.disputedFindingUnofferedFavorite(finding),
                    ].compactMap { $0 }, id: \.self
                ) { sentence in
                    Text(sentence)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                        .proseMeasure()
                }
            } else {
                Text(AttentionDisplay.disputedFindingNoAdjudication)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
                    .proseMeasure()
            }
            if let external = finding.external {
                Text("\(external.reviewer_login) · \(external.thread_id)")
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private func selectedFindingRoutes(
        _ findings: [Components.Schemas.DisputedFinding]
    ) -> [Components.Schemas.FindingRouteChoice] {
        Self.selectedFindingRoutes(findings, selections: alternativeSelections, reasons: findingReasons)
    }

    /// One choice per finding with an offered route picked. A route that
    /// replies in an external thread carries the trimmed reason when it is
    /// sendable; every other route carries none, whatever draft is kept.
    static func selectedFindingRoutes(
        _ findings: [Components.Schemas.DisputedFinding],
        selections: [String: Components.Schemas.AdjudicationRoute],
        reasons: [String: String] = [:]
    ) -> [Components.Schemas.FindingRouteChoice] {
        findings.compactMap { finding in
            guard let route = selections[finding.finding_id], finding.offered_routes.contains(route) else {
                return nil
            }
            let reason =
                AttentionDisplay.disputeRouteRepliesExternally(route, external: finding.external != nil)
                ? Components.Schemas.FindingRouteChoice.sendableReason(reasons[finding.finding_id] ?? "")
                : nil
            return .init(finding_id: finding.finding_id, route: route, reason: reason)
        }
    }

    /// Apply's input gate: every finding has an offered route, and every
    /// route that replies externally has a sendable reason.
    static func findingRoutesReady(
        _ findings: [Components.Schemas.DisputedFinding],
        selections: [String: Components.Schemas.AdjudicationRoute],
        reasons: [String: String]
    ) -> Bool {
        let choices = selectedFindingRoutes(findings, selections: selections, reasons: reasons)
        return !findings.isEmpty && choices.count == findings.count
            && zip(findings, choices).allSatisfy { finding, choice in
                !AttentionDisplay.disputeRouteRepliesExternally(choice.route, external: finding.external != nil)
                    || choice.reason != nil
            }
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
        if action == .adjudicate_findings {
            guard let findings = item.review_dispute?.value1.findings else { return false }
            return Self.findingRoutesReady(findings, selections: alternativeSelections, reasons: findingReasons)
        }
        guard action == .choose_alternative_route else { return true }
        guard let binding = item.finding_adjudication?.value1 else { return false }
        return !selectedAlternatives(binding).isEmpty
    }

    /// The card's identifier rows (#2607): `detailIdentifierRows` and the
    /// effect proposal's own exact values. The card's "Bound to" row
    /// abbreviates the head and base to eight characters, but the approval
    /// authorizes this exact prospective merge, so the identifiers keep the
    /// full head, base ref and SHA, and publication identity for audit and
    /// copy (two bases sharing an eight-character prefix are otherwise
    /// indistinguishable).
    static func identifierRows(
        _ item: Components.Schemas.AttentionItem,
        proposalFacts: Components.Schemas.TaskProposalFactsSnapshot?,
        effectProposalFacts: Components.Schemas.EffectProposalFactsSnapshot?
    ) -> [AttentionDisplay.BindingRow] {
        var rows = AttentionDisplay.detailIdentifierRows(
            item,
            priorProposalDigest: proposalFacts?.supersedes?.value1.proposal_digest
                ?? effectProposalFacts?.supersedes?.value1.proposal_digest,
            proposalDigest: proposalFacts?.proposal_digest ?? effectProposalFacts?.proposal_digest
        )
        if let merge = effectProposalFacts?.source_issue_closure?.value1.merge {
            rows.append(.init(label: "Bound Head", value: merge.candidate_head_sha))
            rows.append(.init(label: "Bound Base", value: "\(merge.base_ref)@\(merge.base_sha)"))
            rows.append(.init(label: "Publication Identity", value: merge.publication_identity))
        }
        if let effectProposalFacts {
            rows.append(contentsOf: AttentionDisplay.followUpFilingDetailRows(effectProposalFacts))
            rows.append(contentsOf: AttentionDisplay.imagePolicyAdoptionDetailRows(effectProposalFacts))
        }
        return rows
    }

    /// The card's reading rows, which Recorded Context draws under the
    /// reason: `detailReadingRows`, then each requested action this surface
    /// can't offer.
    static func readingRows(
        _ item: Components.Schemas.AttentionItem,
        unavailable: [Components.Schemas.Action]
    ) -> [AttentionDisplay.BindingRow] {
        AttentionDisplay.detailReadingRows(item) + AttentionDisplay.unavailableActionRows(unavailable)
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
        // The card's exact handles one secondary click away, as the task
        // eyebrow and each run header offer theirs (#2607).
        .copyMenu {
            ForEach(Self.eyebrowCopyItems(item), id: \.title) { copyItem in
                if let value = copyItem.values.first, copyItem.values.count == 1 {
                    Button(copyItem.title) { Clipboard.copy(value) }
                } else {
                    Menu(copyItem.title) {
                        ForEach(copyItem.values, id: \.self) { value in
                            Button(value) { Clipboard.copy(value) }
                        }
                    }
                }
            }
        }
    }

    /// One entry of the eyebrow's Copy menu: a single value is a button; a
    /// title with several values opens a menu that lists each.
    struct EyebrowCopyItem: Equatable {
        let title: String
        let values: [String]
    }

    /// The eyebrow's Copy menu, in the order the item row's context menu
    /// lists the same values: the item ID, then the evidence digest, one
    /// item per digest when there are several and none when there are none.
    static func eyebrowCopyItems(_ item: Components.Schemas.AttentionItem) -> [EyebrowCopyItem] {
        let digests = AttentionDisplay.uniqueEvidenceDigests(item)
        return [EyebrowCopyItem(title: "Copy item ID", values: [item.id])]
            + (digests.isEmpty ? [] : [EyebrowCopyItem(title: "Copy evidence digest", values: digests)])
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
            // A revised specification names its version beside the type
            // (frame 7.2); a first version has nothing to tell apart.
            if item._type == .spec_approval, Self.priorSpecificationVersion(in: item) != nil,
                let version = Self.specificationVersion(in: item)
            {
                StateChip(label: "Version \(version)", color: .inkDim)
            }
            if AttentionDisplay.showsLifecycleBadge(item.status) {
                StatusBadge(status: item.status)
            }
        }
    }

    @ViewBuilder
    private func banner(_ item: Components.Schemas.AttentionItem) -> some View {
        if model.phase == .superseded {
            receipt(
                .accent, "Superseded",
                "This item changed before your decision applied. Nothing was committed; re-review the replacement below."
            )
        } else {
            // A record-only command's receipt draws with the control group
            // instead, so the card never shows two receipts for one command.
            if let record = model.appliedRecord, controlGroupReceipt(item) == nil {
                // Success is quiet: a neutral wash, never green and never
                // the accent.
                receipt(
                    .neutral, "Recorded",
                    "Decision applied: \(AttentionDisplay.label(record.action, for: item))")
            }
            // The uncertain receipt outranks a failure: when a preserved
            // command may hold a recorded result, resending it is the
            // actionable step, whatever else failed.
            if offersLostResponseRetry {
                RetryableReceipt(
                    isExpanded: $lostResponseExpanded,
                    failureMessage: model.submissionError, isUnsent: model.unsentAfterFailedUpload)
                if !Self.drawsControlGroup(item) {
                    lostResponseRetry
                }
            } else if case .failed(let message) = model.validation {
                receipt(.wax, "Failed", "Couldn't validate current state: \(message)")
            } else if let message = model.submissionError {
                receipt(.wax, "Failed", "Submission failed: \(message)")
            }
        }
    }

    /// The receipt the control group leads with: an accepted record-only
    /// command's, on an item that stays open (#1978). Nil leaves the record
    /// to the head receipt, which also keeps it when the item requests
    /// nothing and so draws no group.
    private func controlGroupReceipt(_ item: Components.Schemas.AttentionItem) -> String? {
        guard Self.drawsControlGroup(item), let record = model.appliedRecord else { return nil }
        return DecisionCardComposition.recordOnlyReceipt(
            action: record.action, item: item, repeatCount: model.appliedRecordRepeat)
    }

    /// Whether a command's answer was lost and the card offers to resend
    /// it: the receipt states it, and the control group holds the Retry,
    /// or the receipt does on a closed card, which draws no group.
    private var offersLostResponseRetry: Bool {
        model.phase != .superseded && model.canRetryLostResponse
    }

    /// The uncertain receipt's act (R14). It leads the control group of an
    /// open card. A closed card draws no group but keeps the act under the
    /// receipt (#2097): resending is how this device learns whether the
    /// command landed before the item closed, and what clears the entry.
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
    /// reason, which plan §9 requires (on `finding_adjudication` it is the
    /// first Provenance row instead, `DecisionRecommendationPresentation
    /// .faceReason`); the Provenance fold; then the filled action. It argues
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
                .proseMeasure()
            if let reason = recommendation.faceReason(for: item) {
                Text(reason)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
                    .proseMeasure()
            }
            // The digests, policy key, and judgment site revalidate the
            // recommendation; an operator deciding never reads them, so they
            // stay one disclosure away, led by the reason where the face
            // does not draw it. The fold sits above the action, so the
            // action and the controls after it read as one block (#2608).
            SentenceDisclosure(label: "Provenance", isExpanded: $provenanceExpanded) {
                FactGroup(spacing: 4) {
                    if let reason = recommendation.provenanceReason(for: item) {
                        StackedFact(label: "Reason") {
                            Text(reason)
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.ink)
                        }
                    }
                    ForEach(recommendation.sourceFacts) { fact in
                        factRow(fact.label, value: fact.value)
                    }
                }
            }
            actionButton(
                recommendation.action,
                item: item,
                tone: recommendationTone(recommendation.action, item: item),
                showsIcon: false
            )
            // The iOS sticky footer appears when this button is off screen, so
            // the measurement is the button's own, not the block's.
            .onGeometryChange(for: Bool.self) { geometry in
                Self.recommendationActionVisible(
                    frame: geometry.frame(in: .named(Self.scrollSpace)),
                    viewportHeight: geometry.bounds(of: .named(Self.scrollSpace))?.height)
            } action: { visible in
                recommendationVisible = visible
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
    /// ancestor, where the old top-edge test is enough.
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
        rendersInteractiveControls: Bool
    ) -> UnverifiedRegister {
        let context = DecisionCardComposition.UnverifiedContext(
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

    /// The recommendation button's tone, shared by its block and the iPhone
    /// footer: destructive when it confirms, filled when it holds the card's
    /// one fill (R6), and outlined while a held pick holds it instead.
    private func recommendationTone(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem
    ) -> FreesideActionButtonStyle.Tone {
        if AttentionDisplay.confirmationConsequence(action, for: item) != nil {
            return .destructive
        }
        return DecisionCardComposition.recommendationIsFilled(
            for: item, ranking: actionRanking(item), holdsPick: holdsPick(item))
            ? .primary : .secondary
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

    /// A keyword section of fact rows, striped as one fact group.
    private func factSection(
        _ title: String, @ViewBuilder rows: () -> some View
    ) -> some View {
        keywordSection(title) {
            FactGroup(spacing: CardScale.moduleGap) {
                rows()
            }
        }
    }

    /// The card foot's measures.
    private enum FootScale {
        /// A fold's rows start under its label's text, past the glyph.
        static let rowIndent: CGFloat = 19
        static let rowGap: CGFloat = 8
    }

    /// The closed Evidence fold's mono line: the counts it holds.
    static func evidenceSummary(claims: Int, attachments: Int) -> String {
        [
            claims == 0 ? nil : claims == 1 ? "1 agent claim" : "\(claims) agent claims",
            attachments == 0 ? nil : attachments == 1 ? "1 attachment" : "\(attachments) attachments",
        ].compactMap { $0 }.joined(separator: " · ")
    }

    /// What VoiceOver calls the Evidence fold when it holds claims. The
    /// unverified mark draws inside the fold, so the closed disclosure has
    /// to say whose claims they are. Nil speaks the label and summary as
    /// drawn.
    static func evidenceSpokenLabel(claims: Int, attachments: Int) -> String? {
        guard claims > 0 else { return nil }
        let claimsPart = claims == 1 ? "1 unverified agent claim" : "\(claims) unverified agent claims"
        let attachmentsPart =
            attachments == 0 ? nil : attachments == 1 ? "1 attachment" : "\(attachments) attachments"
        return (["Evidence", claimsPart, attachmentsPart] as [String?]).compactMap { $0 }
            .joined(separator: ", ")
    }

    /// The closed Identifiers fold's mono line: values, not field names, so
    /// the item's version and head read without opening it.
    static func identifiersSummary(
        _ item: Components.Schemas.AttentionItem, rows: [AttentionDisplay.BindingRow]
    ) -> String {
        let digests = rows.filter { $0.label.hasSuffix("Digest") }.count
        return [
            "item v\(item.item_version)",
            item.pr_head_sha.isEmpty ? nil : "pr head \(AttentionDisplay.shortRevision(item.pr_head_sha))",
            digests == 0 ? nil : digests == 1 ? "1 digest" : "\(digests) digests",
        ].compactMap { $0 }.joined(separator: " · ")
    }

    /// One fold of the card's foot (R2): a sentence disclosure with its mono
    /// summary and its rows indented under the label, set apart by spacing
    /// alone. A fold of agent claims keeps the unverified register as its
    /// first line, since the sentence label carries none.
    private func footFold<Content: View>(
        _ title: String,
        summary: String,
        spokenLabel: String? = nil,
        isExpanded: Binding<Bool>,
        unverified: UnverifiedRegister? = nil,
        spacing: CGFloat = FootScale.rowGap,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        SentenceDisclosure(
            label: title,
            summary: summary,
            spokenLabel: spokenLabel,
            isExpanded: isExpanded
        ) {
            VStack(alignment: .leading, spacing: spacing) {
                if let unverified {
                    UnverifiedLabel(
                        text: "Agent claims", carriesInfo: false,
                        rendersInteractiveControls: unverified.rendersInteractiveControls)
                }
                foldedUnverifiedSentence(unverified)
                content()
            }
            .padding(.leading, FootScale.rowIndent)
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
        case .task(let id):
            // A task this client doesn't hold has no page to open, so its
            // name draws as plain text.
            if tasks.contains(where: { $0.task.id == id }) {
                FactLinkRow(label: fact.label, value: fact.value) { onSelectTask(id) }
            } else {
                factRow(fact.label, value: fact.value)
            }
        case .link(let url):
            FactLinkRow(label: fact.label, value: fact.value) { openURL(url) }
        case .instant(let instant):
            factRow(fact.label, value: fact.value).exactInstant(instant)
        case .prose:
            StackedFact(label: fact.label) {
                Text(fact.value)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.ink)
            }
        case .offers(let offers):
            // One block per offer, the enrollment over its reading, so a
            // reading that wraps on the phone stays with its own offer.
            StackedFact(label: fact.label) {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(offers.indices, id: \.self) { index in
                        VStack(alignment: .leading, spacing: 1) {
                            Text(offers[index].enrollment)
                                .font(FreesideFont.monoValue)
                            Text(offers[index].reading)
                                .font(FreesideFont.cardBody)
                        }
                        .foregroundStyle(Color.ink)
                        .accessibilityElement(children: .combine)
                    }
                }
            }
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
        /// The reference's daemon-validated media type, size, and
        /// availability: an evidence reference's §5.15 metadata, or a task
        /// attachment's record. Nil only for a conversation message
        /// attachment, which the wire carries as a bare digest with no
        /// metadata; that row falls back to fetch-and-inspect and shows no
        /// typed media/size caption.
        var reference: AttachmentReference? = nil
        let attachments: AttachmentLoader
        let loadsAttachments: Bool
        var text: Components.Schemas.ClaimText? = nil
        /// Draws `text` as the card draws an agent summary, its Markdown as
        /// blocks, where the row is that summary's original report.
        var drawsTextAsSummary = false
        var rendersInteractiveControls = true
        /// Draws the row in its own 1pt box, as the card's foot lists its
        /// attachments. A row inside a card section or a message is already
        /// set apart and draws none.
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
                    if let reference {
                        metadataCaption(reference)
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
                        .accessibilityElement(children: .ignore)
                        .accessibilityAddTraits(.isStaticText)
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
        private func metadataCaption(_ reference: AttachmentReference) -> some View {
            Text("\(reference.mediaType) · \(byteCount(reference.sizeBytes))")
                .font(FreesideFont.attachmentFact)
                .foregroundStyle(Color.inkDim)
                .lineLimit(1)
                .textSelection(.enabled)
                .accessibilityElement(children: .ignore)
                .accessibilityAddTraits(.isStaticText)
                .accessibilityLabel(
                    "\(label): \(reference.mediaType), \(byteCount(reference.sizeBytes))"
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
            let reference = reference
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
    /// group (R14). A command whose attachment failed to upload is known
    /// unsent (#2995), so its receipt says so rather than that it may be
    /// recorded.
    private struct RetryableReceipt: View {
        @Binding var isExpanded: Bool
        let failureMessage: String?
        let isUnsent: Bool

        var body: some View {
            VStack(alignment: .leading, spacing: 8) {
                Notice(
                    tone: .accent, keyword: "Unconfirmed",
                    sentence: isUnsent
                        ? "An attachment didn't upload. Nothing was sent."
                        : "The daemon did not answer. Nothing is assumed."
                )
                .accessibilityElement(children: .combine)
                SentenceDisclosure(label: "What Happened", isExpanded: $isExpanded) {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(
                            isUnsent
                                ? "Retry uploads the attachments again and sends the same message."
                                : "The decision may already be recorded. Retry resends the same command and returns the original result."
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
        let filledAction = DecisionCardComposition.filledAction(
            for: item, ranking: ranking, holdsPick: holdsPick(item))
        // Position, because a repeated action fills only its first button.
        let filled = filledAction.flatMap(ranking.principal.firstIndex(of:))
        VStack(alignment: .leading, spacing: controlGap) {
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

            // Last in the stack, so the row's coming and going when
            // validation ends moves no button under the pointer (#1460).
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
        }
    }

    @ViewBuilder
    private func reviewingAction(_ item: Components.Schemas.AttentionItem) -> some View {
        let ranking = actionRanking(item)
        if let reviewing = ranking.reviewing {
            actionButton(
                reviewing,
                item: item,
                tone: DecisionCardComposition.filledAction(
                    for: item, ranking: ranking, holdsPick: holdsPick(item)) == reviewing
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
            // As on the task's menu (`TaskActionsMenu`): a selectable label
            // would answer for the menu button's spoken label (#2993).
            .textSelection(.disabled)
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
        let label = actionLabelText(action, item: item)
        if showsIcon, let systemImage = AttentionDisplay.systemImage(action) {
            Label(label, systemImage: systemImage)
        } else {
            Text(label)
        }
    }

    /// The shared label, except that a routed dispute's Apply counts the
    /// picks the card holds (R32).
    private func actionLabelText(
        _ action: Components.Schemas.Action,
        item: Components.Schemas.AttentionItem
    ) -> String {
        guard action == .adjudicate_findings,
            let findings = item.review_dispute?.value1.findings, !findings.isEmpty
        else { return AttentionDisplay.label(action, for: item) }
        return AttentionDisplay.applyFindingRoutesLabel(
            selected: selectedFindingRoutes(findings).count, of: findings.count)
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
            // The reveal opens whichever fold holds the identifiers: the
            // card's own, or on a page the footer's.
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
        case .adjudicate_findings:
            guard let findings = item?.review_dispute?.value1.findings else { return }
            Task { await model.submitFindingRoutes(selectedFindingRoutes(findings)) }
        case .retry_with_capabilities:
            capabilityRetrySnapshot = model.snapshot
        case .retry_with_alternate_agent:
            alternateAgentRetrySnapshot = model.snapshot
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

        private func takeRecommendationOnReturn() -> KeyPress.Result {
            guard canTakeRecommendation else { return .ignored }
            takeRecommendationFromKeyboard()
            return .handled
        }

        private var canTakeRecommendation: Bool {
            guard let item = model.snapshot?.item else { return false }
            let recommendation = DecisionRecommendationPresentation.of(item)
            let ranking = actionRanking(item)
            return DecisionKeyboardGate.canTakeRecommendation(
                rankedRecommendation: ranking.recommended,
                presentedRecommendation: recommendation?.action,
                recommendationHoldsFill: DecisionCardComposition.recommendationIsFilled(
                    for: item, ranking: ranking, holdsPick: holdsPick(item)),
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
            alternateAgentRetrySnapshot = nil
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
                Text("The proposal becomes active again at this date or time.")
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
    /// border, inside the pane's margin, no wider than the column's cap,
    /// and seated where the column sits (centered on the Mac). The decision
    /// card and both timelines draw on it, so every detail surface in a
    /// pane starts at one x and one y. `topMargin` replaces the pane's top
    /// margin for a card that sits under a row of its own.
    func detailCard(compact: Bool, topMargin: CGFloat? = nil) -> some View {
        var margin = CardScale.paneMargin(compact: compact)
        if let topMargin { margin.top = topMargin }
        return pageCard(compact: compact)
            .frame(maxWidth: CardScale.columnCap, alignment: .topLeading)
            .padding(margin)
            .frame(maxWidth: .infinity, alignment: CardScale.columnAlignment)
    }

    /// Running text at the column's prose measure, leading in the space it
    /// is offered: a wide column widens the card's modules, never a line
    /// of prose. Only the Mac's column widens past a 640pt card; iOS keeps
    /// its lines as they were, so the measure draws nothing there.
    @ViewBuilder
    func proseMeasure() -> some View {
        #if os(macOS)
            frame(maxWidth: CardScale.proseMeasure, alignment: .leading)
        #else
            self
        #endif
    }

    /// The card with no seat of its own, for the task page to place.
    func pageCard(compact: Bool) -> some View {
        padding(CardScale.padding(compact: compact))
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .freesideCard(cornerRadius: CardScale.cornerRadius)
    }

    /// The task page's body. At regular width it draws in the detail card;
    /// at compact width on the page's ground, as a timeline does. One chain
    /// serves both, for the reason `timelinePage` gives.
    func pageBody(compact: Bool) -> some View {
        padding(compact ? EdgeInsets() : CardScale.padding(compact: false))
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .background {
                if !compact { Color.clear.freesideCard(cornerRadius: CardScale.cornerRadius) }
            }
    }

    /// The task page's column: the card's x and width at any pane width.
    func pageColumn(compact: Bool, topMargin: CGFloat? = nil) -> some View {
        var margin = CardScale.paneMargin(compact: compact)
        if let topMargin { margin.top = topMargin }
        return frame(maxWidth: CardScale.columnCap, alignment: .topLeading)
            .padding(margin)
            .frame(maxWidth: .infinity, alignment: CardScale.columnAlignment)
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
            .frame(maxWidth: compact ? nil : CardScale.columnCap, alignment: .topLeading)
            .padding(margin)
            .frame(maxWidth: compact ? nil : .infinity, alignment: CardScale.columnAlignment)
    }
}

/// Hands its content the locale and time zone installed where it draws.
/// Read on the card itself from its module builders, both come back as
/// SwiftUI's defaults (the process zone and the root locale), so the
/// closed card printed its time in the wrong zone under a pinned
/// screenshot or a device set to another zone. Read here, in a body of
/// its own, they are the installed values.
struct TimeFormatEnvironment<Content: View>: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @ViewBuilder let content: (Locale, TimeZone) -> Content

    var body: some View {
        content(locale, timeZone)
    }
}
