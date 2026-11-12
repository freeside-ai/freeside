import FreesideAPI
import SwiftUI

/// One page per task, in every state: the header, the pinned outline, the
/// leading card and the other open items, then the body, split by tense
/// (◆C): Current Run, what is happening, then the Stop states, the
/// Timeline, and Earlier Conversations, what happened. The current thread
/// draws in its card only. The body's history is as the daemon computes
/// it (`GET /tasks/{id}/timeline`): the task's events as one open rail
/// that marks where each stage began, then every earlier campaign folds
/// its runs. Every list keeps the daemon's newest-first order; unlike the run
/// rail, nothing here is reversed or sorted by timestamp.
///
/// This view is the page's content. `TaskPageHost` gives it its one scroll
/// view, which a leading card arriving or leaving doesn't replace.
struct TaskPageView: View {
    /// Keys the timeline refetch task, as `RunTimelineView.TimelineRequestKey`
    /// does: the task's own revision plus the epoch, full-snapshot revision,
    /// and cache generation, so every canonical replacement refetches while
    /// cached content stays on screen, including a bootstrap that dropped a
    /// read in flight without moving any revision.
    struct TimelineRequestKey: Hashable {
        let taskID: String
        let syncEpoch: String?
        let lastFullSnapshotRevision: Int64?
        let cacheGeneration: Int
        let revision: Int64

        init(snapshot: Components.Schemas.TaskSnapshot, cursors: SyncCursors?, cacheGeneration: Int) {
            taskID = snapshot.task.id
            syncEpoch = cursors?.syncEpoch
            lastFullSnapshotRevision = cursors?.lastFullSnapshotRevision
            self.cacheGeneration = cacheGeneration
            revision = snapshot.as_of_revision
        }
    }

    let coordinator: SyncCoordinator
    let snapshot: Components.Schemas.TaskSnapshot
    /// Opens a run's timeline from its section header: the iOS stack pushes
    /// it, the macOS detail column shows it in place of this view.
    let onOpenRun: (String) -> Void
    /// Routes to an item by id, which leads this page with its card: a
    /// one-line row, the Decisions popup, a linked timeline row, and the
    /// header's guidance. Nil (a screenshot) draws plain text, never a link
    /// that goes nowhere.
    var onOpenItem: ((String) -> Void)?
    /// The item whose card `leadingCard` draws.
    private let leadingItemID: String?
    private let leadingCard: AnyView?
    /// The leading card's identifier rows, which the footer lists as its
    /// Decision group (#2607). The card hands them up through its seat.
    private let decisionIdentifiers: [AttentionDisplay.BindingRow]
    /// Counts the leading card's requests to reveal its identifiers; each
    /// new count opens the footer and scrolls to it.
    private let identifiersReveals: Int
    /// The launch input that opens the card's identifiers, which on a page
    /// open the footer. Held apart from the persisted fold so a launch
    /// doesn't write the operator's choice.
    @State private var identifiersOpenAtLaunch: Bool
    /// The scroll view the outline row jumps through. Nil in a screenshot.
    private let scrollProxy: ScrollViewProxy?
    /// Moves the page's offset by a distance, without animation, to hold
    /// the reader's place. Nil in a screenshot, which holds nothing.
    private let scrollBy: ((CGFloat) -> Void)?
    /// Drops the corrections the host still has on their way, because an
    /// operator's move lands somewhere they didn't build on.
    private let forgetCorrections: () -> Void
    /// What a hosted test reads of the page. Nil outside tests.
    private let probe: TaskPagePlaceProbe?
    /// Supplies the existing screenshot composition without loading tasks or
    /// a scroll viewport. Rendering through body installs environment values
    /// before the content helpers read locale, time zone, and text size.
    var screenshotTimeline: Components.Schemas.TaskTimeline?
    /// Draws the outline row as pinned: at the page's top, with the header
    /// scrolled away. A renderer can't scroll to that state.
    private let screenshotPinned: Bool
    /// The gap above the page where a standing notice sits over it. Nil
    /// keeps the detail column's top margin.
    private let topMargin: CGFloat?
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale
    @Environment(\.pinnedNow) private var pinnedNow
    /// Which folds are open, per task id on this device. A screenshot
    /// passes its own instance over throwaway defaults.
    @State private var disclosures: TaskTimelineDisclosurePreferences
    /// The chip last chosen. Nil is the page's default: Decisions with an
    /// open item, Task otherwise.
    @State private var chosenSection: TaskPageSection?
    @State private var outlineHeight: CGFloat = 0
    /// The Stop confirmation and the What Happened fold, held here because
    /// the lazy stack can drop the Stop controls, and their state with them,
    /// while a card leaving shrinks the document under the reader.
    @State private var stopConfirmation: TaskStopModel.Confirmation?
    @State private var showsStopExplanation = false
    /// The specification the documents line opened, presented outside the
    /// lazy stack for the same reason.
    @State private var documentRequest: TaskDocumentRequest?
    /// The attached folders the operator opened, by name; each starts
    /// folded.
    @State private var openAttachmentFolders: Set<String> = []
    /// Keeps the reader's place for a page mounting again for this task,
    /// and counts the cards the operator opens. Nil in a screenshot and in
    /// tests that don't exercise it.
    private let memory: TaskPageMemory?
    @State private var place = PlaceTracker()

    /// The reader's place and the jump targets' latest tops in the scroll
    /// view, kept off the view's observed state so scrolling doesn't redraw
    /// the page every frame.
    @MainActor private final class PlaceTracker {
        var targetTops: [TaskPageAnchor: CGFloat] = [:]
        var rowTop: CGFloat = 0
        /// The section or run the reader has under the pinned outline row.
        /// Nil while the header is in view.
        var anchor: TaskPageAnchor?
        /// Set once the mount has returned to the place the memory held.
        /// Until then the page records nothing, so its first position, the
        /// top it mounts at, doesn't overwrite the place it is returning to.
        /// A page that reappears from under a run keeps its own offset, so
        /// it restores once.
        var restored = false
        /// A move waiting for a change to lay out. The page records nothing
        /// meanwhile, and holds nothing, so the layout the change first
        /// draws isn't taken for the reader's place.
        var pendingMove: PendingMove?
        /// Each hold point's top in the page's own space, which layout moves
        /// and scrolling never does, and the marker that last reported it.
        /// A point that stops drawing leaves.
        var holdTops: [HoldPoint: CGFloat] = [:]
        var holdReporters: [HoldPoint: UUID] = [:]
        /// The page's top in the scroll view, which scrolling moves and
        /// layout inside the page doesn't.
        var pageTop: CGFloat = 0
        /// The point the reader's place is held by, and its top when the
        /// page last matched it.
        var held: HoldPoint?
        var heldTop: CGFloat = 0
        /// Each point's top once the layout pass that last moved the held
        /// point had reported every top, so a held point that goes hands
        /// its hold to one above it with the top it had then.
        var settledTops: [HoldPoint: CGFloat] = [:]
        var settleScheduled = false
        var reselectScheduled = false
        /// Counts the operator's moves, so a move's completion ends only
        /// that move and not one the operator started after it.
        var moveCount = 0
        /// Where the page's top goes as each correction sent is applied, in
        /// order. The scroll a correction makes keeps the held point, so
        /// arriving there picks none anew; picking during the layout that
        /// caused it would read tops the pass hasn't finished reporting.
        var correctedPageTops: [CGFloat] = []
    }

    /// What the page holds the reader's place by: a jump target, or a
    /// marker at the top of a part the outline row offers no jump to (a row
    /// of the Timeline's rail, keyed by `TimelineRail.holdKeys`, a body
    /// section, a conversation, or a part of the seated card).
    fileprivate enum HoldPoint: Hashable {
        case anchor(TaskPageAnchor)
        case marker(String)

        /// The name a test reads the point's top by.
        var probeKey: String {
            switch self {
            case .anchor(let target): "\(target)"
            case .marker(let key): key
            }
        }
    }

    private enum PendingMove {
        /// To a section or run the operator chose.
        case jump
        /// To the leading card, which the operator opened.
        case toCard
        /// To the Identifiers footer, which the leading card revealed.
        case toIdentifiers
    }

    /// The page's top, where a Decisions place with no card left returns.
    private enum PageTop: Hashable { case top }

    /// The page's own coordinate space, which its hold points report in.
    fileprivate static let pageSpace = "TaskPageView.page"

    /// The Identifiers footer, where a card's reveal lands. Not a
    /// `TaskPageAnchor`: the footer is no section the outline row offers.
    private enum IdentifiersFooter: Hashable { case top }

    /// The leading card's thread, where the Conversation chip lands while
    /// the card has one. Not a `TaskPageAnchor`: the thread is part of
    /// the card, so a reader in it keeps the Decisions place. Otherwise
    /// deciding on a card from below its thread's top would return the
    /// reader to Earlier Conversations at the page's end, where the
    /// closed thread goes, instead of the top.
    private enum CardThread: Hashable { case top }

    init(
        coordinator: SyncCoordinator, snapshot: Components.Schemas.TaskSnapshot,
        onOpenRun: @escaping (String) -> Void, onOpenItem: ((String) -> Void)? = nil,
        leadingItemID: String? = nil, leadingCard: AnyView? = nil, scrollProxy: ScrollViewProxy? = nil,
        scrollBy: ((CGFloat) -> Void)? = nil, forgetCorrections: @escaping () -> Void = {},
        probe: TaskPagePlaceProbe? = nil,
        decisionIdentifiers: [AttentionDisplay.BindingRow] = [], identifiersReveals: Int = 0,
        identifiersOpenAtLaunch: Bool = false,
        topMargin: CGFloat? = nil, memory: TaskPageMemory? = nil,
        screenshotTimeline: Components.Schemas.TaskTimeline? = nil,
        screenshotPinned: Bool = false,
        disclosures: TaskTimelineDisclosurePreferences? = nil
    ) {
        self.memory = memory
        self.coordinator = coordinator
        self.snapshot = snapshot
        self.onOpenRun = onOpenRun
        self.onOpenItem = onOpenItem
        self.leadingItemID = leadingItemID
        self.leadingCard = leadingCard
        self.decisionIdentifiers = decisionIdentifiers
        self.identifiersReveals = identifiersReveals
        _identifiersOpenAtLaunch = State(initialValue: identifiersOpenAtLaunch)
        self.scrollProxy = scrollProxy
        self.scrollBy = scrollBy
        self.forgetCorrections = forgetCorrections
        self.probe = probe
        self.topMargin = topMargin
        self.screenshotTimeline = screenshotTimeline
        self.screenshotPinned = screenshotPinned
        _disclosures = State(initialValue: disclosures ?? .shared)
    }

    private var now: Date { pinnedNow ?? Date() }

    private func fold(_ section: TaskTimelineDisclosurePreferences.Section) -> Binding<Bool> {
        disclosures.binding(section, taskID: snapshot.task.id)
    }

    private var timeline: Components.Schemas.TaskTimeline? {
        coordinator.taskTimelinesByTaskID[snapshot.task.id]
    }

    private var compact: Bool { horizontalSizeClass == .compact }

    private var list: UnifiedList { coordinator.unifiedList }

    /// The task's open items, newest first.
    private var openItems: [Components.Schemas.AttentionItemSnapshot] {
        list.openItems(inTask: snapshot.task.id)
    }

    /// Every item the client holds under this task, open or closed.
    private var taskItems: [Components.Schemas.AttentionItemSnapshot] {
        let list = list
        return coordinator.store.orderedSnapshots.filter { list.taskID(ofItem: $0.item.id) == snapshot.task.id }
    }

    /// The threads of items no longer open, which fold under the Timeline.
    private var earlierConversations: [TaskTimelinePresentation.EarlierConversation] {
        TaskTimelinePresentation.earlierConversations(taskItems.map(\.item), leadingItemID: leadingItemID) {
            coordinator.store.conversationsByID[$0]
        }
    }

    /// The messages in the thread the leading card draws; zero without a
    /// card or a thread.
    private var cardThreadMessages: Int {
        guard leadingCard != nil,
            let id = taskItems.first(where: { $0.item.id == leadingItemID })?.item.conversation_id
        else { return 0 }
        return coordinator.store.conversationsByID[id]?.conversation.messages.count ?? 0
    }

    /// Where the Conversation chip goes: the card's thread when it has
    /// one, else Earlier Conversations, else nowhere.
    static func conversationChip(
        cardThreadMessages: Int, earlier: [TaskTimelinePresentation.EarlierConversation]
    ) -> OutlineConversation? {
        if cardThreadMessages > 0 { return .inCard(messages: cardThreadMessages) }
        let messages = earlier.reduce(0) { $0 + $1.conversation.conversation.messages.count }
        return messages > 0 ? .earlier(messages: messages) : nil
    }

    /// The gap between the page's parts: header, outline row, card, one-line
    /// rows, and body.
    private static let contentGap: CGFloat = 16

    @ViewBuilder var body: some View {
        if let screenshotTimeline {
            VStack(alignment: .leading, spacing: 0) {
                if !screenshotPinned {
                    header(screenshotTimeline)
                        .padding(.bottom, Self.contentGap - 6)
                }
                outlineRow(screenshotTimeline)
                sections(screenshotTimeline)
                    .padding(.top, Self.contentGap)
            }
            .pageColumn(compact: compact, topMargin: topMargin)
        } else {
            // A plain stack, with the outline row pinned by drawing it lower
            // as the page scrolls past it. A lazy stack's pinned header
            // would cost the page its body whenever a shrinking document
            // leaves the offset past its end for a moment, and the tops it
            // estimates meanwhile would move the reader (#1460). The scroll
            // view's space starts below its top content inset (the Mac
            // toolbar, the iOS navigation bar), so the row pins at zero.
            VStack(alignment: .leading, spacing: 0) {
                header(timeline)
                    .padding(.bottom, Self.contentGap - 6)
                outlineRow(timeline)
                    .onGeometryChange(for: CGFloat.self, of: \.size.height) { outlineHeight = $0 }
                    .onGeometryChange(for: CGFloat.self, of: { max(0, $0.frame(in: .scrollView).minY) }) {
                        top in
                        place.rowTop = top
                        probe?.rowTop = top
                        recordPlace()
                    }
                    .visualEffect { content, geometry in
                        content.offset(y: max(0, -geometry.frame(in: .scrollView).minY))
                    }
                    .zIndex(1)
                sections(timeline)
                    .padding(.top, Self.contentGap)
            }
            // Named outside the column's margin, which a standing notice in
            // the detail column changes, so a new margin moves the hold
            // points as any other layout above the reader does.
            .pageColumn(compact: compact, topMargin: topMargin)
            .coordinateSpace(.named(Self.pageSpace))
            .onGeometryChange(for: CGFloat.self, of: { $0.frame(in: .scrollView).minY }) { top in
                place.pageTop = top
                probe?.pageTop = top
                if let landed = place.correctedPageTops.firstIndex(where: { abs(top - $0) < 0.5 }) {
                    place.correctedPageTops.removeFirst(landed + 1)
                } else {
                    place.correctedPageTops = []
                    scheduleReselect()
                }
            }
            .id(PageTop.top)
            .task { await restorePlace() }
            .onChange(of: chosenSection) { held?.chosenSection = chosenSection }
            .onChange(of: memory?.cardOpenings) { moveToOpenedCard() }
            // `repairSelection` moves the lead only off a card that closed,
            // and never seats one, so a lead arriving where none was, or
            // moving off a card still open, is the operator choosing it.
            .onChange(of: leadingItemID) { previous, current in
                let chosen = previous.map(openItemIDs.contains) ?? (current != nil)
                // An open the memory counted has already started the move.
                if chosen, place.pendingMove != .toCard {
                    moveToOpenedCard()
                }
            }
            // A reader at Decisions whose last card left has nothing there
            // to hold, so the page goes to its top.
            .onChange(of: leadingCard != nil) { _, leads in
                guard !leads, place.anchor == .section(.decisions) else { return }
                forgetCorrections()
                place.correctedPageTops = []
                scrollProxy?.scrollTo(PageTop.top, anchor: .top)
                place.anchor = nil
                held?.anchor = nil
                chosenSection = nil
            }
            .onChange(of: identifiersReveals) { revealIdentifiers() }
            .onChange(of: selectedSection, initial: true) { probe?.selectedSection = selectedSection }
            .onChange(of: openAttachmentFolders, initial: true) { reportStateFolds() }
            .onChange(of: showsStopExplanation, initial: true) { reportStateFolds() }
            .onAppear {
                probe?.openStateFold = { fold in
                    if fold == TaskPagePlaceProbe.whatHappened {
                        showsStopExplanation = true
                    } else {
                        openAttachmentFolders.insert(fold)
                    }
                }
                probe?.select = { onSelectSection($0, timeline: timeline) }
            }
            .taskStopConfirmation(
                $stopConfirmation, model: coordinator.taskStop, dynamicTypeSize: dynamicTypeSize
            )
            .sheet(item: $documentRequest) { request in
                TaskDocumentReaderSheet(request: request, attachments: coordinator.store.attachments)
                    .dynamicTypeSize(dynamicTypeSize)
            }
            #if os(iOS)
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        if TaskActionsSlot(compact: compact) == .navigationBar { taskActionsMenu(timeline) }
                    }
                }
            #endif
        }
    }

    /// Everything under the outline row. Each part a refresh can change
    /// carries a hold point, so a reader below it holds their place as it
    /// changes above them.
    private func sections(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        VStack(alignment: .leading, spacing: Self.contentGap) {
            let earlier = earlierConversations
            let chip = Self.conversationChip(cardThreadMessages: cardThreadMessages, earlier: earlier)
            if let leadingCard {
                leadingCard
                    .environment(\.taskPageHoldMarker, cardHoldMarker)
                    .background(alignment: .top) { jumpTarget(.section(.decisions)) }
                    // The card draws the chip's target on its own thread,
                    // which only it can place.
                    .environment(
                        \.taskPageCardThreadTarget,
                        chip?.isInCard == true ? AnyView(cardThreadTarget) : nil)
            }
            let others = openItems.filter { $0.item.id != leadingItemID }
            if !others.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(others, id: \.item.id) { other in
                        OneLineItemRow(
                            ask: AttentionDisplay.ask(other.item),
                            type: AttentionDisplay.title(other.item._type)
                        ) {
                            onOpenItem?(other.item.id)
                        }
                        .onAppear { probe?.oneLineItemIDs.insert(other.item.id) }
                        .onDisappear { probe?.oneLineItemIDs.remove(other.item.id) }
                    }
                }
                .background(alignment: .top) { bodyMarker("one-line-rows") }
            }
            VStack(alignment: .leading, spacing: 22) {
                stopStates
                    .background(alignment: .top) { bodyMarker("stop") }
                lineChangeStates
                    .background(alignment: .top) { bodyMarker("line-change") }
                timelineSection(timeline)
                    .background(alignment: .top) { jumpTarget(.section(.timeline)) }
                // Absent when it lists none. It is a place the reader
                // keeps even while the chip goes to the card's thread.
                if !earlier.isEmpty {
                    earlierConversationsSection(earlier)
                        .background(alignment: .top) { jumpTarget(.section(.conversation)) }
                }
                // Outside the timeline, so a seated card's identifiers,
                // which have no other place on the page, stay reachable
                // while the history loads or is unavailable.
                if hasIdentifiersFooter(timeline) {
                    identifiers(timeline)
                        .background(alignment: .top) { bodyMarker("identifiers") }
                }
            }
            .foregroundStyle(Color.ink)
            .pageBody(compact: compact)
        }
    }

    /// The composition with fixture data supplied directly, because
    /// ImageRenderer never executes the loading task.
    func screenshotContent(
        _ timeline: Components.Schemas.TaskTimeline, disclosures: TaskTimelineDisclosurePreferences? = nil,
        leadingItemID: String? = nil, leadingCard: AnyView? = nil,
        decisionIdentifiers: [AttentionDisplay.BindingRow] = [], pinned: Bool = false
    ) -> some View {
        TaskPageView(
            coordinator: coordinator, snapshot: snapshot, onOpenRun: onOpenRun,
            onOpenItem: onOpenItem, leadingItemID: leadingItemID, leadingCard: leadingCard,
            decisionIdentifiers: decisionIdentifiers,
            screenshotTimeline: timeline, screenshotPinned: pinned,
            disclosures: disclosures ?? TaskTimelineDisclosurePreferences(defaults: nil))
    }

    // MARK: Jumps

    /// A target above its view by the outline row's height, so a jump lands
    /// clear of the row pinned over the scroll view's top. It reports its
    /// top as the page scrolls, which is how the page knows its place.
    private func jumpTarget(_ target: TaskPageAnchor) -> some View {
        Color.clear
            .frame(height: outlineHeight)
            .onGeometryChange(for: CGFloat.self, of: { $0.frame(in: .scrollView).minY }) { top in
                place.targetTops[target] = top
                recordPlace()
            }
            .holdsPlace(scrollBy == nil ? nil : .anchor(target), report: reportHoldTop)
            .onDisappear { place.targetTops[target] = nil }
            .id(target)
            .padding(.top, -outlineHeight)
            .accessibilityHidden(true)
    }

    /// The Conversation chip's target on the leading card's thread. It
    /// lands clear of the pinned row as a jump target does, and reports
    /// no top: the page's place there is the card's (`CardThread`).
    private var cardThreadTarget: some View {
        Color.clear
            .frame(height: outlineHeight)
            .id(CardThread.top)
            .padding(.top, -outlineHeight)
            .accessibilityHidden(true)
    }

    /// A hold point keyed by `key`, set the outline row's height above
    /// where it is placed, as a jump target is, so it reads as reached
    /// where a target would. Draws nothing where the page holds nothing.
    private func holdMarker(_ key: String) -> AnyView {
        guard scrollBy != nil else { return AnyView(EmptyView()) }
        return AnyView(
            Color.clear
                .frame(height: outlineHeight)
                .holdsPlace(.marker(key), report: reportHoldTop)
                // A rail row arriving above moves each key to the next
                // marker down; a marker that takes a new key is a new view,
                // so the key reports from where it now draws.
                .id(key)
                .padding(.top, -outlineHeight)
                .accessibilityHidden(true))
    }

    /// A body part's hold point, in a namespace apart from the rail's keys.
    private func bodyMarker(_ key: String) -> AnyView { holdMarker("page:" + key) }

    /// The seated card's hold points, in a namespace of their own.
    private var cardHoldMarker: (@MainActor (String) -> AnyView)? {
        guard scrollBy != nil else { return nil }
        return { key in holdMarker("card:" + key) }
    }

    private func jump(to target: TaskPageAnchor) {
        land(.jump) { scrollProxy?.scrollTo(target, anchor: .top) }
    }

    /// Runs an operator's animated move. The page holds nothing until it
    /// lands, so a correction from a refresh meanwhile can't stop it
    /// partway, and where it lands is the new baseline; the corrections
    /// still on their way built on the place it left.
    private func land(_ move: PendingMove, _ scroll: () -> Void) {
        place.pendingMove = move
        place.moveCount += 1
        let count = place.moveCount
        place.correctedPageTops = []
        forgetCorrections()
        withAnimation(.default, completionCriteria: .logicallyComplete) {
            scroll()
        } completion: { [place] in
            guard place.moveCount == count else { return }
            place.pendingMove = nil
            recordPlace()
            scheduleReselect()
        }
    }

    // MARK: Place

    /// The memory's entry when it is this task's. Nil in a screenshot, and
    /// for the moment before a mount claims the memory.
    private var held: TaskPageMemory? {
        memory?.taskID == snapshot.task.id ? memory : nil
    }

    private var openItemIDs: Set<String> { Set(openItems.map(\.item.id)) }

    /// Records the reader's place when it changes, and writes it through to
    /// the memory for a later mount.
    private func recordPlace() {
        guard place.restored, place.pendingMove == nil else { return }
        let anchor = TaskPageAnchor.reading(offsets: place.targetTops.mapValues { $0 - place.rowTop })
        guard anchor != place.anchor else { return }
        place.anchor = anchor
        held?.anchor = anchor
    }

    /// Holds the reader's place when layout above it changes: a card
    /// seated or leaving, a rail row or message arriving, the header
    /// growing. When the held point's top in the page moves by `d`, the
    /// offset moves by `d` with it, so what the reader sees stays still.
    /// Anything that changes inside the viewport is below the held point,
    /// so it never scrolls the page; nor does an operator's move, which
    /// takes a fresh baseline instead (#1460).
    ///
    /// A marker that leaves after another took its point, as a rail row's
    /// does when a row arrives above it, leaves the newer report in place.
    private func reportHoldTop(_ point: HoldPoint, reporter: UUID, _ top: CGFloat?) {
        guard let top else {
            guard place.holdReporters[point] == reporter else { return }
            place.holdTops[point] = nil
            place.holdReporters[point] = nil
            probe?.holdTops[point.probeKey] = nil
            // A held point that goes, as a rail row does when its label
            // changes, possibly in the refresh that adds a row above it,
            // hands its hold on once the pass has reported every top.
            if point == place.held { scheduleReselect() }
            return
        }
        place.holdTops[point] = top
        place.holdReporters[point] = reporter
        probe?.holdTops[point.probeKey] = top
        guard point == place.held else { return }
        let moved = top - place.heldTop
        place.heldTop = top
        scheduleSettle()
        correct(by: moved)
    }

    /// Scrolls by the distance the reader's content moved, unless the page
    /// is still returning to its place or the operator is moving it.
    private func correct(by moved: CGFloat) {
        guard place.restored, place.pendingMove == nil, abs(moved) >= 0.5, let scrollBy else { return }
        place.correctedPageTops.append((place.correctedPageTops.last ?? place.pageTop) - moved)
        scrollBy(moved)
    }

    private func scheduleSettle() {
        guard !place.settleScheduled else { return }
        place.settleScheduled = true
        Task { @MainActor [place] in
            place.settleScheduled = false
            place.settledTops = place.holdTops
        }
    }

    /// Picks the point that holds the reader's place after a scroll: the
    /// lowest at or above the outline row's top, as `TaskPageAnchor.reading`
    /// picks the section. Waits for the layout pass to end, so a scroll the
    /// pass caused, as a shrinking document clamps the offset, isn't read
    /// against tops it hasn't reported yet.
    private func scheduleReselect() {
        guard scrollBy != nil, !place.reselectScheduled else { return }
        place.reselectScheduled = true
        Task { @MainActor [place] in
            place.reselectScheduled = false
            // With the page's body not drawn, nothing is to be read; the
            // held point stays until it draws again.
            guard !place.holdTops.isEmpty else { return }
            // A held point that went is followed by the nearest point above
            // it still drawn, which moved as far as the reader's content.
            if let gone = place.held, place.holdTops[gone] == nil, let goneTop = place.settledTops[gone],
                let heir = place.settledTops
                    .filter({ $0.value <= goneTop && place.holdTops[$0.key] != nil })
                    .max(by: { $0.value < $1.value }),
                let heirTop = place.holdTops[heir.key]
            {
                correct(by: heirTop - heir.value)
            }
            // Read where the page lands once its corrections apply.
            let pageTop = place.correctedPageTops.last ?? place.pageTop
            let reading = place.holdTops
                .filter { pageTop + $0.value - place.rowTop <= 1 }
                .max { $0.value < $1.value }
            place.held = reading?.key
            place.heldTop = reading?.value ?? 0
            place.settledTops = place.holdTops
        }
    }

    /// Moves to the leading card when the operator opens one: a one-line
    /// row, the Decisions popup, a timeline row link, the header's
    /// guidance, or a notification link, including to the card that
    /// already leads, or a lead moving off a card that is still open, as a
    /// sidebar task row does. The route seats a card it changes in a later
    /// transaction, so the move waits the same beat.
    private func moveToOpenedCard() {
        place.pendingMove = .toCard
        Task { @MainActor [place] in
            try? await Task.sleep(for: .milliseconds(120))
            guard place.pendingMove == .toCard else { return }
            chosenSection = nil
            // Opening forgot the memory's place; the record after the move
            // lands writes the new one through.
            place.anchor = nil
            land(.toCard) { scrollProxy?.scrollTo(TaskPageAnchor.section(.decisions), anchor: .top) }
        }
    }

    /// Opens the Identifiers footer and moves to it when the leading card
    /// reveals its identifiers: the item row's Reveal in Identifiers, an
    /// inspection, or a launch input. The route seats a card it changes in
    /// a later transaction, so the move waits the beat an opened card's
    /// does, and wins over that move, which the same route starts.
    private func revealIdentifiers() {
        identifiersFold.wrappedValue = true
        place.pendingMove = .toIdentifiers
        Task { @MainActor [place] in
            try? await Task.sleep(for: .milliseconds(120))
            guard place.pendingMove == .toIdentifiers else { return }
            chosenSection = nil
            place.anchor = nil
            land(.toIdentifiers) { scrollProxy?.scrollTo(IdentifiersFooter.top, anchor: .top) }
        }
    }

    /// Returns a fresh mount to the place the memory holds for this task,
    /// and to the chip last chosen: the Mac detail column replaces the page
    /// with a run, so Back to Task mounts it again.
    private func restorePlace() async {
        guard !place.restored else { return }
        defer { place.restored = true }
        guard let memory else { return }
        let target = memory.restore(taskID: snapshot.task.id, leadsWithCard: leadingCard != nil)
        chosenSection = memory.chosenSection
        place.anchor = target
        guard let target else { return }
        // The page lays out over a later commit, as `jump(toRun:)` waits for.
        guard (try? await Task.sleep(for: .milliseconds(120))) != nil else { return }
        scrollProxy?.scrollTo(target, anchor: .top)
    }

    /// Opens the folds that hold a run, then jumps to it once they've drawn.
    /// A run with no card on the page opens its run page instead.
    private func jump(toRun runID: String, in timeline: Components.Schemas.TaskTimeline?) {
        guard let timeline else { return }
        switch TaskTimelinePresentation.runTarget(runID, task: snapshot.task, timeline: timeline) {
        case .currentRun:
            break
        case .earlierAttempt:
            fold(.run(runID)).wrappedValue = true
        case .earlierCampaign(let campaignID):
            fold(.campaign(campaignID)).wrappedValue = true
            fold(.run(runID)).wrappedValue = true
        case .runPage:
            onOpenRun(runID)
            return
        }
        chosenSection = .runs
        // The folds lay out on a later commit, and no transaction completes
        // when they were already open, so the jump waits a beat for either.
        Task { @MainActor in
            try? await Task.sleep(for: .milliseconds(120))
            jump(to: .run(runID))
        }
    }

    // MARK: Header

    private func reviewRounds(
        _ timeline: Components.Schemas.TaskTimeline?
    ) -> [String: [Components.Schemas.RunReviewRound]] {
        timeline.map { TaskTimelinePresentation.reviewRounds($0, runTimelines: coordinator.timelinesByRunID) } ?? [:]
    }

    /// The task's position with the same stage and round its list row reads
    /// (`UnifiedListView.taskPosition`).
    private func position(_ timeline: Components.Schemas.TaskTimeline?) -> TaskDisplay.Position? {
        let task = snapshot.task
        return TaskDisplay.position(
            task, runs: coordinator.runs, attentionItems: coordinator.store.orderedSnapshots, history: timeline,
            stage: currentStage(timeline))
    }

    private func currentStage(_ timeline: Components.Schemas.TaskTimeline?) -> TaskDisplay.CurrentStage? {
        TaskDisplay.currentStage(
            snapshot.task, runs: coordinator.runs, history: timeline, reviewRounds: reviewRounds(timeline))
    }

    /// The header's parked-task move at `position`, or nil while it has
    /// nothing to say. Internal so a test can check the header's choice.
    func parkedTaskMove(at position: TaskDisplay.Position?) -> ParkedTaskMoveView? {
        let move = ParkedTaskMoveView(
            coordinator: coordinator, taskID: snapshot.task.id, offers: TaskDisplay.moveOffers(position))
        return move.isQuiet ? nil : move
    }

    /// Layer 0: what the work is and where it stands, without scrolling
    /// (5.6): the task's project, issue, and stage over its name, one status
    /// line, the hold when there is one, and where to act. The task id lives
    /// in the page's Identifiers footer; the copy menu keeps a quick path to
    /// it. A look-alike task's header leads its detail with the marker its
    /// row draws, so a card on the page says which task it belongs to.
    /// Internal so a screenshot golden can draw the header on its own.
    func header(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = snapshot.task
        let marker = Self.lookAlikeMarker(task, in: list, now: now, locale: locale, timeZone: timeZone)
        let position = position(timeline)
        let lines = TaskDisplay.rowLines(task, position: position)
        let stage = TaskDisplay.detailStage(
            task, runs: coordinator.runs, history: timeline, reviewRounds: reviewRounds(timeline))
        let open = openItems
        let status = TaskTimelinePresentation.statusLine(
            task, position: position, stage: currentStage(timeline), openItems: open.map(\.item),
            run: TaskTimelinePresentation.currentRun(task, in: timeline),
            completion: TaskTimelinePresentation.completion(task, runs: coordinator.runs),
            now: now, locale: locale, timeZone: timeZone)
        return PageHeader(
            eyebrow: TaskDisplay.rowContext(task, position: position),
            title: TaskNameLabel(
                name: TaskTimelinePresentation.headerName(timeline: timeline, snapshot: snapshot),
                font: FreesideFont.ask,
                monoFont: FreesideFont.mono(.title2),
                lineLimit: 3),
            status: status
        ) {
            if let marker {
                SidebarRowContext(text: marker)
            }
            documentsLine(timeline)
            attachmentsFold
            // A parked status line already names the hold.
            if let hold = TaskDisplay.holdCallout(task, position: position, stage: stage),
                position?.hold.map(status.hasSuffix) != true
            {
                holdCallout(hold)
            }
            // A quota-window park may offer agents on other pools; moving
            // to one is the operator's choice, never a fallback. A saved move
            // or its refusal stays after the hold clears, so its Retry is
            // never out of reach.
            if let move = parkedTaskMove(at: position) { move }
            switch lines.visibleGuidance(attention: position?.attention == true) {
            case .link(let title):
                // The card the guidance names is on the page, so it says
                // nothing the page doesn't. Link styling promises navigation,
                // so it is drawn only where tapping it opens that item.
                if open.isEmpty {
                    if let itemID = position?.attentionItemID, let onOpenItem {
                        Button {
                            onOpenItem(itemID)
                        } label: {
                            FreesideLink(title: title, face: FreesideFont.noticeAction)
                                .multilineTextAlignment(.leading)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        .buttonStyle(.plain)
                    } else {
                        guidanceSentence(lines.guidance)
                    }
                }
            case .sentence(let sentence): guidanceSentence(sentence)
            case nil: EmptyView()
            }
        }
        .copyMenu {
            Button("Copy task ID") { Clipboard.copy(snapshot.task.id) }
        }
    }

    /// The marker the header draws for a look-alike task (#1922), or nil for
    /// any other. It reads the list's rows, the input the list's own marker
    /// reads, so the page marks exactly the tasks whose rows do. Not the
    /// eyebrow: `CardEyebrow` uppercases its text, so the id would no longer
    /// match the row's.
    static func lookAlikeMarker(
        _ task: Components.Schemas.Task, in list: UnifiedList, now: Date, locale: Locale, timeZone: TimeZone
    ) -> String? {
        guard TaskDisplay.ambiguousTaskIDs(list.rows.compactMap(\.taskSnapshot)).contains(task.id) else {
            return nil
        }
        return TaskDisplay.lookAlikeMarker(task, now: now, locale: locale, timeZone: timeZone)
    }

    /// The documents line (◆D): accent link words at body size with faint
    /// dots between them, no keyword or box. It wraps where the column
    /// can't hold it, which on a phone puts it under the title. A dot
    /// rides the end of the part before it, so a wrapped line starts with
    /// a word.
    @ViewBuilder
    private func documentsLine(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let links = TaskTimelinePresentation.documentLinks(
            snapshot.task, timeline: timeline,
            completion: TaskTimelinePresentation.completion(snapshot.task, runs: coordinator.runs),
            items: taskItems.map(\.item))
        if !links.isEmpty {
            WrappingHStack(horizontalSpacing: 8, verticalSpacing: 4) {
                ForEach(Array(links.enumerated()), id: \.offset) { index, link in
                    HStack(spacing: 8) {
                        documentLink(link)
                        if index < links.count - 1 {
                            Text("·")
                                .font(FreesideFont.cardBody)
                                .foregroundStyle(Color.inkFaint)
                                .accessibilityHidden(true)
                        }
                    }
                }
            }
        }
    }

    /// A part with somewhere to go is a link: `Link` for GitHub, which
    /// VoiceOver reads as a link, and a link-trait button for the reader.
    /// Without one it is the same words in dim plain text, since link
    /// styling promises somewhere to go.
    @ViewBuilder
    private func documentLink(_ link: TaskTimelinePresentation.DocumentLink) -> some View {
        let label = FreesideLink(title: link.title, face: FreesideFont.noticeAction)
        if let hint = link.accessibilityHint, screenshotTimeline == nil {
            switch link {
            case .issue(_, let url?), .pullRequest(_, let url?):
                Link(destination: url) { label }
                    .accessibilityLabel(link.spokenTitle)
                    .accessibilityHint(hint)
            case .prompt, .specification:
                Button {
                    documentRequest = link.readerRequest
                } label: {
                    label.contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(link.spokenTitle)
                // Read as a link only, as the GitHub parts beside it are.
                .accessibilityRemoveTraits(.isButton)
                .accessibilityAddTraits(.isLink)
                .accessibilityHint(hint)
            case .issue, .pullRequest, .noPullRequest:
                EmptyView()
            }
        } else if link.accessibilityHint != nil {
            label
        } else {
            Text(link.title)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
    }

    private func outlineRow(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let open = openItems
        let conversation = Self.conversationChip(
            cardThreadMessages: cardThreadMessages, earlier: earlierConversations)
        return OutlineRow(
            decisions: open.map { .init(id: $0.item.id, title: AttentionDisplay.ask($0.item)) },
            leadingItemID: leadingItemID,
            conversation: conversation,
            runs: (timeline?.sections.flatMap(\.runs) ?? []).map {
                .init(id: $0.run_id, title: TaskTimelinePresentation.runTitle($0))
            },
            selected: selectedSection,
            rendersInteractiveControls: screenshotTimeline == nil,
            onSelect: { onSelectSection($0, timeline: timeline) },
            onChooseDecision: { onOpenItem?($0) },
            onChooseRun: { jump(toRun: $0, in: timeline) },
            trailing: {
                if TaskActionsSlot(compact: compact) == .outlineRow { taskActionsMenu(timeline) }
            }
        )
    }

    /// The chip the outline row selects. It follows the seated card, not
    /// the open items, so an item opening under a reader leaves it where
    /// it was.
    private var selectedSection: TaskPageSection {
        chosenSection ?? (leadingItemID == nil ? .task : .decisions)
    }

    private func onSelectSection(_ section: TaskPageSection, timeline: Components.Schemas.TaskTimeline?) {
        // With no card seated the Decisions target isn't drawn, so
        // the chip opens the item, as its one-line row does.
        if section == .decisions, leadingItemID == nil, let newest = openItems.first {
            onOpenItem?(newest.item.id)
            return
        }
        // A one-run page's Runs chip goes where choosing that run
        // from the popup would; with none it lands on the Timeline.
        if section == .runs, let run = timeline?.sections.flatMap(\.runs).first {
            jump(toRun: run.run_id, in: timeline)
            return
        }
        chosenSection = section
        switch section {
        // The Task chip goes to the page head, where the task is
        // named. The head is no place the reader keeps.
        case .task: land(.jump) { scrollProxy?.scrollTo(PageTop.top, anchor: .top) }
        case .runs: jump(to: .section(.timeline))
        case .conversation:
            if Self.conversationChip(cardThreadMessages: cardThreadMessages, earlier: earlierConversations)?
                .isInCard == true
            {
                land(.jump) { scrollProxy?.scrollTo(CardThread.top, anchor: .top) }
            } else {
                jump(to: .section(.conversation))
            }
        case .decisions, .timeline: jump(to: .section(section))
        }
    }

    private func reportStateFolds() {
        probe?.openStateFolds =
            openAttachmentFolders.union(showsStopExplanation ? [TaskPagePlaceProbe.whatHappened] : [])
    }

    /// Where the task's menu draws. Both are pinned, so Stop stays in reach
    /// while the page scrolls.
    enum TaskActionsSlot {
        /// The outline row's trailing slot, outside its chips' scroll.
        case outlineRow
        /// The navigation bar's trailing slot: a phone's row has no room.
        case navigationBar

        init(compact: Bool) { self = compact ? .navigationBar : .outlineRow }
    }

    /// The page's own items in the task's menu: copying every identifier
    /// the Identifiers footer holds, offered while the footer draws (once
    /// the history loads, or while the leading decision names some).
    static func taskActions(
        _ timeline: Components.Schemas.TaskTimeline?, task: TaskTimelinePresentation.TaskIdentifiers,
        decision: [AttentionDisplay.BindingRow] = []
    ) -> [TaskActionsMenu.Action] {
        guard timeline != nil || !decision.isEmpty else { return [] }
        let text = TaskTimelinePresentation.allIdentifiersText(
            TaskTimelinePresentation.identifierGroups(timeline, task: task, decision: decision))
        return [.init(title: "Copy all identifiers") { Clipboard.copy(text) }]
    }

    /// The task's one menu (◆B): its own items, then Stop.
    private func taskActionsMenu(_ timeline: Components.Schemas.TaskTimeline?) -> TaskActionsMenu {
        TaskActionsMenu(
            coordinator: coordinator, taskID: snapshot.task.id,
            actions: Self.taskActions(timeline, task: taskIdentifiers(timeline), decision: decisionIdentifiers),
            confirmation: $stopConfirmation,
            rendersInteractiveControls: screenshotTimeline == nil)
    }

    // MARK: Body

    /// The Current Run section's model, or nil where the page draws none:
    /// a finished task, or one whose history holds no run.
    private func currentRun(
        _ timeline: Components.Schemas.TaskTimeline
    ) -> TaskTimelinePresentation.CurrentRun? {
        TaskTimelinePresentation.currentRunSection(
            snapshot.task, timeline: timeline, runTimelines: coordinator.timelinesByRunID,
            schedules: coordinator.schedules)
    }

    /// Current Run (◆C): the attempt the task is on now, always open, as a
    /// full item: its title, chip, and `Open run history`, the mono line
    /// naming what runs it, its Review, and Run Details. The current
    /// campaign's other attempts follow as one-line items. What it holds
    /// changes with the stage; its place does not.
    private func currentRunSection(
        _ current: TaskTimelinePresentation.CurrentRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 11) {
            KeywordLabel(text: "Current Run")
                .accessibilityAddTraits(.isHeader)
            VStack(alignment: .leading, spacing: 12) {
                item(verticalPadding: 18, horizontalPadding: 20) {
                    VStack(alignment: .leading, spacing: 16) {
                        VStack(alignment: .leading, spacing: 6) {
                            openRunButton(current.run, in: timeline, showsTitle: true)
                            // The run page's face for an invocation's
                            // admission line.
                            if let line = current.line {
                                Text(line)
                                    .font(FreesideFont.trailingSummary)
                                    .foregroundStyle(Color.inkDim)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                        runBody(
                            current.run, in: timeline, showsReview: current.showsReview,
                            schedules: current.schedules)
                    }
                }
                .background(alignment: .top) { jumpTarget(.run(current.run.run_id)) }
                ForEach(current.earlierAttempts, id: \.run_id) { run in
                    item(verticalPadding: 12, horizontalPadding: 18) { priorRun(run, in: timeline) }
                        .background(alignment: .top) { jumpTarget(.run(run.run_id)) }
                }
            }
        }
    }

    /// What the task's Stop says, drawn only while there is a Stop state
    /// to state; Stop itself starts from the Task Actions menu.
    @ViewBuilder private var stopStates: some View {
        let stop = TaskStopView(
            coordinator: coordinator, taskID: snapshot.task.id, explanationExpanded: $showsStopExplanation)
        if !stop.isQuiet { stop }
    }

    /// What the task's line change says, drawn only while there is
    /// something to say; the change itself starts from the Task Actions
    /// menu.
    @ViewBuilder private var lineChangeStates: some View {
        let change = TaskLineChangeView(model: coordinator.taskLineChange, taskID: snapshot.task.id)
        if !change.isQuiet { change }
    }

    /// The threads of items no longer open, newest first, one closed fold
    /// each, labelled by the item's title and summarised by its status, its
    /// message count, and its last message's time. Each opens read-only:
    /// the item is closed, so nothing takes a reply, and the fold's label
    /// already names the thread, so it draws no Conversation keyword.
    private func earlierConversationsSection(
        _ earlier: [TaskTimelinePresentation.EarlierConversation]
    ) -> some View {
        VStack(alignment: .leading, spacing: 11) {
            KeywordLabel(text: "Earlier Conversations")
                .accessibilityAddTraits(.isHeader)
            ForEach(earlier, id: \.conversation.conversation.id) { thread in
                let title = AttentionDisplay.title(thread.item)
                let summary = TaskTimelinePresentation.earlierConversationSummary(
                    thread, now: now, locale: locale, timeZone: timeZone)
                SentenceDisclosure(
                    label: title,
                    summary: summary,
                    spokenLabel: "Conversation, \(title), \(summary)",
                    isExpanded: fold(.conversation(thread.conversation.conversation.id))
                ) {
                    ConversationView(
                        snapshot: thread.conversation,
                        showsKeyword: false,
                        attachments: coordinator.store.attachments,
                        loadsAttachments: screenshotTimeline == nil,
                        now: now,
                        rendersInteractiveControls: screenshotTimeline == nil)
                }
                // Threads are newest first, so a thread closing moves every
                // fold under it.
                .background(alignment: .top) {
                    bodyMarker("conversation:" + thread.conversation.conversation.id)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder
    private func timelineSection(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        if let timeline {
            content(timeline)
        } else if coordinator.taskTimelineLoadStates[snapshot.task.id] == .unavailable {
            UnavailableStateView(
                glyph: .tasks, title: "Timeline unavailable",
                description: "Freeside could not load the daemon's history for this task."
            )
            .frame(maxWidth: .infinity, minHeight: 180)
        } else {
            ProgressView("Loading timeline…")
                .tint(.waterText)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .frame(maxWidth: .infinity, minHeight: 180)
        }
    }

    /// The current hold, set apart under the name so the reason the task
    /// waits is read before its history (visual audit D02). Every hold kind
    /// takes this one treatment: the hold's own words, the status chip, and
    /// the guidance sentence are what tell a capacity wait from a hold that
    /// needs the operator. The stage the task waits in leads, when one is
    /// known, and no round appears. VoiceOver reads the stage, then the hold.
    private func holdCallout(_ hold: TaskDisplay.HoldCallout) -> some View {
        SystemCallout {
            if let stage = hold.stage {
                Text(stage)
                    .font(FreesideFont.cardBody)
            }
            Text(hold.hold)
                .font(FreesideFont.statement)
        }
        .foregroundStyle(Color.ink)
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityElement(children: .combine)
    }

    private func guidanceSentence(_ sentence: String) -> some View {
        Text(sentence)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }

    /// The files and images the operator attached when submitting (#2590),
    /// in submitted order, as one fold in the page head under the
    /// documents line: they are the task's input, as the prompt is. Closed
    /// at first and kept per task; absent when there are none. Images
    /// render inline through the attachment loader. The daemon never
    /// deletes task attachment bytes, so each reference is available. A
    /// folder's files (#2591) fold under one row with its file count and
    /// total size, so a large folder loads nothing until opened.
    @ViewBuilder private var attachmentsFold: some View {
        let attachments = snapshot.task.attachments
        if !attachments.isEmpty {
            SentenceDisclosure(
                label: "Attachments",
                summary: Self.folderSummary(attachments),
                isExpanded: fold(.attachments)
            ) {
                VStack(alignment: .leading, spacing: 11) {
                    ForEach(AttachmentGroup.grouped(attachments, name: \.name)) { group in
                        switch group {
                        case .file(let name, let attachment):
                            attachmentRow(attachment, label: name)
                        case .folder(let name, let files):
                            SentenceDisclosure(
                                label: name,
                                summary: Self.folderSummary(files),
                                spokenLabel: "Folder \(name), \(Self.folderSummary(files))",
                                isExpanded: attachmentFolderFold(name)
                            ) {
                                VStack(alignment: .leading, spacing: 11) {
                                    ForEach(files, id: \.name) { file in
                                        attachmentRow(file, label: String(file.name.dropFirst(name.count + 1)))
                                    }
                                }
                            }
                        }
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// A folder's file count and total size, as its composer chip reads.
    static func folderSummary(_ files: [Components.Schemas.TaskAttachment]) -> String {
        let bytes = files.reduce(0) { $0 + Int(clamping: $1.size_bytes) }
        return "\(files.count.formatted()) \(files.count == 1 ? "file" : "files") · "
            + ComposerAttachmentChip.size(bytes)
    }

    private func attachmentFolderFold(_ name: String) -> Binding<Bool> {
        Binding(
            get: { openAttachmentFolders.contains(name) },
            set: { open in
                if open { openAttachmentFolders.insert(name) } else { openAttachmentFolders.remove(name) }
            })
    }

    private func attachmentRow(_ attachment: Components.Schemas.TaskAttachment, label: String) -> some View {
        DecisionDetailView.AttachmentRow(
            label: label,
            digest: attachment.digest,
            reference: AttachmentReference(
                mediaType: attachment.media_type.rawValue,
                sizeBytes: Int(clamping: attachment.size_bytes),
                availability: .available),
            attachments: coordinator.store.attachments,
            loadsAttachments: screenshotTimeline == nil,
            rendersInteractiveControls: screenshotTimeline == nil,
            boxed: true)
    }

    /// The Timeline is the task's events, always open: the rail that says
    /// which stage the task is in and how it got there, the current
    /// campaign's included, with no fold around them. Current Run follows
    /// the rail, for when the glance is not enough: the rail says where the
    /// task stands, and the run's review and admission say how it is going.
    /// Under it each earlier campaign is one folded disclosure row, and the
    /// page's exact identifiers close it under one hairline.
    private func content(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        let rows = TaskTimelinePresentation.rows(
            timeline,
            reviewRounds: TaskTimelinePresentation.reviewRounds(timeline, runTimelines: coordinator.timelinesByRunID),
            items: taskItems.map(\.item))
        let disambiguate = TaskTimelinePresentation.campaignCount(timeline) > 1
        let rowMarker: ((String) -> AnyView)? = scrollBy == nil ? nil : { holdMarker($0) }
        return VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Timeline")
                    .accessibilityAddTraits(.isHeader)
                if let message = TaskTimelinePresentation.availabilityMessage(
                    state: coordinator.taskTimelineLoadStates[snapshot.task.id],
                    answered: coordinator.answeredTaskTimelineIDs.contains(snapshot.task.id),
                    freshness: coordinator.store.freshness)
                {
                    Text(message)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
                if rows.isEmpty {
                    Text("No recorded events in this history.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                } else {
                    TimelineRail(rows: rows, onOpenItem: onOpenItem, rowMarker: rowMarker)
                }
            }
            if let current = currentRun(timeline) {
                currentRunSection(current, in: timeline)
            }
            if timeline.sections.isEmpty {
                Text("No campaigns or runs in this history.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            ForEach(
                Array(TaskTimelinePresentation.earlierSections(snapshot.task, in: timeline).enumerated()),
                id: \.offset
            ) { _, section in
                let ids = TaskTimelinePresentation.technicalRows(section: section)
                SentenceDisclosure(
                    label: TaskTimelinePresentation.sectionTitle(section, disambiguate: disambiguate),
                    summary: TaskTimelinePresentation.sectionSummary(
                        section, now: now, locale: locale, timeZone: timeZone),
                    isExpanded: fold(.campaign(section.campaign_id)),
                    // A section outside a campaign may hold no id at all.
                    menuItems: ids.isEmpty ? nil : { copyItems(ids) }
                ) {
                    sectionBody(section, in: timeline)
                        .padding(.top, 3)
                }
            }
        }
    }

    /// The page's one hairline, then its one Identifiers fold (R26), as the
    /// run timeline closes its page: every exact id on the page, grouped by
    /// what it names, under everything the page says in words, the leading
    /// card's among them (#2607). Each card keeps a copy menu for its own
    /// ids, so a single copy needs no trip here.
    private func identifiers(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = taskIdentifiers(timeline)
        return TechnicalDetailsSection(
            groups: TaskTimelinePresentation.identifierGroups(timeline, task: task, decision: decisionIdentifiers),
            summary: TaskTimelinePresentation.identifiersSummary(
                timeline, task: task, hasDecision: !decisionIdentifiers.isEmpty),
            isExpanded: identifiersFold
        )
        .padding(.top, 18)
        .overlay(alignment: .top) {
            Color.rule.frame(height: 1)
        }
        // A reveal lands clear of the outline row pinned over the top.
        .background(alignment: .top) {
            Color.clear
                .frame(height: outlineHeight)
                .id(IdentifiersFooter.top)
                .padding(.top, -outlineHeight)
                .accessibilityHidden(true)
        }
    }

    /// The task's own identifiers: its id, its repository when it is
    /// issue-sourced, and the current run's synced policy digest.
    private func taskIdentifiers(
        _ timeline: Components.Schemas.TaskTimeline?
    ) -> TaskTimelinePresentation.TaskIdentifiers {
        let run = TaskTimelinePresentation.currentRun(snapshot.task, in: timeline)
        return .init(
            snapshot.task,
            policyDigest: run.flatMap { current in
                coordinator.runs.first { $0.run.id == current.run_id }?.run.policy_digest
            })
    }

    /// Whether the page draws its Identifiers footer: once the history
    /// loads, whose ids make up most of it, or as soon as a seated card
    /// hands up its own.
    private func hasIdentifiersFooter(_ timeline: Components.Schemas.TaskTimeline?) -> Bool {
        timeline != nil || !decisionIdentifiers.isEmpty
    }

    /// The footer's fold, open while the launch input holds it open as well
    /// as when the operator opened it; closing it ends both.
    private var identifiersFold: Binding<Bool> {
        let fold = fold(.identifiers)
        return Binding(
            get: { fold.wrappedValue || identifiersOpenAtLaunch },
            set: { expanded in
                if !expanded { identifiersOpenAtLaunch = false }
                fold.wrappedValue = expanded
            })
    }

    /// One context-menu item per identifier, each copying the exact value.
    private func copyItems(_ rows: [AttentionDisplay.BindingRow]) -> some View {
        ForEach(Array(rows.enumerated()), id: \.offset) { _, row in
            Button(TaskTimelinePresentation.copyMenuTitle(for: row.label)) { Clipboard.copy(row.value) }
        }
    }

    /// An earlier campaign's runs, each a one-line card that opens to the
    /// full card's content.
    private func sectionBody(
        _ section: Components.Schemas.TaskTimelineSection, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            ForEach(section.runs, id: \.run_id) { run in
                item(verticalPadding: 12, horizontalPadding: 18) { priorRun(run, in: timeline) }
                    .background(alignment: .top) { jumpTarget(.run(run.run_id)) }
            }
        }
    }

    /// A run as a bordered item on the page's ground (R3): the border
    /// alone sets it apart, with no fill of its own.
    private func item<Content: View>(
        verticalPadding: CGFloat, horizontalPadding: CGFloat, @ViewBuilder content: () -> Content
    ) -> some View {
        content()
            .padding(.vertical, verticalPadding)
            .padding(.horizontal, horizontalPadding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    private func chip(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    )
        -> (label: String, cut: StateChip.Cut)?
    {
        let task = snapshot.task
        return TaskTimelinePresentation.runChip(
            run, task: task,
            position: TaskDisplay.position(
                task, runs: coordinator.runs, attentionItems: coordinator.store.orderedSnapshots, history: timeline),
            synced: coordinator.runs.first { $0.run.id == run.run_id }?.run)
    }

    /// The run's way into its own history. With the title it is the full
    /// card's title row; without, it is the link alone under a prior run's
    /// fold. Either way the whole row is the button and says what it said.
    private func openRunButton(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline, showsTitle: Bool
    ) -> some View {
        Button {
            onOpenRun(run.run_id)
        } label: {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    // Under a prior run's fold the link stands alone, so it
                    // leads; beside a title it trails.
                    if showsTitle {
                        runTitle(run, in: timeline, dimmed: false)
                        Spacer(minLength: 8)
                    }
                    FreesideLink(title: "Open run history", face: FreesideFont.noticeAction)
                    if !showsTitle { Spacer(minLength: 0) }
                }
                VStack(alignment: .leading, spacing: 6) {
                    if showsTitle { runTitle(run, in: timeline, dimmed: false) }
                    FreesideLink(title: "Open run history", face: FreesideFont.noticeAction)
                }
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .copyMenu { copyItems(TaskTimelinePresentation.technicalRows(run: run, in: timeline)) }
        .accessibilityLabel(TaskTimelinePresentation.runCardAccessibilityLabel(run, in: timeline))
        .accessibilityHint("Shows this run's recorded milestones and invocation observations.")
    }

    /// The run's title (`Specification`, `Implementation · Attempt N`) with
    /// its chip beside it: the title is fixed vocabulary that cannot wrap,
    /// which is the one case a chip sits beside a title.
    private func runTitle(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline, dimmed: Bool
    ) -> some View {
        WrappingHStack(horizontalSpacing: 10, verticalSpacing: 4) {
            Text(TaskTimelinePresentation.runTitle(run))
                .font(dimmed ? FreesideFont.factLabel : FreesideFont.statement)
                .foregroundStyle(dimmed ? Color.inkDim : Color.ink)
            if let chip = chip(run, in: timeline) {
                StateChip(label: chip.label, cut: chip.cut)
            }
        }
    }

    /// A prior run as one line (chevron, title, faint chip, newest
    /// milestone time) that opens to the full card's content in place.
    private func priorRun(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        let isExpanded = fold(.run(run.run_id))
        return VStack(alignment: .leading, spacing: 16) {
            Button {
                isExpanded.wrappedValue.toggle()
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Image(systemName: "arrowtriangle.right.fill")
                        .font(FreesideFont.disclosureGlyph)
                        .foregroundStyle(Color.accentText)
                        .rotationEffect(.degrees(isExpanded.wrappedValue ? 90 : 0))
                        .accessibilityHidden(true)
                    ViewThatFits(in: .horizontal) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            runTitle(run, in: timeline, dimmed: true)
                            Spacer(minLength: 8)
                            milestoneTime(run)
                        }
                        VStack(alignment: .leading, spacing: 4) {
                            runTitle(run, in: timeline, dimmed: true)
                            milestoneTime(run)
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: 4)
            .copyMenu { copyItems(TaskTimelinePresentation.technicalRows(run: run, in: timeline)) }
            .accessibilityValue(isExpanded.wrappedValue ? "Expanded" : "Collapsed")
            if isExpanded.wrappedValue {
                openRunButton(run, in: timeline, showsTitle: false)
                runBody(
                    run, in: timeline, showsReview: run.role == nil || run.role?.value1 == .implementation)
            }
        }
    }

    @ViewBuilder
    private func milestoneTime(_ run: Components.Schemas.TaskTimelineRun) -> some View {
        if let time = TaskTimelinePresentation.newestMilestoneTime(run) {
            Text(FreesideFormat.shortTime(time, now: now, locale: locale, timeZone: timeZone))
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .exactInstant(time)
        }
    }

    /// A run's stages from its synced snapshot, which name the attempt each
    /// review round reviewed. Empty until the run syncs; labels then fall
    /// back to the attempt's kind.
    private func stages(of runID: String) -> [Components.Schemas.Stage] {
        coordinator.runs.first { $0.run.id == runID }?.run.stages ?? []
    }

    /// Everything under a run's title: its review, then its details. The
    /// run timeline draws its milestones, one click away. `schedules` are
    /// the task's armed schedules, which only the current run's details
    /// carry.
    @ViewBuilder
    private func runBody(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline,
        showsReview: Bool, schedules: [Components.Schemas.ScheduleSnapshot] = []
    ) -> some View {
        if showsReview {
            RunReviewSection(
                coordinator: coordinator, runID: run.run_id,
                facts: coordinator.timelinesByRunID[run.run_id]?.review?.value1,
                stages: stages(of: run.run_id),
                hasTimeline: coordinator.timelinesByRunID[run.run_id] != nil,
                previousRoundLine: true,
                folds: .init(
                    facts: { fold(.roundFacts($0)) }, priorRounds: fold(.priorRounds(run.run_id))))
        }
        if TaskTimelinePresentation.hasRunDetails(run, schedules: schedules) {
            SentenceDisclosure(
                label: "Run Details",
                summary: TaskTimelinePresentation.runDetailsSummary(
                    run, schedules: schedules, now: now, locale: locale, timeZone: timeZone),
                isExpanded: fold(.runDetails(run.run_id))
            ) {
                runDetails(run, in: timeline, schedules: schedules)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            // Under the Review section, whose rounds a refresh adds.
            .background(alignment: .top) { bodyMarker("run-details:" + run.run_id) }
        }
    }

    /// The run's role, lineage, and recorded hold as facts, then each armed
    /// schedule against its next fire time (the kind alone when none is
    /// recorded). A run this history holds is a link to its own timeline;
    /// one it doesn't hold is named by its short id, with nowhere to go.
    private func runDetails(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline,
        schedules: [Components.Schemas.ScheduleSnapshot]
    ) -> some View {
        FactGroup(spacing: 8) {
            if let role = run.role?.value1 {
                FactRow(label: "Role", value: TaskTimelinePresentation.label(role))
            }
            if let reason = run.attempt_reason {
                FactRow(label: "Reason", value: reason)
            }
            if let parent = run.parent_run_id {
                runFact("Parent", runID: parent, in: timeline)
            }
            if let successor = run.superseded_by {
                runFact("Superseded By", runID: successor, in: timeline)
            }
            if let hold = run.hold?.value1 {
                FactRow(label: "Recorded Hold", value: RunDisplay.label(hold.reason))
                if let fact = RunDisplay.holdFact(hold, runID: run.run_id) {
                    FactRow(label: "Hold Detail", value: fact)
                }
                FactRow(label: "Hold Code", value: hold.reason.rawValue, valueColor: .inkDim)
            }
            ForEach(schedules, id: \.schedule.id) { snapshot in
                let schedule = snapshot.schedule
                if let fireAt = schedule.fire_at {
                    FactRow(
                        label: RunDisplay.label(schedule.kind),
                        value: FreesideFormat.shortTime(fireAt, now: now, locale: locale, timeZone: timeZone)
                    )
                    .exactInstant(fireAt)
                } else {
                    Text(RunDisplay.label(schedule.kind))
                        .font(FreesideFont.factLabel)
                }
            }
        }
    }

    @ViewBuilder
    private func runFact(
        _ label: String, runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        if let title = TaskTimelinePresentation.runLinkTitle(runID, in: timeline) {
            FactLinkRow(label: label, value: title) { onOpenRun(runID) }
        } else {
            FactRow(label: label, value: ShortIdentifier.short(runID))
        }
    }
}

/// Where a jump lands, and the reader's place the page keeps: a section, or
/// a run inside the Timeline.
extension EnvironmentValues {
    /// Draws a hold point of the task page a seated card sits in, keyed
    /// within the card, so a reader inside the card holds their place as
    /// the card changes above them (#1460). Nil outside a page.
    @Entry var taskPageHoldMarker: (@MainActor (String) -> AnyView)? = nil
}

enum TaskPageAnchor: Hashable {
    case section(TaskPageSection)
    case run(String)

    /// The anchor the reader is at, from each jump target's top measured
    /// from the outline row's top. A jump target sits the row's height above
    /// its view, so a target at or above the row's top has its view under
    /// the pinned row; the lowest such target is the one being read. Nil
    /// while every target is below the row, which is where the row sits
    /// while the header is in view.
    static func reading(offsets: [TaskPageAnchor: CGFloat]) -> TaskPageAnchor? {
        // A jump lands its target on the row's top, give or take rounding.
        let tolerance: CGFloat = 1
        return offsets.filter { $0.value <= tolerance }.max { $0.value < $1.value }?.key
    }
}

extension EnvironmentValues {
    /// The task page's scroll target for the leading card's thread, set
    /// on the card while its thread is where the outline's Conversation
    /// chip goes. Nil everywhere else, so an unseated card draws nothing
    /// for it.
    @Entry var taskPageCardThreadTarget: AnyView? = nil
}

/// Draws the task page's Conversation chip target on a seated card's
/// thread, and nothing anywhere else.
struct TaskPageConversationAnchor: View {
    @Environment(\.taskPageCardThreadTarget) private var target

    var body: some View {
        if let target { target }
    }
}

/// What a hosted test reads of a mounted task page, which builds no
/// accessibility tree for a test host to find: where its hold points sit in
/// the viewport, the chip it selects, the one-line rows it draws, and the
/// folds it holds in view state rather than in preferences. Its closures
/// act as the operator would. Nil outside tests.
@MainActor final class TaskPagePlaceProbe {
    /// The fold `openStateFolds` names What Happened by; attachment
    /// folders go by their names.
    static let whatHappened = "What Happened"

    /// Each hold point's top in the page's own space, by jump target or
    /// rail key, and the page's top in the scroll view.
    var holdTops: [String: CGFloat] = [:]
    var pageTop: CGFloat = 0
    /// The outline row's top in the scroll view.
    var rowTop: CGFloat = 0
    /// Each hold point's top in the scroll view, where the reader sees it.
    var viewTops: [String: CGFloat] { holdTops.mapValues { $0 + pageTop } }
    var selectedSection: TaskPageSection?
    var oneLineItemIDs: Set<String> = []
    var openStateFolds: Set<String> = []
    /// Chooses an outline chip.
    var select: ((TaskPageSection) -> Void)?
    /// Opens What Happened or an attachment folder.
    var openStateFold: ((String) -> Void)?

    /// The point the reader has under the outline row, by the rule the page
    /// holds by, and its top.
    var reading: (key: String, top: CGFloat)? {
        viewTops.filter { $0.value - rowTop <= 1 }.max { $0.value < $1.value }.map { ($0.key, $0.value) }
    }
}

extension View {
    /// Reports `point`'s top in the task page's own space while the view
    /// draws, and nil when it stops, for holding the reader's place. Nil
    /// adds nothing.
    @ViewBuilder
    fileprivate func holdsPlace(
        _ point: TaskPageView.HoldPoint?, report: @escaping (TaskPageView.HoldPoint, UUID, CGFloat?) -> Void
    ) -> some View {
        if let point {
            modifier(HoldMarker(point: point, report: report))
        } else {
            self
        }
    }
}

/// A hold point's reports, signed by the marker that sends them, so a
/// marker leaving after another took its point doesn't clear that point.
private struct HoldMarker: ViewModifier {
    let point: TaskPageView.HoldPoint
    let report: (TaskPageView.HoldPoint, UUID, CGFloat?) -> Void
    @State private var reporter = UUID()

    func body(content: Content) -> some View {
        content
            .onGeometryChange(for: CGFloat.self, of: { $0.frame(in: .named(TaskPageView.pageSpace)).minY }) {
                report(point, reporter, $0)
            }
            .onDisappear { report(point, reporter, nil) }
    }
}

/// Hosts the page in its one scroll view and keeps its history fresh. The
/// leading card is content in the page, seated without a scroll view of its
/// own, so a card arriving, leaving, or handing the lead to another leaves
/// the page mounted; only the card is replaced. It also holds the scroll
/// position the page moves to keep the reader's place.
struct TaskPageHost: View {
    /// The scroll view's content offset and its range, kept off observed
    /// state so scrolling doesn't redraw the host, and the offset a
    /// correction asked for until the scroll view reaches it.
    @MainActor private final class Offset {
        var y: CGFloat = 0
        var range: ClosedRange<CGFloat> = 0...0
        var asked: CGFloat?

        /// True when the offset is where `asked` lands, within the range,
        /// to the point the page holds to: a fractional move lands rounded
        /// to the display's pixels.
        func reaches(_ asked: CGFloat) -> Bool {
            abs(min(max(asked, range.lowerBound), range.upperBound) - y) <= 1
        }
    }

    private struct ScrollReading: Equatable {
        var y: CGFloat
        var range: ClosedRange<CGFloat>
    }

    let coordinator: SyncCoordinator
    let snapshot: Components.Schemas.TaskSnapshot
    let leadingItemID: String?
    var topMargin: CGFloat? = nil
    var memory: TaskPageMemory? = nil
    let onOpenRun: (String) -> Void
    let onOpenItem: (String) -> Void
    /// The launch input that opens a card's identifiers; on a page with a
    /// card it opens the footer that holds them.
    var identifiersOpenAtLaunch = false
    /// Which folds are open. Nil uses the device's shared preferences; a
    /// test passes its own.
    var disclosures: TaskTimelineDisclosurePreferences? = nil
    var probe: TaskPagePlaceProbe? = nil
    /// Builds the leading item's card, seated in the page's scroll view.
    let card: (String, DecisionDetailView.PageSeat) -> DecisionDetailView
    /// The seated card's identifier rows, by item id. The card builds them
    /// from facts its model fetches and publishes them here, because the
    /// footer that lists them belongs to the page.
    @State private var seatedIdentifiers: [String: [AttentionDisplay.BindingRow]] = [:]
    @State private var identifiersReveals = 0
    @State private var position = ScrollPosition()
    @State private var offset = Offset()
    #if os(iOS)
        @State private var footerHeight: CGFloat = 0
    #endif

    private var title: String { TaskDisplay.projectName(snapshot.task) }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                TaskPageView(
                    coordinator: coordinator, snapshot: snapshot, onOpenRun: onOpenRun, onOpenItem: onOpenItem,
                    leadingItemID: leadingItemID,
                    leadingCard: leadingItemID.map { AnyView(card($0, seat(proxy)).id($0)) },
                    scrollProxy: proxy,
                    scrollBy: scroll(by:), forgetCorrections: { offset.asked = nil }, probe: probe,
                    decisionIdentifiers: leadingItemID.flatMap { seatedIdentifiers[$0] } ?? [],
                    identifiersReveals: identifiersReveals,
                    identifiersOpenAtLaunch: identifiersOpenAtLaunch && leadingItemID != nil,
                    topMargin: topMargin, memory: memory, disclosures: disclosures)
            }
            .coordinateSpace(name: DecisionDetailView.scrollSpace)
            .scrollPosition($position)
            .onScrollGeometryChange(
                for: ScrollReading.self,
                of: { geometry in
                    let top = -geometry.contentInsets.top
                    let bottom =
                        geometry.contentSize.height + geometry.contentInsets.bottom
                        - geometry.containerSize.height
                    return ScrollReading(y: geometry.contentOffset.y, range: top...max(top, bottom))
                }
            ) { _, reading in
                offset.y = reading.y
                offset.range = reading.range
                // An operator's scroll replaces any move still on its way.
                if position.isPositionedByUser { offset.asked = nil }
                if let asked = offset.asked, offset.reaches(asked) { offset.asked = nil }
            }
            #if os(iOS)
                // The seated card's sticky recommended action, drawn over
                // room the scroll view leaves for it, as the card alone
                // draws its own in a safe-area inset.
                .safeAreaInset(edge: .bottom, spacing: 0) { Color.clear.frame(height: footerHeight) }
                .overlayPreferenceValue(TaskPageFooterKey.self, alignment: .bottom) { footer in
                    VStack(spacing: 0) { footer }
                    .onGeometryChange(for: CGFloat.self, of: \.size.height) { footerHeight = $0 }
                }
            #endif
        }
        .navigationTitle(title)
        .task(
            id: TaskPageView.TimelineRequestKey(
                snapshot: snapshot, cursors: coordinator.cursors, cacheGeneration: coordinator.cacheGeneration)
        ) {
            await coordinator.refreshTaskTimeline(for: snapshot.task.id)
        }
        .task(id: coordinator.taskReviewRequestKey(for: snapshot.task.id, revision: snapshot.as_of_revision)) {
            await coordinator.refreshTaskReviews(for: snapshot.task.id, revision: snapshot.as_of_revision)
        }
    }

    /// Moves the offset by `distance` with animation off. The next move
    /// can come before the scroll view reaches this one, and the scroll
    /// view can report the offset it had meanwhile, so a move builds on the
    /// offset last asked for until the scroll view reaches it.
    private func scroll(by distance: CGFloat) {
        let asked = (offset.asked ?? offset.y) + distance
        // A move back to where the scroll view already is draws no change
        // for it to report.
        offset.asked = offset.reaches(asked) ? nil : asked
        var transaction = Transaction()
        transaction.disablesAnimations = true
        withTransaction(transaction) { position.scrollTo(y: asked) }
    }

    private func seat(_ proxy: ScrollViewProxy) -> DecisionDetailView.PageSeat {
        DecisionDetailView.PageSeat(
            scrollProxy: proxy,
            // Only the card that leads keeps rows here: the page lists one.
            publishIdentifiers: { itemID, rows in seatedIdentifiers = [itemID: rows] },
            revealIdentifiers: { identifiersReveals += 1 })
    }
}

#if os(iOS)
    /// The sticky recommended action a seated card hands up to the task page
    /// (`DecisionDetailView.PageSeat`), or nil while its own button shows.
    /// The page reads it with each card update, so the button it draws is
    /// never a stale copy.
    struct TaskPageFooterKey: PreferenceKey {
        static var defaultValue: AnyView? { nil }

        static func reduce(value: inout AnyView?, nextValue: () -> AnyView?) {
            value = value ?? nextValue()
        }
    }
#endif

/// Current projections retain daemon ordering. Legacy cached projections
/// recover their nested events in newest-first order.
enum TaskTimelinePresentation {
    /// Older saved projections put only creation at task level. Recover their
    /// recorded nested events without changing the current daemon projection.
    static func events(_ timeline: Components.Schemas.TaskTimeline) -> [Components.Schemas.TaskEvent] {
        guard timeline.events.allSatisfy({ $0.kind == .task_created }) else { return timeline.events }
        var events = timeline.events
        for section in timeline.sections {
            for event in section.events + section.runs.flatMap(\.events) where !events.contains(event) {
                events.append(event)
            }
        }
        return events.enumerated().sorted {
            if $0.element.recorded_at != $1.element.recorded_at {
                return $0.element.recorded_at > $1.element.recorded_at
            }
            return $0.offset < $1.offset
        }.map(\.element)
    }

    /// A cached partition remains useful after a failed refresh, but neither
    /// its contents nor an empty array establish current, complete history.
    /// History the daemon has `answered` this session is current while
    /// freshness holds, so a routine refetch of it shows no notice; only a
    /// failed one does.
    static func availabilityMessage(
        state: SyncCoordinator.TimelineLoadState?, answered: Bool, freshness: InboxStore.Freshness
    ) -> String? {
        if answered, !freshness.isFailing {
            return state == .unavailable ? "Task history refresh failed. Showing saved history." : nil
        }
        if state == .loading { return "Showing saved task history while refreshing…" }
        if state == .unavailable { return "Task history refresh failed. Showing saved history." }
        if state != .loaded || freshness.isFailing { return "Saved task history. Freshness unconfirmed." }
        return nil
    }

    /// The page's elements flattened in render order, so a test can pin the
    /// order without a view: the event rail's rows by their words, then the
    /// current run and the current campaign's other attempts, then each
    /// earlier campaign and its runs.
    enum Entry: Equatable {
        case row(String)
        case section(campaignID: String?)
        case run(String)
    }

    static func entries(_ timeline: Components.Schemas.TaskTimeline, task: Components.Schemas.Task) -> [Entry] {
        var entries: [Entry] = rows(timeline).map { .row($0.label) }
        if let current = currentRunSection(task, timeline: timeline, runTimelines: [:], schedules: []) {
            entries += ([current.run] + current.earlierAttempts).map { .run($0.run_id) }
        }
        for section in earlierSections(task, in: timeline) {
            entries.append(.section(campaignID: section.campaign_id))
            entries += section.runs.map { .run($0.run_id) }
        }
        return entries
    }

    /// Where the walk in `rows(_:)` stands: a stage within one campaign. A
    /// new campaign is a new position even when its first stage repeats the
    /// last one, so a revised specification gets its own start point.
    private struct StagePosition: Equatable {
        let stage: Components.Schemas.StageName
        let campaignID: String?
    }

    /// The event rail's rows, newest first in the order `events(_:)` gives.
    /// Nothing here sorts, so events at one instant keep the daemon's order.
    ///
    /// The daemon records no stage starts, so they are read off its events
    /// (devlog/2026-10-11-1140-task-stage-timeline.md). Walking oldest to
    /// newest, a run's submission enters its role's stage and a review
    /// request enters Review, and a verification result or a publication
    /// milestone enters Verification. When that changes where the task
    /// stands, the event's own row becomes the start point: one row with
    /// the start marker, the stage first and the event's words after it
    /// (`Review started · Round 1 requested`), at the event's time and with
    /// its item link. A submission's words drop their own `started`
    /// (`Implementation started · Attempt 1`). Verification's start
    /// therefore carries the time of the first event that moved the
    /// task there, which trails the real start by the daemon's own
    /// verification (devlog/2026-10-23-0856-verification-start-point.md).
    /// No other event moves the stage: an attempt's result can be recorded
    /// after the review of its work was requested, and must not read as a
    /// return to implementation.
    ///
    /// A remediation is a return to Implementation. `reviewRounds` holds
    /// each run's review rounds by run id; a round whose findings went to a
    /// remediator names its invocation. That invocation's start draws
    /// "Remediation started", or `Implementation started · Remediation`
    /// as a start point when it returns the task there, and the decision
    /// that sent the findings there directly under it, at the decision's
    /// own time. Until the remediator starts, the
    /// decision sits where its time falls. The remediator's result reads as the
    /// remediation's and is not counted as a pass of the attempt.
    ///
    /// Three kinds of event are machinery and leave the rail: a campaign's
    /// allocation, any other invocation's start, and the result of a review
    /// round's own invocation, which the round's entry already states.
    ///
    /// `items` are the task's items the client holds. A row that records a
    /// decision links to its item's card: by the id the event names where it
    /// names one, and otherwise by a join the wire doesn't promise, so a row
    /// with no match stays plain (`linkedItemID`).
    static func rows(
        _ timeline: Components.Schemas.TaskTimeline,
        reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:],
        items: [Components.Schemas.AttentionItem] = []
    ) -> [TimelineRail.Row] {
        let events = events(timeline)
        var runs: [String: (run: Components.Schemas.TaskTimelineRun, campaignID: String?)] = [:]
        for section in timeline.sections {
            for run in section.runs where runs[run.run_id] == nil {
                runs[run.run_id] = (run, section.campaign_id)
            }
        }
        let reviewInvocations = Set(events.compactMap { $0.review?.value1.invocation_id })
        func remediated(_ milestone: Components.Schemas.RunMilestone?) -> Components.Schemas.RunReviewRound? {
            guard let milestone, let invocation = milestone.invocation_id else { return nil }
            return reviewRounds[milestone.run_id]?.first { $0.remediation?.value1.invocation_id == invocation }
        }
        let remediatorStarts = Set(
            events.compactMap { event -> String? in
                guard let milestone = event.milestone?.value1, milestone.kind == .invocation_started,
                    remediated(milestone) != nil
                else { return nil }
                return milestone.invocation_id
            })
        // A decision whose remediator has not started sits where its own
        // time falls among the events: the head of the rail at first, and
        // under whatever happens after it, such as a stop that means the
        // remediator never starts. One with no recorded time leads the rail.
        var unstarted = timeline.sections.reversed().flatMap { $0.runs.reversed() }
            .flatMap { run in
                (reviewRounds[run.run_id] ?? []).compactMap { round in
                    round.remediation.map { (round: round, remediation: $0.value1) }
                }
            }
            .filter { !remediatorStarts.contains($0.remediation.invocation_id) }
            .enumerated()
            .sorted {
                ($0.element.remediation.decided_at ?? .distantFuture, $0.offset)
                    < ($1.element.remediation.decided_at ?? .distantFuture, $1.offset)
            }
            .map(\.element)
        var position: StagePosition?
        var results: [String: Int] = [:]
        var startedRemediations: Set<String> = []
        var oldestFirst: [TimelineRail.Row] = []
        for event in events.reversed() {
            while let next = unstarted.first, let decidedAt = next.remediation.decided_at,
                decidedAt < event.recorded_at
            {
                oldestFirst.append(decidedRow(next.round, next.remediation, items: items))
                unstarted.removeFirst()
            }
            let milestone = event.milestone?.value1
            let source = (event.run_id ?? milestone?.run_id).flatMap { runs[$0] }
            var pass = 1
            var start: Components.Schemas.StageName?
            switch (event.kind, milestone?.kind) {
            case (_, .invocation_started):
                guard let round = remediated(milestone), let remediation = round.remediation?.value1,
                    startedRemediations.insert(remediation.invocation_id).inserted
                else { continue }
                oldestFirst.append(decidedRow(round, remediation, items: items))
                let next = StagePosition(stage: .implementation, campaignID: source?.campaignID)
                let starts = next != position
                position = next
                oldestFirst.append(
                    TimelineRail.Row(
                        kind: starts ? .stageStart(.implementation) : .event,
                        label: (starts ? "Implementation started · Remediation" : "Remediation started")
                            + " · Round \(round.round)",
                        recordedAt: event.recorded_at))
                continue
            case (.campaign_allocated, _):
                continue
            case (_, .run_submitted):
                let next = source?.run.role.map {
                    StagePosition(stage: stage($0.value1), campaignID: source?.campaignID)
                }
                if let next, next != position { start = next.stage }
                // A run with no role has no stage to enter, and leaves the
                // one before it: what follows is not known to continue it.
                position = next
            case (_, .terminal_recorded):
                if let milestone, remediated(milestone) != nil {
                    oldestFirst.append(
                        TimelineRail.Row(
                            kind: recordsFailure(event) ? .failure : .event,
                            label: "Remediation " + (terminalWord(milestone) ?? "ended"),
                            recordedAt: event.recorded_at))
                    continue
                }
                if let invocation = milestone?.invocation_id, reviewInvocations.contains(invocation) { continue }
                if let runID = milestone?.run_id {
                    pass = results[runID, default: 0] + 1
                    results[runID] = pass
                }
            case (.review_requested, _), (.external_review_reentered, _):
                // An external review re-entry opens the round its request
                // later names, so the withdrawn ready card has its cause.
                let next = StagePosition(stage: .review, campaignID: source?.campaignID)
                if next != position { start = .review }
                position = next
            case (.verification_recorded, _), (_, .publication_ready), (_, .publication_blocked):
                let next = StagePosition(stage: .verification, campaignID: source?.campaignID)
                if next != position { start = .verification }
                position = next
            default:
                break
            }
            // A start event names nothing that failed (`recordsFailure`), so
            // the merged row never needs the failure kind.
            let words = rowLabel(event, run: source?.run, pass: pass, startsStage: start != nil)
            oldestFirst.append(
                TimelineRail.Row(
                    kind: start.map { .stageStart($0) } ?? (recordsFailure(event) ? .failure : .event),
                    label: start.map { stage in
                        ["\(AttentionDisplay.label(stage)) started", words].compactMap { $0 }.joined(separator: " · ")
                    } ?? words ?? "",
                    recordedAt: event.recorded_at, context: event.triage?.value1.reason,
                    itemID: linkedItemID(event, items: items)))
        }
        oldestFirst += unstarted.map { decidedRow($0.round, $0.remediation, items: items) }
        var rows = Array(oldestFirst.reversed())
        if !rows.isEmpty { rows[0].isNewest = true }
        return rows
    }

    /// The decision that sent a round's findings to a remediator, with the
    /// findings the operator declined when the record counts them.
    private static func decidedRow(
        _ round: Components.Schemas.RunReviewRound, _ remediation: Components.Schemas.ReviewRoundRemediation,
        items: [Components.Schemas.AttentionItem]
    ) -> TimelineRail.Row {
        var label = "Round \(round.round) findings decided · \(remediation.finding_ids.count) to remediation"
        if let declined = round.dispositions?.value1.declined, declined > 0 { label += " · \(declined) declined" }
        // The round names no item: the adjudication that sent the findings
        // there is the one decided at the same recorded instant. The daemon
        // copies that instant from the item (`findingAdjudicationDecidedAt`).
        let item = remediation.decided_at.flatMap { decidedAt in
            items.first {
                [.finding_adjudication, .review_dispute].contains($0._type) && $0.decided_at == decidedAt
            }
        }
        return TimelineRail.Row(kind: .event, label: label, recordedAt: remediation.decided_at, itemID: item?.id)
    }

    /// The item an event records a decision on, if the client holds it. A
    /// verification and an external review name theirs. A specification
    /// approval names none, so it joins to the decided approval on the run
    /// that specified it. A request for changes supersedes its item on the
    /// same run, so only the resolved one approved it.
    private static func linkedItemID(
        _ event: Components.Schemas.TaskEvent, items: [Components.Schemas.AttentionItem]
    ) -> String? {
        if let named = event.verification?.value1.item_id ?? event.external_review?.value1.item_id {
            return items.contains { $0.id == named } ? named : nil
        }
        guard event.kind == .specification_approved, let runID = event.specification_run_id else { return nil }
        return items.first { item in
            guard item._type == .spec_approval, item.status == .resolved, case .run(let subject) = item.subject
            else { return false }
            return subject.run_id == runID
        }?.id
    }

    /// Each run's review rounds by run id, for `rows(_:reviewRounds:)`:
    /// the rounds of every run in `timeline` whose own timeline is loaded.
    static func reviewRounds(
        _ timeline: Components.Schemas.TaskTimeline, runTimelines: [String: Components.Schemas.RunTimeline]
    ) -> [String: [Components.Schemas.RunReviewRound]] {
        var rounds: [String: [Components.Schemas.RunReviewRound]] = [:]
        for run in timeline.sections.flatMap(\.runs) {
            if let review = runTimelines[run.run_id]?.review?.value1 { rounds[run.run_id] = review.rounds }
        }
        return rounds
    }

    /// The run the page's status line and Current Run name: the task's
    /// current one, else the history's newest.
    static func currentRun(
        _ task: Components.Schemas.Task, in timeline: Components.Schemas.TaskTimeline?
    ) -> Components.Schemas.TaskTimelineRun? {
        let runs = timeline?.sections.flatMap(\.runs) ?? []
        return runs.first { $0.run_id == task.current_position?.value1.run_id } ?? runs.first
    }

    /// The index of the current campaign: the section holding the current
    /// run, else the history's first. Nil for a history with no section.
    private static func currentSectionIndex(
        _ task: Components.Schemas.Task, in timeline: Components.Schemas.TaskTimeline
    ) -> Int? {
        guard !timeline.sections.isEmpty else { return nil }
        let runID = currentRun(task, in: timeline)?.run_id
        return timeline.sections.firstIndex { $0.runs.contains { $0.run_id == runID } } ?? 0
    }

    /// Every campaign but the current one, newest first: each folds below
    /// the rail. The current campaign has no fold; its attempts are the
    /// rail's own events, and Current Run's while the task is active.
    static func earlierSections(
        _ task: Components.Schemas.Task, in timeline: Components.Schemas.TaskTimeline
    ) -> [Components.Schemas.TaskTimelineSection] {
        let current = currentSectionIndex(task, in: timeline)
        return timeline.sections.indices.filter { $0 != current }.map { timeline.sections[$0] }
    }

    /// What the Current Run section holds (◆C).
    struct CurrentRun {
        let run: Components.Schemas.TaskTimelineRun
        /// What runs it, in the run page's grammar; nil when nothing names it.
        let line: String?
        /// Whether its Review draws: an implementation run whose timeline
        /// holds a round.
        let showsReview: Bool
        /// Every armed schedule the task has, which its Run Details carry.
        let schedules: [Components.Schemas.ScheduleSnapshot]
        /// The current campaign's other runs, in section order.
        let earlierAttempts: [Components.Schemas.TaskTimelineRun]
    }

    /// Current Run for an active task whose loaded history holds a current
    /// run; nil otherwise. A finished task has none: the rail names its
    /// newest campaign's runs and each run page holds the rest.
    static func currentRunSection(
        _ task: Components.Schemas.Task, timeline: Components.Schemas.TaskTimeline?,
        runTimelines: [String: Components.Schemas.RunTimeline],
        schedules: [Components.Schemas.ScheduleSnapshot]
    ) -> CurrentRun? {
        guard TaskDisplay.isActive(task), let timeline, let run = currentRun(task, in: timeline),
            let index = currentSectionIndex(task, in: timeline)
        else { return nil }
        let runTimeline = runTimelines[run.run_id]
        let reviews = run.role == nil || run.role?.value1 == .implementation
        return CurrentRun(
            run: run,
            line: currentRunLine(task, run: run, runTimeline: runTimeline),
            showsReview: reviews && runTimeline?.review?.value1.rounds.isEmpty == false,
            schedules: TaskDisplay.armedSchedules(for: task, in: schedules),
            earlierAttempts: timeline.sections[index].runs.filter { $0.run_id != run.run_id })
    }

    /// Current Run's mono line, in the run page's grammar: the harness,
    /// model, and effort of the run's latest execution admission for its
    /// own role (specifier for a specification run, implementer otherwise),
    /// then that role's pool, then the operating mode, skipping any fact
    /// the admission did not record. With no admission to read (the run's
    /// timeline not loaded, or nothing admitted yet) it is the role's task
    /// line, agent and pool: the nearest fact the client holds.
    static func currentRunLine(
        _ task: Components.Schemas.Task, run: Components.Schemas.TaskTimelineRun,
        runTimeline: Components.Schemas.RunTimeline?
    ) -> String? {
        let specifies = run.role?.value1 == .specification
        let line = task.task_lines.first { $0.role == (specifies ? .specifier : .implementer) }
        let pool = (line?.provenance?.value1.pool).map { "pool \($0)" }
        let parts: [String?]
        if let admission = runTimeline.flatMap({
            RunDisplay.latestExecutionAdmission($0, role: specifies ? .specifier : .implementer)
        }) {
            parts = [
                RunDisplay.admissionLine(admission), pool,
                admission.operating_mode.map { RunDisplay.label($0.value1).lowercased() },
            ]
        } else {
            parts = [line.flatMap { TaskDisplay.lineLabel(agent: $0.agent, selection: $0.selection?.value1) }, pool]
        }
        let present = parts.compactMap { $0 }
        return present.isEmpty ? nil : present.joined(separator: " · ")
    }

    /// Where choosing a run from the Runs chip goes.
    enum RunTarget: Equatable {
        /// The current run's card in Current Run.
        case currentRun
        /// An earlier attempt's fold in Current Run, opened.
        case earlierAttempt
        /// A run in an earlier campaign's fold, with both folds opened.
        case earlierCampaign(String?)
        /// No card on the page draws the run, so its run page opens.
        case runPage
    }

    static func runTarget(
        _ runID: String, task: Components.Schemas.Task, timeline: Components.Schemas.TaskTimeline
    ) -> RunTarget {
        if let current = currentRunSection(task, timeline: timeline, runTimelines: [:], schedules: []) {
            if current.run.run_id == runID { return .currentRun }
            if current.earlierAttempts.contains(where: { $0.run_id == runID }) { return .earlierAttempt }
        }
        if let section = earlierSections(task, in: timeline).first(where: {
            $0.runs.contains { $0.run_id == runID }
        }) {
            return .earlierCampaign(section.campaign_id)
        }
        return .runPage
    }

    /// The pull request the task opened, by the history's newest record of it.
    static func pullRequest(_ timeline: Components.Schemas.TaskTimeline) -> Int? {
        events(timeline).first { $0.kind == .pr_opened }?.pr_number
    }

    /// One part of the header's documents line (◆D), in the order it draws.
    enum DocumentLink: Equatable {
        /// The intake issue, linked when its repository names a GitHub URL.
        case issue(number: Int, url: URL?)
        /// The text a typed task was submitted with, by its work-item
        /// artifact's digest.
        case prompt(digest: String)
        /// The current campaign's approved specification, by the digest
        /// its approval recorded; the version only when a held approval
        /// names that digest.
        case specification(digest: String, version: Int?)
        /// The task's PR, linked only when a held item names its repository.
        case pullRequest(number: Int, url: URL?)
        /// A loaded history that records no PR.
        case noPullRequest

        /// The words the line draws.
        var title: String {
            switch self {
            case .issue(let number, _): "Issue #\(number)"
            case .prompt: "Prompt"
            case .specification(_, let version): version.map { "Specification v\($0)" } ?? "Specification"
            case .pullRequest(let number, _): "Pull request #\(number)"
            case .noPullRequest: "No pull request yet"
            }
        }

        /// What VoiceOver reads for the words, without the number sign.
        var spokenTitle: String {
            switch self {
            case .issue(let number, _): "Issue \(number)"
            case .prompt: title
            case .specification(_, let version):
                version.map { "Specification version \($0)" } ?? "Specification"
            case .pullRequest(let number, _): "Pull request \(number)"
            case .noPullRequest: title
            }
        }

        /// Where the link goes, as VoiceOver's hint. Nil for plain text:
        /// a part with nowhere to go is not a link.
        var accessibilityHint: String? {
            switch self {
            case .issue(_, let url), .pullRequest(_, let url): url == nil ? nil : "Opens on GitHub"
            case .prompt: "Opens the prompt reader"
            case .specification: "Opens the specification reader"
            case .noPullRequest: nil
            }
        }

        /// The reader the part opens, nil for a part that opens none.
        var readerRequest: TaskDocumentRequest? {
            switch self {
            case .prompt(let digest): .init(digest: digest, document: .prompt)
            case .specification(let digest, let version):
                .init(digest: digest, document: .specification(version: version))
            case .issue, .pullRequest, .noPullRequest: nil
            }
        }
    }

    /// The documents line under the header's status: Issue for an
    /// issue-sourced task or Prompt for a typed one, the approved
    /// Specification, and the Pull Request.
    /// The PR is the current run's (its completion, else the history's
    /// `pr_opened`). The task names no repository, so the PR's link comes
    /// from an item held under the task whose `pr_reference` has its number;
    /// with none it is plain text. Before the history loads, a part the
    /// history decides is left out rather than guessed.
    @MainActor static func documentLinks(
        _ task: Components.Schemas.Task, timeline: Components.Schemas.TaskTimeline?,
        completion: Components.Schemas.WorkUnitCompletionFacts?, items: [Components.Schemas.AttentionItem]
    ) -> [DocumentLink] {
        var links: [DocumentLink] = []
        switch task.source?.value1 {
        case .issue_subject(let source)?:
            links.append(
                .issue(
                    number: source.issue_subject.issue_number,
                    url: DecisionModel.issueURL(for: source.issue_subject)))
        case .work_item_artifact(let source)?:
            links.append(.prompt(digest: source.work_item_artifact_digest.value1))
        case nil:
            break
        }
        if let digest = approvedSpecificationDigest(task, in: timeline) {
            let version = items.first { item in
                item._type == .spec_approval && DecisionDetailView.specificationClaim(in: item)?.digest == digest
            }.flatMap(DecisionDetailView.specificationVersion(in:))
            links.append(.specification(digest: digest, version: version))
        }
        if let number = completion?.pr_number ?? timeline.flatMap(pullRequest) {
            let reference = items.lazy.compactMap { $0.pr_reference?.value1 }.first { $0.number == number }
            links.append(.pullRequest(number: number, url: reference.flatMap(DecisionModel.pullRequestURL(for:))))
        } else if timeline != nil {
            links.append(.noPullRequest)
        }
        return links
    }

    /// The digest the current campaign's approval recorded: the
    /// `specification_approved` event in the section holding the current
    /// run. Nil until that campaign's specification is approved.
    static func approvedSpecificationDigest(
        _ task: Components.Schemas.Task, in timeline: Components.Schemas.TaskTimeline?
    ) -> String? {
        guard let timeline, let run = currentRun(task, in: timeline),
            let section = timeline.sections.first(where: { $0.runs.contains { $0.run_id == run.run_id } })
        else { return nil }
        return section.events.first { $0.kind == .specification_approved }?.approved_spec_digest?.value1
    }

    /// The merge a finished task's run records.
    static func completion(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot]
    ) -> Components.Schemas.WorkUnitCompletionFacts? {
        task.run_ids.lazy.compactMap { id in
            runs.first { $0.run.id == id }?.run.completion?.value1
        }.first
    }

    /// The choice the task's line for `stage` records, and its pool. The line
    /// is what the next admission reads: the wire names no agent for a run
    /// in progress, so this is the nearest fact, and a remediation reads as
    /// its stage's implementer. The daemon records a pool only on a router
    /// line, as the pool its routing chose.
    private static func agentAndPool(
        _ task: Components.Schemas.Task, stage: Components.Schemas.StageName?
    ) -> [String] {
        let role: Components.Schemas.TaskLineRole =
            switch stage {
            case .specification: .specifier
            case .review: .reviewer
            case .implementation, .verification, nil: .implementer
            }
        guard let line = task.task_lines.first(where: { $0.role == role }) else { return [] }
        return [
            TaskDisplay.lineLabel(agent: line.agent, selection: line.selection?.value1),
            line.provenance.map { "pool \($0.value1.pool)" },
        ]
        .compactMap { $0 }
    }

    /// The header's one status line, which reads the same words the task's
    /// list row keys on. No resume time reaches the client, so a parked task
    /// says what parked it and not until when.
    static func statusLine(
        _ task: Components.Schemas.Task, position: TaskDisplay.Position?, stage: TaskDisplay.CurrentStage?,
        openItems: [Components.Schemas.AttentionItem], run: Components.Schemas.TaskTimelineRun?,
        completion: Components.Schemas.WorkUnitCompletionFacts?, now: Date, locale: Locale, timeZone: TimeZone
    ) -> String {
        if let oldest = openItems.compactMap(\.created_at).min() {
            let age = AttentionDisplay.relativeRowTime(oldest, now: now)
            let count = openItems.count == 1 ? "1 open decision" : "\(openItems.count) open decisions"
            return "Waiting on you since \(age) · \(count)"
        }
        if TaskDisplay.isActive(task) {
            let row = TaskDisplay.workingRow(task, position: position, stage: stage)
            if row.keyword == TaskDisplay.parked {
                return ([row.keyword] + [position?.hold].compactMap { $0 }).joined(separator: " · ")
            }
            let attempt = run?.attempt_number.map { "run \($0)" }
            return ([row.keyword] + [attempt].compactMap { $0 } + agentAndPool(task, stage: stage?.stage)).joined(
                separator: " · ")
        }
        if let completion {
            let merged = FreesideFormat.shortTime(completion.recorded_at, now: now, locale: locale, timeZone: timeZone)
            return "Merged as PR #\(completion.pr_number) · \(merged)"
        }
        let finished = FreesideFormat.shortTime(task.last_activity_at, now: now, locale: locale, timeZone: timeZone)
        return "\(TaskDisplay.finishedRow(task, position: position).keyword) · \(finished)"
    }

    /// A thread the page folds under the Timeline, with the item it
    /// belongs to.
    struct EarlierConversation {
        let item: Components.Schemas.AttentionItem
        let conversation: Components.Schemas.ConversationSnapshot
    }

    /// The threads of items no longer open, newest first, each once. A
    /// conversation belongs to the newest item that names it, and is left
    /// out when that item is open (its card draws it), when the leading
    /// item names it too (the leading card draws it), when the store
    /// doesn't hold it, or when it holds no message.
    static func earlierConversations(
        _ items: [Components.Schemas.AttentionItem], leadingItemID: String?,
        lookup: (String) -> Components.Schemas.ConversationSnapshot?
    ) -> [EarlierConversation] {
        let leading = items.first { $0.id == leadingItemID }?.conversation_id
        var seen: Set<String> = []
        return
            items
            .sorted { ($0.created_at ?? .distantPast) > ($1.created_at ?? .distantPast) }
            .compactMap { item in
                guard let id = item.conversation_id, seen.insert(id).inserted,
                    item.status != .open, id != leading,
                    let conversation = lookup(id), !conversation.conversation.messages.isEmpty
                else { return nil }
                return EarlierConversation(item: item, conversation: conversation)
            }
    }

    /// An earlier thread's fold summary: the item's status word, the
    /// message count, and the last message's time, as in
    /// `Resolved · 6 messages · Nov 3, 2:02 PM`.
    static func earlierConversationSummary(
        _ thread: EarlierConversation, now: Date, locale: Locale, timeZone: TimeZone
    ) -> String {
        let messages = thread.conversation.conversation.messages
        let count = messages.count == 1 ? "1 message" : "\(messages.count.formatted()) messages"
        let last = messages.max { $0.sequence < $1.sequence }
        return
            ([AttentionDisplay.label(thread.item.status), count]
            + [last.map { FreesideFormat.shortTime($0.created_at, now: now, locale: locale, timeZone: timeZone) }]
            .compactMap { $0 }).joined(separator: " · ")
    }

    /// The stage the rail's newest start point names, which the header's
    /// chip and hold callout repeat.
    static func newestStage(_ rows: [TimelineRail.Row]) -> Components.Schemas.StageName? {
        newestStageStart(rows)?.stage
    }

    /// The rail's newest start point and when it was recorded.
    static func newestStageStart(
        _ rows: [TimelineRail.Row]
    ) -> (stage: Components.Schemas.StageName, recordedAt: Date?)? {
        for row in rows {
            if case .stageStart(let stage?) = row.kind { return (stage, row.recordedAt) }
        }
        return nil
    }

    /// The round the newest round-opening event names: an external review
    /// re-entry, or a round request. Events are newest first.
    static func newestReviewRound(_ timeline: Components.Schemas.TaskTimeline) -> Int? {
        for event in timeline.events {
            switch event.kind {
            case .external_review_reentered:
                if let round = event.external_review?.value1.round { return round }
            case .review_requested:
                if let round = event.review?.value1.round { return round }
            default:
                continue
            }
        }
        return nil
    }

    private static func stage(_ role: Components.Schemas.TaskRunRole) -> Components.Schemas.StageName {
        switch role {
        case .specification: .specification
        case .implementation: .implementation
        }
    }

    /// A failed review round, a failed stop, or a work result the daemon
    /// recorded as failed: the entries whose own words say so.
    private static func recordsFailure(_ event: Components.Schemas.TaskEvent) -> Bool {
        if event.kind == .review_failed || event.kind == .stop_failed { return true }
        guard let milestone = event.milestone?.value1, milestone.kind == .terminal_recorded else { return false }
        return terminalWord(milestone) == Components.Schemas.ObservedInvocationStatus.failed.rawValue
    }

    /// A work result in the word the run rail's detail uses for it,
    /// lowercased to follow the attempt it ends.
    private static func terminalWord(_ milestone: Components.Schemas.RunMilestone) -> String? {
        (milestone.terminal?.value1.rawValue ?? milestone.outcome?.value1.rawValue)?
            .replacingOccurrences(of: "_", with: " ")
    }

    /// An event's words on the rail. A specification run is named by its
    /// phase, an implementation run by its attempt, and a review round by
    /// its number, never by an id; a run with no role or no attempt number
    /// keeps the label it had, since there is nothing to name it by.
    /// `pass` counts the run's work results, oldest first: a fix-up pass
    /// records a second one for the same attempt.
    ///
    /// `startsStage` drops the submission's own `started`, which the merged
    /// stage start already says: `Implementation started · Attempt 1`. A
    /// specification run's submission then has no words of its own, since
    /// the stage start names the run: `Specification started`.
    private static func rowLabel(
        _ event: Components.Schemas.TaskEvent, run: Components.Schemas.TaskTimelineRun?, pass: Int,
        startsStage: Bool = false
    ) -> String? {
        let attempt = run?.role == nil ? nil : run?.attempt_number
        if let milestone = event.milestone?.value1, run?.role?.value1 == .specification {
            if milestone.kind == .run_submitted { return startsStage ? nil : "Specification run started" }
            if milestone.kind == .terminal_recorded, let word = terminalWord(milestone) {
                return "Specification run" + (pass > 1 ? ", pass \(pass)" : "") + " \(word)"
            }
        }
        if let milestone = event.milestone?.value1, let attempt {
            if milestone.kind == .run_submitted { return "Attempt \(attempt)" + (startsStage ? "" : " started") }
            if milestone.kind == .terminal_recorded, let word = terminalWord(milestone) {
                return "Attempt \(attempt)" + (pass > 1 ? ", pass \(pass)" : "") + " \(word)"
            }
        }
        // The reviewer by login and the pull request by number: the two
        // names a reader can find on the forge.
        if let external = event.external_review?.value1, let number = event.pr_number,
            event.kind == .external_review_reentered
        {
            return "External review by @\(external.reviewer_login) on #\(number) · Round \(external.round)"
        }
        if let review = event.review?.value1 {
            switch event.kind {
            case .review_requested: return "Round \(review.round) requested"
            case .review_failed: return "Round \(review.round) failed"
            case .review_completed:
                let outcome = review.outcome?.value1.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
                return "Round \(review.round) completed" + (outcome.map { " · \($0)" } ?? "")
            default: break
            }
        }
        // The PR number is the one identifier the rail keeps: it names the
        // pull request to a reader, where a run id names nothing.
        if let number = event.pr_number, event.kind == .pr_opened || event.kind == .pr_merged {
            return "\(label(event.kind)) · #\(number)"
        }
        return label(event)
    }

    /// The header's name: the fetched timeline carries the task's current
    /// name at a newer revision than the bootstrap snapshot (a rename can
    /// land between the two), so it wins once loaded; the snapshot's name
    /// stands in while the timeline loads or is unavailable.
    static func headerName(
        timeline: Components.Schemas.TaskTimeline?, snapshot: Components.Schemas.TaskSnapshot
    ) -> Components.Schemas.DisplayName {
        timeline?.name ?? snapshot.task.display_names.task
    }

    /// A run is titled by its phase, since a campaign's specification run
    /// and its first implementation run both carry attempt 1 (#1462). A
    /// campaign holds one specification run, so it needs no number; an
    /// implementation run adds its attempt. A run with no role is titled
    /// by its attempt, or, missing that, by its short id, as the run
    /// timeline does. The exact id is in the page's Identifiers footer.
    static func runTitle(_ run: Components.Schemas.TaskTimelineRun) -> String {
        let attempt = run.attempt_number.map { "Attempt \($0)" }
        switch run.role?.value1 {
        case .specification:
            return label(.specification)
        case .implementation:
            return [label(.implementation), attempt].compactMap { $0 }.joined(separator: " · ")
        case nil:
            return attempt ?? "Run \(ShortIdentifier.short(run.run_id))"
        }
    }

    /// The run card's accessibility label: the title, which names the
    /// run's phase, without the opaque id VoiceOver would otherwise read
    /// character by character. Attempt numbers restart per campaign, so
    /// when another run shares this title and role the label names the
    /// campaign; the label stands in for the button's content, and two
    /// controls must not read alike.
    static func runCardAccessibilityLabel(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> String {
        var label = "Open detailed run history for \(runTitle(run))"
        if !isUniqueRunControl(run, in: timeline),
            let campaign = campaignID(forRun: run.run_id, in: timeline)
        {
            label += ", campaign \(ShortIdentifier.short(campaign))"
        }
        return label
    }

    /// Whether the run's title and role are unique among the timeline's runs.
    /// Not unique means attempts have restarted in another campaign, so the
    /// control needs a campaign to tell it apart.
    private static func isUniqueRunControl(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> Bool {
        let key = runControlKey(run)
        var matches = 0
        for section in timeline.sections {
            for other in section.runs where runControlKey(other) == key {
                matches += 1
                if matches > 1 { return false }
            }
        }
        return true
    }

    private static func runControlKey(
        _ run: Components.Schemas.TaskTimelineRun
    ) -> String {
        "\(runTitle(run))|\(run.role?.value1.rawValue ?? "")"
    }

    /// The number of campaign sections, so a section's title shows a
    /// disambiguating short campaign id only when more than one campaign is on
    /// screen.
    static func campaignCount(_ timeline: Components.Schemas.TaskTimeline) -> Int {
        timeline.sections.count { $0.campaign_id != nil }
    }

    static func sectionTitle(
        _ section: Components.Schemas.TaskTimelineSection, disambiguate: Bool = false
    ) -> String {
        guard let campaign = section.campaign_id else { return "Outside a Campaign" }
        return disambiguate ? "Campaign \(ShortIdentifier.short(campaign))" : "Campaign"
    }

    /// Names another run by its role and attempt when it sits in this timeline,
    /// so a supersession or parent line reads as "implementation attempt 2"
    /// within the history; otherwise by its short id, since the run is not on
    /// screen to number. The role is kept because a campaign's specification
    /// run and its first implementation run both carry attempt 1, so a bare
    /// "attempt 1" reference could not tell them apart.
    static func runReference(_ runID: String, in timeline: Components.Schemas.TaskTimeline) -> String {
        for section in timeline.sections {
            guard let run = section.runs.first(where: { $0.run_id == runID }) else { continue }
            switch run.role?.value1 {
            case .specification:
                return "specification run"
            case .implementation:
                return run.attempt_number.map { "implementation attempt \($0)" } ?? "implementation run"
            case nil:
                return run.attempt_number.map { "attempt \($0)" } ?? ShortIdentifier.short(runID)
            }
        }
        return ShortIdentifier.short(runID)
    }

    /// The facts the Identifiers footer's Task group holds.
    struct TaskIdentifiers {
        let taskID: String
        /// The task's repository slug. Only an issue-sourced task names
        /// one: no other client source does.
        let repository: String?
        /// The current run's synced policy digest, in full.
        let policyDigest: String?

        init(taskID: String, repository: String? = nil, policyDigest: String? = nil) {
            self.taskID = taskID
            self.repository = repository
            self.policyDigest = policyDigest
        }

        init(_ task: Components.Schemas.Task, policyDigest: String?) {
            var repository: String?
            if case .issue_subject(let source)? = task.source?.value1 { repository = source.issue_subject.repo }
            self.init(taskID: task.id, repository: repository, policyDigest: policyDigest)
        }
    }

    /// The task's identifiers: the exact task id, then its repository and
    /// policy digest where known.
    static func technicalRows(task: TaskIdentifiers) -> [AttentionDisplay.BindingRow] {
        [.init(label: "Task ID", value: task.taskID)]
            + [task.repository.map { .init(label: "Repository", value: $0) }].compactMap { $0 }
            + [task.policyDigest.map { .init(label: "Policy Digest", value: $0) }].compactMap { $0 }
    }

    /// A campaign section's identifiers: the exact campaign id and, when
    /// the section recorded one, the approved specification digest.
    static func technicalRows(
        section: Components.Schemas.TaskTimelineSection
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = []
        if let campaign = section.campaign_id {
            rows.append(.init(label: "Campaign ID", value: campaign))
        }
        if let digest = section.events.first(where: { $0.kind == .specification_approved })?
            .approved_spec_digest?.value1
        {
            rows.append(.init(label: "Approved Specification Digest", value: digest))
        }
        return rows
    }

    /// A run card's identifiers: the exact run, parent, and superseding
    /// ids, and for every verification a verification event records for this
    /// run, its Inbox item id and the head and base it was run against. A
    /// run that returned to the agent or remediated can record several, each
    /// its own ready item, so all are kept, not just the first. The event
    /// rail names a verification in words only, so its commits live here.
    static func technicalRows(
        run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = [.init(label: "Run ID", value: run.run_id)]
        if let parent = run.parent_run_id {
            rows.append(.init(label: "Parent Run ID", value: parent))
        }
        if let successor = run.superseded_by {
            rows.append(.init(label: "Superseded by Run ID", value: successor))
        }
        // Number the rows when a remediated run recorded more than one, so each
        // row's copy control has a distinct accessibility label.
        let verifications = verifications(for: run.run_id, in: timeline)
        for (index, verification) in verifications.enumerated() {
            let number = verifications.count > 1 ? " \(index + 1)" : ""
            rows += [
                .init(label: "Verification Inbox Item ID\(number)", value: verification.item_id),
                .init(label: "Verified Head\(number)", value: verification.head_sha),
                .init(label: "Verified Base\(number)", value: verification.base_sha),
            ]
        }
        return rows
    }

    /// The verification each verification event records for the run, one per
    /// distinct Inbox item id, in event order. The daemon emits one
    /// verification event per ready item, so a remediated run has more than
    /// one.
    private static func verifications(
        for runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> [Components.Schemas.TaskEventVerification] {
        var seen: Set<String> = []
        var verifications: [Components.Schemas.TaskEventVerification] = []
        for event in events(timeline) where event.run_id == runID {
            if let verification = event.verification?.value1, seen.insert(verification.item_id).inserted {
                verifications.append(verification)
            }
        }
        return verifications
    }

    /// The campaign a run belongs to, taken from its section. A daemon
    /// run-scoped event (milestone, review, verification, PR) carries only a
    /// run id, so its campaign is recovered here rather than read off the
    /// event.
    static func campaignID(
        forRun runID: String, in timeline: Components.Schemas.TaskTimeline
    ) -> String? {
        for section in timeline.sections where section.runs.contains(where: { $0.run_id == runID }) {
            return section.campaign_id
        }
        return nil
    }

    /// The header's one meta line: project, issue, lifecycle, source.
    static func headerMetaLine(_ task: Components.Schemas.Task) -> String {
        ([TaskDisplay.projectName(task)] + [TaskDisplay.issueReference(task)].compactMap { $0 } + [
            TaskDisplay.lifecycleLabel(task).text, TaskDisplay.sourceLine(task),
        ]).joined(separator: " · ")
    }

    /// When a section's work began: its campaign's allocation when the
    /// daemon recorded one, else its earliest milestone. Nil for a section
    /// with neither, which then names no date.
    static func sectionStart(_ section: Components.Schemas.TaskTimelineSection) -> Date? {
        let events = section.events + section.runs.flatMap(\.events)
        if let allocated = events.filter({ $0.kind == .campaign_allocated }).map(\.recorded_at).min() {
            return allocated
        }
        return section.runs.flatMap(\.milestones).map(\.recorded_at).min()
    }

    /// The closed summary beside an earlier campaign's keyword:
    /// `1 run · Sep 10`. Why it ended is not a recorded fact, so the
    /// summary does not say.
    static func sectionSummary(
        _ section: Components.Schemas.TaskTimelineSection, now: Date,
        locale: Locale = .current, timeZone: TimeZone = .current
    ) -> String {
        let count = section.runs.count
        let start = sectionStart(section).map {
            FreesideFormat.shortDate($0, now: now, locale: locale, timeZone: timeZone)
        }
        return (["\(count) \(count == 1 ? "run" : "runs")"] + [start].compactMap { $0 }).joined(separator: " · ")
    }

    /// The closed summary of a run's details: role, attempt reason, the
    /// recorded hold, and, from `schedules`, the soonest one to fire
    /// (`next checks 4:40 AM`). Nil when none of them is known, and then
    /// the disclosure has nothing to fold unless it holds lineage.
    static func runDetailsSummary(
        _ run: Components.Schemas.TaskTimelineRun, schedules: [Components.Schemas.ScheduleSnapshot] = [],
        now: Date = .now, locale: Locale = .current, timeZone: TimeZone = .current
    ) -> String? {
        let next = schedules.compactMap { snapshot in snapshot.schedule.fire_at.map { (snapshot.schedule, $0) } }
            .min { $0.1 < $1.1 }
            .map { schedule, fireAt in
                "next \(RunDisplay.label(schedule.kind).lowercased()) "
                    + FreesideFormat.shortTime(fireAt, now: now, locale: locale, timeZone: timeZone)
            }
        let parts =
            [run.role.map { label($0.value1) }, run.attempt_reason]
            + [run.hold.map { "hold: \(RunDisplay.label($0.value1.reason))" }, next]
        let present = parts.compactMap { $0 }
        return present.isEmpty ? nil : present.joined(separator: " · ")
    }

    /// How a fact names a run this history holds, or nil for one it
    /// doesn't: `runReference` in sentence case, as a value that leads its
    /// own line. A run with neither role nor attempt number keeps its
    /// short id as written.
    static func runLinkTitle(_ runID: String, in timeline: Components.Schemas.TaskTimeline) -> String? {
        guard let run = timeline.sections.lazy.flatMap(\.runs).first(where: { $0.run_id == runID }) else {
            return nil
        }
        let reference = runReference(runID, in: timeline)
        guard run.role != nil || run.attempt_number != nil else { return reference }
        return reference.prefix(1).uppercased() + reference.dropFirst()
    }

    /// The page's Identifiers footer, in page order: the task (its id,
    /// repository, and policy digest), the leading card's decision, then,
    /// once the history loads, each campaign followed by its attempts,
    /// every owner under the name the page gives it. An owner with no
    /// identifiers (a section outside a campaign with no approved
    /// specification) is left out.
    static func identifierGroups(
        _ timeline: Components.Schemas.TaskTimeline?, task: TaskIdentifiers,
        decision: [AttentionDisplay.BindingRow] = []
    ) -> [IdentifierGroup] {
        var groups = [
            IdentifierGroup(owner: "Task", rows: technicalRows(task: task)),
            IdentifierGroup(owner: "Decision", rows: decision),
        ]
        guard let timeline else { return groups.filter { !$0.rows.isEmpty } }
        let disambiguate = campaignCount(timeline) > 1
        for section in timeline.sections {
            groups.append(
                .init(owner: sectionTitle(section, disambiguate: disambiguate), rows: technicalRows(section: section)))
            for run in section.runs {
                groups.append(
                    .init(owner: identifierOwner(run, in: timeline), rows: technicalRows(run: run, in: timeline)))
            }
        }
        return groups.filter { !$0.rows.isEmpty }
    }

    /// A run's name over its ids in the footer and in the copied text, where
    /// no card stands beside it to say which run it is: its role as
    /// `runLinkTitle` names it, since a campaign's specification run and its
    /// first implementation run both carry attempt 1, and the campaign too
    /// when attempts restart in another one, as the run card's spoken label
    /// does. A run with no role keeps its card title. No two owners read
    /// alike.
    static func identifierOwner(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> String {
        let title = run.role == nil ? runTitle(run) : runLinkTitle(run.run_id, in: timeline) ?? runTitle(run)
        guard !isUniqueRunControl(run, in: timeline), let campaign = campaignID(forRun: run.run_id, in: timeline)
        else { return title }
        return "\(title), campaign \(ShortIdentifier.short(campaign))"
    }

    /// What the closed footer says it holds:
    /// `task · repo · policy · decision · campaign · 2 runs`.
    static func identifiersSummary(
        _ timeline: Components.Schemas.TaskTimeline?, task: TaskIdentifiers, hasDecision: Bool = false
    ) -> String {
        let sections = timeline?.sections ?? []
        let campaigns = sections.count { !technicalRows(section: $0).isEmpty }
        let runs = sections.reduce(0) { $0 + $1.runs.count }
        return
            (["task", task.repository == nil ? nil : "repo", task.policyDigest == nil ? nil : "policy"]
            + [hasDecision ? "decision" : nil]
            + [campaigns > 0 ? (campaigns == 1 ? "campaign" : "\(campaigns) campaigns") : nil]
            + [runs > 0 ? "\(runs) \(runs == 1 ? "run" : "runs")" : nil])
            .compactMap { $0 }.joined(separator: " · ")
    }

    /// Words a copy item keeps capitalized: `ID` is an initialism and
    /// `Inbox` names the screen.
    private static let copyTitleKeptWords: Set<String> = ["ID", "Inbox"]

    /// A row's context-menu item: `Copy ` and the row's label in sentence
    /// case (`Copy parent run ID`, `Copy verified head 1`).
    static func copyMenuTitle(for label: String) -> String {
        let words = label.split(separator: " ").map { word in
            copyTitleKeptWords.contains(String(word)) ? String(word) : word.lowercased()
        }
        return (["Copy"] + words).joined(separator: " ")
    }

    /// `Copy all identifiers`: each group as a block, its owner on a line
    /// and each `Label: value` under it, a blank line between blocks, for
    /// pasting into a terminal or an issue.
    static func allIdentifiersText(_ groups: [IdentifierGroup]) -> String {
        groups.map { group in
            ([group.owner] + group.rows.map { "\($0.label): \($0.value)" }).joined(separator: "\n")
        }
        .joined(separator: "\n\n")
    }

    static func hasRunDetails(
        _ run: Components.Schemas.TaskTimelineRun, schedules: [Components.Schemas.ScheduleSnapshot] = []
    ) -> Bool {
        runDetailsSummary(run) != nil || run.superseded_by != nil || run.parent_run_id != nil || !schedules.isEmpty
    }

    /// A run card's chip. The run the task currently stands on repeats the
    /// task's own status and cut. Every other run is history, so faint: its
    /// synced outcome in the run vocabulary, marked superseded when the
    /// timeline says so, and no chip when neither is known.
    ///
    /// A specification run that a successor superseded handed off to
    /// implementation, so it reads `Handed Off`. A run that never reached a
    /// final outcome (`pending` or `unobserved`) shows none once something
    /// ended it, a successor or the task leaving its active lifecycle: the
    /// daemon leaves such runs unconcluded, and `In Progress` would claim
    /// they still run (#1462).
    static func runChip(
        _ run: Components.Schemas.TaskTimelineRun, task: Components.Schemas.Task,
        position: TaskDisplay.Position?, synced: Components.Schemas.Run?
    ) -> (label: String, cut: StateChip.Cut)? {
        if task.current_position?.value1.run_id == run.run_id {
            return (
                position?.status ?? TaskDisplay.rowStatus(task, run: synced),
                TaskDisplay.statusCut(task, position: position)
            )
        }
        if run.role?.value1 == .specification, run.superseded_by != nil { return ("Handed Off", .faint) }
        let ended = run.superseded_by != nil || !TaskDisplay.isActive(task)
        let outcome = synced.flatMap { synced in
            ended && [.pending, .unobserved].contains(synced.outcome) ? nil : RunDisplay.label(synced.outcome)
        }
        let parts = [outcome, run.superseded_by == nil ? nil : "Superseded"].compactMap { $0 }
        return parts.isEmpty ? nil : (parts.joined(separator: " · "), .faint)
    }

    static func newestMilestoneTime(_ run: Components.Schemas.TaskTimelineRun) -> Date? {
        run.milestones.first?.recorded_at
    }

    static func label(_ role: Components.Schemas.TaskRunRole) -> String {
        switch role {
        case .specification: "Specification"
        case .implementation: "Implementation"
        }
    }

    static func label(_ kind: Components.Schemas.TaskEventKind) -> String {
        switch kind {
        case .task_created: "Task created"
        case .task_started: "Task started"
        case .task_completed: "Task work completed"
        case .task_abandoned: "Task work abandoned"
        case .stop_requested: "Stop requested"
        case .task_stopped: "Task stopped"
        case .stop_failed: "Stop failed"
        case .run_milestone: "Run activity"
        case .review_requested: "Review requested"
        case .review_completed: "Review completed"
        case .review_failed: "Review failed"
        case .verification_recorded: "Verification recorded"
        case .campaign_allocated: "Campaign allocated"
        case .specification_approved: "Specification approved"
        case .pr_opened: "PR opened"
        case .pr_merged: "PR merged"
        case .external_review_reentered: "External review re-entered"
        case .task_line_set: "Agent set"
        case .specifier_triaged: "Specifying tier chosen"
        }
    }

    static func label(_ event: Components.Schemas.TaskEvent) -> String {
        if let milestone = event.milestone?.value1 {
            let detail = RunHistoryPresentation.detail(milestone)
            return RunDisplay.label(milestone.kind) + (detail.map { " · \($0)" } ?? "")
        }
        if let verification = event.verification?.value1 {
            return verification._class == .ready_clean
                ? "Verification recorded · Clean" : "Verification recorded · Degraded"
        }
        if let line = event.line?.value1 {
            return label(line)
        }
        if let triage = event.triage?.value1 {
            return label(triage)
        }
        return label(event.kind)
    }

    // An answer names the tier the triager chose. A failure or skip names
    // what stood in for an answer, after the escalation it always causes, so
    // the row never reads as the triager's choice. The task proceeds either
    // way, so the row is an event, not a failure.
    static func label(_ triage: Components.Schemas.TaskEventTriage) -> String {
        if let tier = triage.tier?.value1 {
            switch tier {
            case .standard: return "Specifying tier · Standard"
            case .escalated: return "Specifying tier · Escalated"
            }
        }
        let cause =
            switch triage.outcome {
            case .answered: "answered"
            case .call_failed: "call failed"
            case .invalid_answer: "invalid answer"
            case .budget_spent: "budget spent"
            case .no_triager_line: "no triager line"
            }
        return "Specifying tier escalated · \(cause)"
    }

    static func label(_ role: Components.Schemas.TaskLineRole) -> String {
        switch role {
        case .specifier: "Specifying agent"
        case .implementer: "Implementing agent"
        case .remediator: "Remediating agent"
        case .reviewer: "Reviewing agent"
        }
    }

    // The arrow is reserved for an agent that held the role and was replaced.
    // An agent the router passed over never ran, and falling back is routine,
    // so that row names it without a reason or a warning. The operator stays
    // implied, as on the other rows it causes.
    // Each choice reads in the encoding its version recorded (lineLabel).
    static func label(_ line: Components.Schemas.TaskEventTaskLine) -> String {
        let subject = label(line.role)
        let superseded = TaskDisplay.lineLabel(
            agent: line.superseded_agent, selection: line.superseded_selection?.value1)
        guard let agent = TaskDisplay.lineLabel(agent: line.agent, selection: line.selection?.value1) else {
            return "\(subject) back to lineup" + (superseded.map { " · \($0)" } ?? "")
        }
        let actor =
            switch line.source {
            case .router: " by router"
            case .triage: " by triage"
            default: ""
            }
        if let superseded {
            return "\(subject) changed\(actor) · \(superseded) → \(agent)"
        }
        let fallback =
            line.provenance?.value1.passed_over.flatMap { mark in
                (mark.value1.enrollment ?? mark.value1.agent).map { " instead of \($0)" }
            } ?? ""
        return "\(subject) set\(actor) · \(agent)\(fallback)"
    }
}
