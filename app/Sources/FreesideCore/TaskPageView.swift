import FreesideAPI
import SwiftUI

/// One page per task, in every state: the header, the pinned outline, the
/// leading card and the other open items, then the body (the Task fold, the
/// Stop states, Conversation, and the Timeline). The body's history is as the
/// daemon computes it (`GET /tasks/{id}/timeline`): the task's events as one
/// open rail that marks where each stage began, then every campaign folds
/// its runs and detailed milestones. Every list keeps the daemon's
/// newest-first order; unlike the run rail, nothing here is reversed or
/// sorted by timestamp.
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
    @State private var specificationRequest: TaskSpecificationRequest?
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
        /// The open items the recorded layout drew.
        var itemIDs: Set<String> = []
        /// Whether a card leads the page as last drawn, for a move that
        /// runs after the view it was scheduled from has been replaced.
        var leadsWithCard = false
        /// A move waiting for a change to lay out. The page records nothing
        /// meanwhile, so the layout the change first draws, with the
        /// reader's section pushed down or pulled up, isn't their place.
        var pendingMove: PendingMove?
    }

    private enum PendingMove {
        /// Back to the reader's place, after an open item arrived or left.
        case toPlace
        /// To the leading card, which the operator opened.
        case toCard
        /// To the Identifiers footer, which the leading card revealed.
        case toIdentifiers
    }

    /// The page's top, where a Decisions place with no card left returns.
    private enum PageTop: Hashable { case top }

    /// The Identifiers footer, where a card's reveal lands. Not a
    /// `TaskPageAnchor`: the footer is no section the outline row offers.
    private enum IdentifiersFooter: Hashable { case top }

    init(
        coordinator: SyncCoordinator, snapshot: Components.Schemas.TaskSnapshot,
        onOpenRun: @escaping (String) -> Void, onOpenItem: ((String) -> Void)? = nil,
        leadingItemID: String? = nil, leadingCard: AnyView? = nil, scrollProxy: ScrollViewProxy? = nil,
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

    private var conversations: [Components.Schemas.ConversationSnapshot] {
        TaskTimelinePresentation.conversations(taskItems.map(\.item)) { coordinator.store.conversationsByID[$0] }
    }

    private static func messageCount(_ conversations: [Components.Schemas.ConversationSnapshot]) -> Int {
        conversations.reduce(0) { $0 + $1.conversation.messages.count }
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
            LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                header(timeline)
                    .padding(.bottom, Self.contentGap - 6)
                Section {
                    sections(timeline)
                        .padding(.top, Self.contentGap)
                } header: {
                    outlineRow(timeline)
                        .onGeometryChange(for: CGFloat.self, of: \.size.height) { outlineHeight = $0 }
                        .onGeometryChange(for: CGFloat.self, of: { $0.frame(in: .scrollView).minY }) { top in
                            place.rowTop = top
                            recordPlace()
                        }
                }
            }
            .pageColumn(compact: compact, topMargin: topMargin)
            .id(PageTop.top)
            .task { await restorePlace() }
            .onChange(of: chosenSection) { held?.chosenSection = chosenSection }
            .onChange(of: memory?.cardOpenings) { moveToOpenedCard() }
            // `repairSelection` moves the lead only off a card that closed,
            // so a move off one still open is the operator choosing another.
            .onChange(of: leadingItemID) { previous, _ in
                // An open the memory counted has already started the move.
                if let previous, openItemIDs.contains(previous), place.pendingMove != .toCard {
                    moveToOpenedCard()
                }
            }
            .onChange(of: leadingCard != nil, initial: true) { place.leadsWithCard = leadingCard != nil }
            .onChange(of: identifiersReveals) { revealIdentifiers() }
            .taskStopConfirmation(
                $stopConfirmation, model: coordinator.taskStop, dynamicTypeSize: dynamicTypeSize
            )
            .sheet(item: $specificationRequest) { request in
                TaskSpecificationReaderSheet(request: request, attachments: coordinator.store.attachments)
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

    /// Everything under the outline row. One container, so a lazy stack
    /// holds every jump target as soon as the page draws.
    private func sections(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        VStack(alignment: .leading, spacing: Self.contentGap) {
            if let leadingCard {
                leadingCard
                    .background(alignment: .top) { jumpTarget(.section(.decisions)) }
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
                    }
                }
            }
            let conversations = conversations
            VStack(alignment: .leading, spacing: 22) {
                taskFold(timeline)
                    .background(alignment: .top) { jumpTarget(.section(.task)) }
                stopStates
                schedules
                // Absent at zero, as Decisions is: the outline drops its
                // chip then too.
                if Self.messageCount(conversations) > 0 {
                    conversationSection(conversations)
                        .background(alignment: .top) { jumpTarget(.section(.conversation)) }
                }
                timelineSection(timeline)
                    .background(alignment: .top) { jumpTarget(.section(.timeline)) }
                // Outside the timeline, so a seated card's identifiers,
                // which have no other place on the page, stay reachable
                // while the history loads or is unavailable.
                if hasIdentifiersFooter(timeline) {
                    identifiers(timeline)
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
            .onDisappear { place.targetTops[target] = nil }
            .id(target)
            .padding(.top, -outlineHeight)
            .accessibilityHidden(true)
    }

    private func jump(to target: TaskPageAnchor) {
        withAnimation { scrollProxy?.scrollTo(target, anchor: .top) }
    }

    // MARK: Place

    /// The memory's entry when it is this task's. Nil in a screenshot, and
    /// for the moment before a mount claims the memory.
    private var held: TaskPageMemory? {
        memory?.taskID == snapshot.task.id ? memory : nil
    }

    private var openItemIDs: Set<String> { Set(openItems.map(\.item.id)) }

    /// Records the reader's place when it changes, and writes it through to
    /// the memory for a later mount. When the open items change, it moves
    /// back to the place instead.
    private func recordPlace() {
        guard place.restored, place.pendingMove == nil else { return }
        if openItemIDs != place.itemIDs {
            returnToPlace()
            return
        }
        let anchor = TaskPageAnchor.reading(offsets: place.targetTops.mapValues { $0 - place.rowTop })
        guard anchor != place.anchor else { return }
        place.anchor = anchor
        held?.anchor = anchor
    }

    /// Returns the page to the reader's place after an open item arrives or
    /// leaves. A card arriving above the reader's section pushes it down
    /// and one leaving pulls it up; the page itself stays mounted. The
    /// route's repair, which seats or drops the card, follows the store
    /// change in a later transaction, so the move waits the beat a restore
    /// does. A Decisions place with no card left goes to the top.
    private func returnToPlace() {
        place.pendingMove = .toPlace
        Task { @MainActor [place, scrollProxy] in
            try? await Task.sleep(for: .milliseconds(120))
            // An opened card wins: the operator chose where to go.
            guard place.pendingMove == .toPlace else { return }
            if let anchor = place.anchor {
                if anchor == .section(.decisions), !place.leadsWithCard {
                    scrollProxy?.scrollTo(PageTop.top, anchor: .top)
                    place.anchor = nil
                    held?.anchor = nil
                    chosenSection = nil
                } else {
                    await drawTarget(anchor)
                    scrollProxy?.scrollTo(anchor, anchor: .top)
                }
            }
            place.itemIDs = openItemIDs
            place.pendingMove = nil
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
            await drawTarget(.section(.decisions))
            jump(to: .section(.decisions))
            place.itemIDs = openItemIDs
            // Opening forgot the memory's place; the next record after the
            // jump writes the new one through.
            place.anchor = nil
            place.pendingMove = nil
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
        Task { @MainActor [place, scrollProxy] in
            try? await Task.sleep(for: .milliseconds(120))
            guard place.pendingMove == .toIdentifiers else { return }
            chosenSection = nil
            await drawTarget(.section(.timeline))
            withAnimation { scrollProxy?.scrollTo(IdentifiersFooter.top, anchor: .top) }
            place.itemIDs = openItemIDs
            place.anchor = nil
            place.pendingMove = nil
        }
    }

    /// Brings back the page's body when `target` isn't drawn, so a jump to
    /// it lands. A card leaving or a shorter one taking the lead shrinks the
    /// document under a reader near its end, past which the lazy stack drops
    /// the body and every jump target in it; the top draws it again.
    private func drawTarget(_ target: TaskPageAnchor) async {
        guard place.targetTops[target] == nil else { return }
        scrollProxy?.scrollTo(PageTop.top, anchor: .top)
        try? await Task.sleep(for: .milliseconds(50))
    }

    /// Returns a fresh mount to the place the memory holds for this task,
    /// and to the chip last chosen: the Mac detail column replaces the page
    /// with a run, so Back to Task mounts it again.
    private func restorePlace() async {
        guard !place.restored else { return }
        defer { place.restored = true }
        place.itemIDs = openItemIDs
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
    private func jump(toRun runID: String, in timeline: Components.Schemas.TaskTimeline?) {
        chosenSection = .runs
        let section = timeline?.sections.first { $0.runs.contains { $0.run_id == runID } }
        if let section {
            fold(.campaign(section.campaign_id)).wrappedValue = true
        }
        fold(.run(runID)).wrappedValue = true
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

    /// Layer 0: what the work is and where it stands, without scrolling
    /// (5.6): the task's project, issue, and stage over its name, one status
    /// line, the hold when there is one, and where to act. The task id lives
    /// in the page's Identifiers footer; the copy menu keeps a quick path to
    /// it. Internal so a screenshot golden can draw the header on its own.
    func header(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = snapshot.task
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
            documentsLine(timeline)
            // A parked status line already names the hold.
            if let hold = TaskDisplay.holdCallout(task, position: position, stage: stage),
                position?.hold.map(status.hasSuffix) != true
            {
                holdCallout(hold)
            }
            // A quota-window park may offer agents on other pools; moving
            // to one is the operator's choice, never a fallback.
            let offers = TaskDisplay.moveOffers(position)
            if !offers.isEmpty {
                ParkedTaskMoveView(model: coordinator.parkedTaskMove, taskID: task.id, offers: offers)
            }
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
            case .specification(let request):
                Button {
                    specificationRequest = request
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
        return OutlineRow(
            decisions: open.map { .init(id: $0.item.id, title: AttentionDisplay.ask($0.item)) },
            leadingItemID: leadingItemID,
            conversationMessages: Self.messageCount(conversations),
            runs: (timeline?.sections.flatMap(\.runs) ?? []).map {
                .init(id: $0.run_id, title: TaskTimelinePresentation.runTitle($0))
            },
            selected: chosenSection ?? (open.isEmpty ? .task : .decisions),
            rendersInteractiveControls: screenshotTimeline == nil,
            onSelect: { section in
                chosenSection = section
                // A one-run page's Runs chip lands on that run's history.
                jump(to: .section(section == .runs ? .timeline : section))
            },
            onChooseDecision: { onOpenItem?($0) },
            onChooseRun: { jump(toRun: $0, in: timeline) },
            trailing: {
                if TaskActionsSlot(compact: compact) == .outlineRow { taskActionsMenu(timeline) }
            }
        )
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
        _ timeline: Components.Schemas.TaskTimeline?, taskID: String,
        decision: [AttentionDisplay.BindingRow] = []
    ) -> [TaskActionsMenu.Action] {
        guard timeline != nil || !decision.isEmpty else { return [] }
        let text = TaskTimelinePresentation.allIdentifiersText(
            TaskTimelinePresentation.identifierGroups(timeline, taskID: taskID, decision: decision))
        return [.init(title: "Copy all identifiers") { Clipboard.copy(text) }]
    }

    /// The task's one menu (◆B): its own items, then Stop.
    private func taskActionsMenu(_ timeline: Components.Schemas.TaskTimeline?) -> TaskActionsMenu {
        TaskActionsMenu(
            coordinator: coordinator, taskID: snapshot.task.id,
            actions: Self.taskActions(timeline, taskID: snapshot.task.id, decision: decisionIdentifiers),
            confirmation: $stopConfirmation,
            rendersInteractiveControls: screenshotTimeline == nil)
    }

    // MARK: Body

    /// What the task is bound to, one mono line closed. Isolation and branch
    /// have no client source yet, so the fold omits them rather than guess.
    private func taskFold(_ timeline: Components.Schemas.TaskTimeline?) -> some View {
        let task = snapshot.task
        let position = position(timeline)
        let run = TaskTimelinePresentation.currentRun(task, in: timeline)
        let stage = currentStage(timeline)?.stage
        let completion = TaskTimelinePresentation.completion(task, runs: coordinator.runs)
        return SentenceDisclosure(
            label: "Task",
            summary: TaskTimelinePresentation.taskSummary(task, position: position, stage: stage, run: run),
            isExpanded: fold(.task)
        ) {
            VStack(alignment: .leading, spacing: 8) {
                ForEach(
                    TaskTimelinePresentation.taskFacts(
                        task, position: position, stage: stage, run: run,
                        policyDigest: run.flatMap { current in
                            coordinator.runs.first { $0.run.id == current.run_id }?.run.policy_digest
                        },
                        pullRequest: completion?.pr_number ?? timeline.flatMap(TaskTimelinePresentation.pullRequest),
                        historyLoaded: timeline != nil,
                        admission: run.flatMap { coordinator.timelinesByRunID[$0.run_id] }
                            .flatMap(RunDisplay.latestImplementerAdmission)),
                    id: \.label
                ) { row in
                    FactRow(label: row.label, value: row.value)
                }
            }
            .padding(.leading, 19)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// What the task's Stop says, drawn only while there is a Stop state
    /// to state; Stop itself starts from the Task Actions menu.
    @ViewBuilder private var stopStates: some View {
        let stop = TaskStopView(
            coordinator: coordinator, taskID: snapshot.task.id, explanationExpanded: $showsStopExplanation)
        if !stop.isQuiet { stop }
    }

    /// Every conversation the task's items name, read-only: a reply goes
    /// through the leading card, which is what the thread's item asks.
    private func conversationSection(_ conversations: [Components.Schemas.ConversationSnapshot]) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            ForEach(conversations, id: \.conversation.id) { conversation in
                ConversationView(
                    snapshot: conversation,
                    attachments: coordinator.store.attachments,
                    loadsAttachments: screenshotTimeline == nil,
                    now: now,
                    rendersInteractiveControls: screenshotTimeline == nil)
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

    /// The task's armed watches and deadlines, each with when it next
    /// fires. They sit on the task's own page, not on its list row (6.1),
    /// and draw nothing while none is armed.
    @ViewBuilder private var schedules: some View {
        let armed = TaskDisplay.armedSchedules(for: snapshot.task, in: coordinator.schedules)
        if !armed.isEmpty {
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Schedules")
                ForEach(armed, id: \.schedule.id) { snapshot in
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
    }

    /// Layer 1 is the task's events, always open: the rail that says which
    /// stage the task is in and how it got there. Under it every campaign,
    /// the current one included, is one folded disclosure row, and the
    /// page's exact identifiers close it under one hairline.
    private func content(_ timeline: Components.Schemas.TaskTimeline) -> some View {
        let rows = TaskTimelinePresentation.rows(
            timeline,
            reviewRounds: TaskTimelinePresentation.reviewRounds(timeline, runTimelines: coordinator.timelinesByRunID),
            items: taskItems.map(\.item))
        let disambiguate = TaskTimelinePresentation.campaignCount(timeline) > 1
        return VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Timeline")
                    .accessibilityAddTraits(.isHeader)
                if let message = TaskTimelinePresentation.availabilityMessage(
                    state: coordinator.taskTimelineLoadStates[snapshot.task.id], freshness: coordinator.store.freshness)
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
                    TimelineRail(rows: rows, onOpenItem: onOpenItem)
                }
            }
            if timeline.sections.isEmpty {
                Text("No campaigns or runs in this history.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            ForEach(Array(timeline.sections.enumerated()), id: \.offset) { index, section in
                let ids = TaskTimelinePresentation.technicalRows(section: section)
                SentenceDisclosure(
                    label: TaskTimelinePresentation.sectionTitle(section, disambiguate: disambiguate),
                    summary: TaskTimelinePresentation.sectionSummary(
                        section, isCurrent: index == 0, now: now, locale: locale, timeZone: timeZone),
                    isExpanded: fold(.campaign(section.campaign_id)),
                    // A section outside a campaign may hold no id at all.
                    menuItems: ids.isEmpty ? nil : { copyItems(ids) }
                ) {
                    sectionBody(section, in: timeline, leadsWithFullCard: index == 0)
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
        TechnicalDetailsSection(
            groups: TaskTimelinePresentation.identifierGroups(
                timeline, taskID: snapshot.task.id, decision: decisionIdentifiers),
            summary: TaskTimelinePresentation.identifiersSummary(
                timeline, hasDecision: !decisionIdentifiers.isEmpty),
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

    /// A section's runs. The current section leads with its newest run as a
    /// full card; every other run, here and in an earlier campaign, is a
    /// one-line card that opens to the same content.
    private func sectionBody(
        _ section: Components.Schemas.TaskTimelineSection, in timeline: Components.Schemas.TaskTimeline,
        leadsWithFullCard: Bool
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            ForEach(Array(section.runs.enumerated()), id: \.element.run_id) { index, run in
                if leadsWithFullCard && index == 0 {
                    item(verticalPadding: 18, horizontalPadding: 20) {
                        VStack(alignment: .leading, spacing: 16) {
                            openRunButton(run, in: timeline, showsTitle: true)
                            runBody(run, in: timeline)
                        }
                    }
                    .background(alignment: .top) { jumpTarget(.run(run.run_id)) }
                } else {
                    item(verticalPadding: 12, horizontalPadding: 18) { priorRun(run, in: timeline) }
                        .background(alignment: .top) { jumpTarget(.run(run.run_id)) }
                }
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

    /// `Attempt N` with its chip beside it: the title is fixed vocabulary
    /// that cannot wrap, which is the one case a chip sits beside a title.
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
                runBody(run, in: timeline)
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

    /// Everything under a run's title: review, milestones, details.
    @ViewBuilder
    private func runBody(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        if run.role == nil || run.role?.value1 == .implementation {
            RunReviewSection(
                coordinator: coordinator, runID: run.run_id,
                facts: coordinator.timelinesByRunID[run.run_id]?.review?.value1,
                stages: stages(of: run.run_id),
                hasTimeline: coordinator.timelinesByRunID[run.run_id] != nil,
                folds: .init(
                    facts: { fold(.roundFacts($0)) }, priorRounds: fold(.priorRounds(run.run_id))))
        }
        VStack(alignment: .leading, spacing: 11) {
            KeywordLabel(text: "Milestones")
            if run.milestones.isEmpty {
                Text("No execution milestones in this run's task history.")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            } else {
                TimelineRail(
                    rows: TaskTimelinePresentation.milestoneRows(
                        run,
                        reviewRounds: coordinator.timelinesByRunID[run.run_id]?.review?.value1.rounds ?? [],
                        stages: stages(of: run.run_id)))
            }
        }
        if TaskTimelinePresentation.hasRunDetails(run) {
            SentenceDisclosure(
                label: "Run Details", summary: TaskTimelinePresentation.runDetailsSummary(run),
                isExpanded: fold(.runDetails(run.run_id))
            ) {
                runDetails(run, in: timeline)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// The run's role, lineage, and recorded hold as facts. A run this
    /// history holds is a link to its own timeline; one it doesn't hold is
    /// named by its short id, with nowhere to go.
    private func runDetails(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> some View {
        VStack(alignment: .leading, spacing: 8) {
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

/// Hosts the page in its one scroll view and keeps its history fresh. The
/// leading card is content in the page, seated without a scroll view of its
/// own, so a card arriving, leaving, or handing the lead to another leaves
/// the page mounted; only the card is replaced.
struct TaskPageHost: View {
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
    /// Builds the leading item's card, seated in the page's scroll view.
    let card: (String, DecisionDetailView.PageSeat) -> DecisionDetailView
    /// The seated card's identifier rows, by item id. The card builds them
    /// from facts its model fetches and publishes them here, because the
    /// footer that lists them belongs to the page.
    @State private var seatedIdentifiers: [String: [AttentionDisplay.BindingRow]] = [:]
    @State private var identifiersReveals = 0
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
                    decisionIdentifiers: leadingItemID.flatMap { seatedIdentifiers[$0] } ?? [],
                    identifiersReveals: identifiersReveals,
                    identifiersOpenAtLaunch: identifiersOpenAtLaunch && leadingItemID != nil,
                    topMargin: topMargin, memory: memory, disclosures: disclosures)
            }
            .coordinateSpace(name: DecisionDetailView.scrollSpace)
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
    static func availabilityMessage(
        state: SyncCoordinator.TimelineLoadState?, freshness: InboxStore.Freshness
    ) -> String? {
        if state == .loading { return "Showing saved task history while refreshing…" }
        if state == .unavailable { return "Task history refresh failed. Showing saved history." }
        if state != .loaded || freshness.isFailing { return "Saved task history. Freshness unconfirmed." }
        return nil
    }

    /// The page's elements flattened in render order, so a test can pin the
    /// order without a view: the event rail's rows by their words, then
    /// each campaign's runs and milestones.
    enum Entry: Equatable {
        case row(String)
        case section(campaignID: String?)
        case run(String)
        case milestone(runID: String, kind: Components.Schemas.RunMilestoneKind)
    }

    static func entries(_ timeline: Components.Schemas.TaskTimeline) -> [Entry] {
        var entries: [Entry] = rows(timeline).map { .row($0.label) }
        for section in timeline.sections {
            entries.append(.section(campaignID: section.campaign_id))
            for run in section.runs {
                entries.append(.run(run.run_id))
                entries += run.milestones.map { .milestone(runID: run.run_id, kind: $0.kind) }
            }
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
    /// milestone enters Verification; each draws a start point directly
    /// under itself when that changes where the task stands. Verification's
    /// start therefore carries the time of the first event that moved the
    /// task there, which trails the real start by the daemon's own
    /// verification (devlog/2026-10-23-0856-verification-start-point.md).
    /// No other event moves the stage: an attempt's result can be recorded
    /// after the review of its work was requested, and must not read as a
    /// return to implementation.
    ///
    /// A remediation is a return to Implementation. `reviewRounds` holds
    /// each run's review rounds by run id; a round whose findings went to a
    /// remediator names its invocation. That invocation's start draws
    /// "Remediation started" with an Implementation start point under it,
    /// and the decision that sent the findings there directly under both,
    /// at the decision's own time. Until the remediator starts, the
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
                if next != position {
                    oldestFirst.append(
                        TimelineRail.Row(
                            kind: .stageStart(.implementation), label: "Implementation started",
                            recordedAt: event.recorded_at))
                }
                position = next
                oldestFirst.append(
                    TimelineRail.Row(
                        kind: .event, label: "Remediation started · Round \(round.round)",
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
            if let start {
                oldestFirst.append(
                    TimelineRail.Row(
                        kind: .stageStart(start), label: "\(AttentionDisplay.label(start)) started",
                        recordedAt: event.recorded_at))
            }
            oldestFirst.append(
                TimelineRail.Row(
                    kind: recordsFailure(event) ? .failure : .event,
                    label: rowLabel(event, run: source?.run, pass: pass), recordedAt: event.recorded_at,
                    itemID: linkedItemID(event, items: items)))
        }
        oldestFirst += unstarted.map { decidedRow($0.round, $0.remediation, items: items) }
        var rows = Array(oldestFirst.reversed())
        if !rows.isEmpty { rows[0].isNewest = true }
        if rows.count > 1, case .stageStart = rows[1].kind { rows[1].isNewest = true }
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

    /// The run the page's status line and Task fold name: the task's current
    /// one, else the history's newest.
    static func currentRun(
        _ task: Components.Schemas.Task, in timeline: Components.Schemas.TaskTimeline?
    ) -> Components.Schemas.TaskTimelineRun? {
        let runs = timeline?.sections.flatMap(\.runs) ?? []
        return runs.first { $0.run_id == task.current_position?.value1.run_id } ?? runs.first
    }

    /// The pull request the task opened, by the history's newest record of it.
    static func pullRequest(_ timeline: Components.Schemas.TaskTimeline) -> Int? {
        events(timeline).first { $0.kind == .pr_opened }?.pr_number
    }

    /// One part of the header's documents line (◆D), in the order it draws.
    enum DocumentLink: Equatable {
        /// The intake issue, linked when its repository names a GitHub URL.
        case issue(number: Int, url: URL?)
        /// The current campaign's approved specification, by the digest
        /// its approval recorded; the revision only when a held approval
        /// names that digest.
        case specification(TaskSpecificationRequest)
        /// The task's PR, linked only when a held item names its repository.
        case pullRequest(number: Int, url: URL?)
        /// A loaded history that records no PR.
        case noPullRequest

        /// The words the line draws.
        var title: String {
            switch self {
            case .issue(let number, _): "Issue #\(number)"
            case .specification(let request): request.revision.map { "Specification rev \($0)" } ?? "Specification"
            case .pullRequest(let number, _): "Pull request #\(number)"
            case .noPullRequest: "No pull request yet"
            }
        }

        /// What VoiceOver reads for the words, without the number sign.
        var spokenTitle: String {
            switch self {
            case .issue(let number, _): "Issue \(number)"
            case .specification(let request):
                request.revision.map { "Specification revision \($0)" } ?? "Specification"
            case .pullRequest(let number, _): "Pull request \(number)"
            case .noPullRequest: title
            }
        }

        /// Where the link goes, as VoiceOver's hint. Nil for plain text:
        /// a part with nowhere to go is not a link.
        var accessibilityHint: String? {
            switch self {
            case .issue(_, let url), .pullRequest(_, let url): url == nil ? nil : "Opens on GitHub"
            case .specification: "Opens the specification reader"
            case .noPullRequest: nil
            }
        }
    }

    /// The documents line under the header's status: Issue for an
    /// issue-sourced task, the approved Specification, and the Pull Request.
    /// The PR is the Task fold's (the run's completion, else the history's
    /// `pr_opened`). The task names no repository, so the PR's link comes
    /// from an item held under the task whose `pr_reference` has its number;
    /// with none it is plain text. Before the history loads, a part the
    /// history decides is left out rather than guessed.
    @MainActor static func documentLinks(
        _ task: Components.Schemas.Task, timeline: Components.Schemas.TaskTimeline?,
        completion: Components.Schemas.WorkUnitCompletionFacts?, items: [Components.Schemas.AttentionItem]
    ) -> [DocumentLink] {
        var links: [DocumentLink] = []
        if case .issue_subject(let source)? = task.source?.value1 {
            links.append(
                .issue(
                    number: source.issue_subject.issue_number,
                    url: DecisionModel.issueURL(for: source.issue_subject)))
        }
        if let digest = approvedSpecificationDigest(task, in: timeline) {
            let revision = items.first { item in
                item._type == .spec_approval && DecisionDetailView.specificationClaim(in: item)?.digest == digest
            }.flatMap(DecisionDetailView.specificationRevisionIteration(in:))
            links.append(.specification(.init(digest: digest, revision: revision)))
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

    /// The agent the task's line for `stage` names, and its pool. The line
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
        return [line.agent] + [line.provenance?.value1.pool].compactMap { $0.map { "pool \($0)" } }
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

    /// The closed Task fold's one mono line.
    static func taskSummary(
        _ task: Components.Schemas.Task, position: TaskDisplay.Position?, stage: Components.Schemas.StageName?,
        run: Components.Schemas.TaskTimelineRun?
    ) -> String {
        ([TaskDisplay.projectName(task), TaskDisplay.currentPhaseAndRound(position)].compactMap { $0 }
            + [run?.attempt_number.map { "run \($0)" }].compactMap { $0 } + agentAndPool(task, stage: stage))
            .joined(separator: " · ")
    }

    /// The open Task fold's facts: each one a client source can state.
    /// `admission` is the current run's latest implementer admission, nil
    /// until that run's timeline has loaded.
    static func taskFacts(
        _ task: Components.Schemas.Task, position: TaskDisplay.Position?, stage: Components.Schemas.StageName?,
        run: Components.Schemas.TaskTimelineRun?, policyDigest: String?, pullRequest: Int?, historyLoaded: Bool,
        admission: Components.Schemas.InvocationAdmissionFacts? = nil
    ) -> [AttentionDisplay.BindingRow] {
        var rows: [AttentionDisplay.BindingRow] = [.init(label: "Project", value: TaskDisplay.projectName(task))]
        if let stage = TaskDisplay.currentPhaseAndRound(position) {
            rows.append(.init(label: "Stage", value: stage))
        }
        if let run {
            rows.append(.init(label: "Current Run", value: runTitle(run)))
        }
        let agent = agentAndPool(task, stage: stage)
        if !agent.isEmpty {
            rows.append(.init(label: "Agent and Pool", value: agent.joined(separator: " · ")))
        }
        if let admission {
            rows += RunDisplay.admissionRows(admission)
        }
        // Only a loaded history can say that no pull request exists yet.
        if let pullRequest {
            rows.append(.init(label: "Pull Request", value: "#\(pullRequest)"))
        } else if historyLoaded {
            rows.append(.init(label: "Pull Request", value: "None yet"))
        }
        if let policyDigest {
            rows.append(.init(label: "Policy Digest", value: ShortIdentifier.short(policyDigest)))
        }
        return rows
    }

    /// The conversations `items` name, each once, by its newest-first item.
    static func conversations(
        _ items: [Components.Schemas.AttentionItem],
        lookup: (String) -> Components.Schemas.ConversationSnapshot?
    ) -> [Components.Schemas.ConversationSnapshot] {
        var seen: Set<String> = []
        return
            items
            .sorted { ($0.created_at ?? .distantPast) > ($1.created_at ?? .distantPast) }
            .compactMap { item in
                guard let id = item.conversation_id, seen.insert(id).inserted else { return nil }
                return lookup(id)
            }
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

    /// An event's words on the rail. A run is named by its attempt and a
    /// review round by its number, never by an id; a run with no role or
    /// no attempt number keeps the label it had, since there is nothing to
    /// name it by. `pass` counts the run's work results, oldest first: a
    /// fix-up pass records a second one for the same attempt.
    private static func rowLabel(
        _ event: Components.Schemas.TaskEvent, run: Components.Schemas.TaskTimelineRun?, pass: Int
    ) -> String {
        let attempt = run?.role == nil ? nil : run?.attempt_number
        if let milestone = event.milestone?.value1, let attempt {
            if milestone.kind == .run_submitted { return "Attempt \(attempt) started" }
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

    /// A run inside a campaign is titled by its attempt; a run outside one,
    /// or missing its attempt number, by its short id, as the run timeline
    /// does. The exact id is in the page's Identifiers footer.
    static func runTitle(_ run: Components.Schemas.TaskTimelineRun) -> String {
        run.attempt_number.map { "Attempt \($0)" } ?? "Run \(ShortIdentifier.short(run.run_id))"
    }

    /// The run card's accessibility label: the title and the run's role,
    /// without the opaque id VoiceOver would otherwise read character by
    /// character. Attempt numbers restart per campaign, so when another run
    /// shares this title and role the label names the campaign; the label
    /// stands in for the button's content, and two controls must not read
    /// alike.
    static func runCardAccessibilityLabel(
        _ run: Components.Schemas.TaskTimelineRun, in timeline: Components.Schemas.TaskTimeline
    ) -> String {
        var label = "Open detailed run history for \(runTitle(run))"
        if let role = run.role?.value1 { label += ", \(self.label(role)) run" }
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

    /// The task's identifiers: the exact task id.
    static func technicalRows(taskID: String) -> [AttentionDisplay.BindingRow] {
        [.init(label: "Task ID", value: taskID)]
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

    /// The run card's milestones as rail rows, newest first, read the way
    /// the run's own page reads them. The task history holds them newest
    /// first, so they are reversed to the oldest-first order the run page's
    /// rows take. With
    /// the run's review rounds loaded, each milestone names its
    /// invocation's role and the findings adjudications that started a
    /// remediation join the rail.
    static func milestoneRows(
        _ run: Components.Schemas.TaskTimelineRun, reviewRounds: [Components.Schemas.RunReviewRound] = [],
        stages: [Components.Schemas.Stage] = []
    ) -> [TimelineRail.Row] {
        RunHistoryPresentation.rows(
            milestones: run.milestones.reversed(),
            context: {
                RunHistoryPresentation.roleContext(invocationID: $0, reviewRounds: reviewRounds, stages: stages)
            },
            reviewRounds: reviewRounds)
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

    /// The closed summary beside a section's keyword. The current section
    /// reads `current · 2 attempts · from Sep 11`; an earlier one reads
    /// `1 run · Sep 10`. Why an earlier campaign ended is not a recorded
    /// fact, so the summary does not say.
    static func sectionSummary(
        _ section: Components.Schemas.TaskTimelineSection, isCurrent: Bool, now: Date,
        locale: Locale = .current, timeZone: TimeZone = .current
    ) -> String {
        let count = section.runs.count
        let start = sectionStart(section).map {
            FreesideFormat.shortDate($0, now: now, locale: locale, timeZone: timeZone)
        }
        if isCurrent {
            return
                (["current", "\(count) \(count == 1 ? "attempt" : "attempts")"]
                + [start.map { "from \($0)" }]
                .compactMap { $0 }).joined(separator: " · ")
        }
        return (["\(count) \(count == 1 ? "run" : "runs")"] + [start].compactMap { $0 }).joined(separator: " · ")
    }

    /// The closed summary of a run's details: role, attempt reason, and the
    /// recorded hold. Nil when the run carries none of them, and then the
    /// disclosure has nothing to fold.
    static func runDetailsSummary(_ run: Components.Schemas.TaskTimelineRun) -> String? {
        let parts =
            [run.role.map { label($0.value1) }, run.attempt_reason]
            + [run.hold.map { "hold: \(RunDisplay.label($0.value1.reason))" }]
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

    /// The page's Identifiers footer, in page order: the task, then each
    /// campaign, each followed by its attempts, every owner under the name
    /// the page gives it. An owner with no identifiers (a section outside a
    /// campaign with no approved specification) is left out.
    /// The footer's groups in page order: the task, the leading card's
    /// decision, then each campaign and its runs once the history loads.
    static func identifierGroups(
        _ timeline: Components.Schemas.TaskTimeline?, taskID: String,
        decision: [AttentionDisplay.BindingRow] = []
    ) -> [IdentifierGroup] {
        var groups = [
            IdentifierGroup(owner: "Task", rows: technicalRows(taskID: taskID)),
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
    /// `task · decision · campaign · 2 runs`.
    static func identifiersSummary(
        _ timeline: Components.Schemas.TaskTimeline?, hasDecision: Bool = false
    ) -> String {
        let sections = timeline?.sections ?? []
        let campaigns = sections.count { !technicalRows(section: $0).isEmpty }
        let runs = sections.reduce(0) { $0 + $1.runs.count }
        return
            (["task", hasDecision ? "decision" : nil]
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

    static func hasRunDetails(_ run: Components.Schemas.TaskTimelineRun) -> Bool {
        runDetailsSummary(run) != nil || run.superseded_by != nil || run.parent_run_id != nil
    }

    /// A run card's chip. The run the task currently stands on repeats the
    /// task's own status and cut. Every other run is history, so faint: its
    /// synced outcome in the run vocabulary, marked superseded when the
    /// timeline says so, and no chip when neither is known.
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
        let outcome = synced.map { RunDisplay.label($0.outcome) }
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
        return label(event.kind)
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
    static func label(_ line: Components.Schemas.TaskEventTaskLine) -> String {
        let subject = label(line.role)
        guard let agent = line.agent else {
            return "\(subject) back to lineup" + (line.superseded_agent.map { " · \($0)" } ?? "")
        }
        let actor =
            switch line.source {
            case .router: " by router"
            case .triage: " by triage"
            default: ""
            }
        if let superseded = line.superseded_agent {
            return "\(subject) changed\(actor) · \(superseded) → \(agent)"
        }
        let fallback = line.provenance?.value1.passed_over.map { " instead of \($0.value1.agent)" } ?? ""
        return "\(subject) set\(actor) · \(agent)\(fallback)"
    }
}
