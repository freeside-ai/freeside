import Foundation
import FreesideAPI
import SwiftUI

/// The one list both clients draw: Needs You, then Working and Finished
/// behind their disclosures, under a tools row. The list is a projection of
/// what the client already holds (`UnifiedList`). An unsent New Task draft
/// sits above Needs You as a line of its own, outside the projection and
/// its selection (#2935). An empty Needs You draws nothing, and a section
/// with no rows draws no head. On the Mac, Working and Finished rest at the
/// sidebar's foot while everything fits and follow Needs You down once it
/// doesn't; on iPhone they follow Needs You in one scrolling list.
struct UnifiedListView: View {
    /// The Needs You count: the rows the filtered list draws. Absent until
    /// the list has loaded, when a zero would read as nothing needing you.
    static func needsYouCount(in list: UnifiedList, store: InboxStore) -> Int? {
        store.loadState == .loaded ? list.needsYou.count : nil
    }

    /// What the Mac's detail pane says once the operator concludes the last
    /// open item (R13), naming the project when one is filtered. The list
    /// itself draws nothing for an empty Needs You.
    static func emptyNeedsYou(projectID: String?) -> (title: String, description: String) {
        (
            projectID == nil ? "Nothing needs you." : "Nothing in this project needs you.",
            "Decisions you make will show up here."
        )
    }

    /// The tools row's filter state.
    static func filterState(projectID: String?) -> String {
        switch projectID {
        case nil: "All Projects"
        case UnifiedList.noProject: "No project"
        case let projectID?: projectID
        }
    }

    /// Whether the filter button takes its active face: a filter is applied,
    /// `No project` included, or the popover is open.
    static func filterIsActive(projectID: String?, isPresented: Bool) -> Bool {
        projectID != nil || isPresented
    }

    #if os(macOS)
        @FocusedValue(\.decisionCommandActions) private var decisionCommandActions
    #endif
    let store: InboxStore
    let tasks: [Components.Schemas.TaskSnapshot]
    /// The run list, so a task's stage line can follow its newest run
    /// (`TaskDisplay.position`).
    var runs: [Components.Schemas.RunSnapshot] = []
    var taskTimelines: [String: Components.Schemas.TaskTimeline] = [:]
    /// Run timelines by run id, whose review rounds tell the task rail a
    /// remediation from a review, as the task detail header reads them.
    var runTimelines: [String: Components.Schemas.RunTimeline] = [:]
    var cursors: SyncCursors?
    /// The coordinator's cache generation, so a row's timeline read that a
    /// same-revision bootstrap dropped is reissued
    /// (`TaskPageView.TimelineRequestKey`).
    var cacheGeneration = 0
    var onLoadTimeline: @MainActor (String) async -> Void = { _ in }
    let navigation: NavigationModel
    let preferences: ListSectionPreferences
    var launchProjectID: String?
    /// Why New Task is disabled, shown above the list so the disabled action
    /// explains itself; nil while the composer is available.
    var newTaskBlockedReason: String?
    /// The New Task draft, drawn as a row while there is one. Only that
    /// row reads it, so typing in the composer redraws nothing else.
    var draftStore: NewTaskDraftStore?
    var lastUpdatedAt: Date?
    var onRefresh: @MainActor () async -> Void = {}
    var onRevealTechnicalDetails: (String) -> Void = { _ in }
    /// The look-alike marker's clock, as the task page's header reads it.
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @State private var projectFilterPresented = false
    /// How many Finished rows an expanded Finished draws (`FinishedPage`);
    /// collapsing it starts the page over.
    @State private var finishedLimit = FinishedPage.initialLimit
    #if os(macOS)
        /// The column's keyboard focus, which a click on a row takes, so the
        /// arrow keys, j/k, and Return act on the list.
        @FocusState private var listFocused: Bool
    #endif

    private var list: UnifiedList {
        UnifiedList(
            open: store.openSnapshots, closed: store.closedSnapshots, tasks: tasks,
            projectFilter: store.projectID)
    }

    private var selection: UnifiedSelection? {
        list.selection(
            attentionItemID: navigation.attentionSelection, taskID: navigation.taskSelection)
    }

    var body: some View {
        let list = list
        Group {
            switch store.loadState {
            case .idle, .loading:
                ProgressView()
            case .failed(let message):
                UnavailableStateView(glyph: .inbox, title: "Couldn't load the list", description: message)
            case .loaded:
                VStack(spacing: 0) {
                    tools(list)
                    rows(list)
                    #if os(iOS)
                        LastUpdatedLabel(lastUpdatedAt: lastUpdatedAt)
                            .padding(.horizontal)
                            .padding(.vertical, 6)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    #endif
                }
            }
        }
        .navigationTitle("Freeside")
        #if os(iOS)
            // The list hides its bar, so this is for the pages it pushes,
            // which inherit the mode.
            .navigationBarTitleDisplayMode(.inline)
        #endif
        .task {
            if let launchProjectID { store.applyLaunchProjectFilter(launchProjectID) }
        }
        .onAppear { revealSelection() }
        .onChange(of: navigation.path) {
            revealSelection()
            // Keep a page grown to a routed row once the selection moves on.
            finishedLimit = max(finishedLimit, finishedPage(list).rows.count)
        }
        .onChange(of: store.projectID) { repairSelection() }
        .onChange(of: preferences.finishedExpanded) { _, expanded in
            if !expanded { finishedLimit = FinishedPage.initialLimit }
        }
        .onChange(of: store.projects) {
            if store.freshness == .fresh {
                store.finishLaunchProjectRepair()
            } else {
                store.repairProjectFilter()
            }
            repairSelection()
        }
        .onChange(of: store.freshness) {
            if store.freshness == .fresh {
                store.finishLaunchProjectRepair()
            }
        }
        .onChange(of: store.openSnapshots.map(\.item.id)) { previous, _ in
            repairSelection(previouslyOpen: Set(previous))
        }
        .onChange(of: list.rows.map(\.id)) {
            // A link can arrive before the row it names has loaded.
            revealSelection()
            repairSelection()
        }
        #if os(iOS)
            .refreshable { await onRefresh() }
        #endif
    }

    private func repairSelection(previouslyOpen: Set<String> = []) {
        guard store.loadState == .loaded else { return }
        navigation.repairSelection(in: list, previouslyOpen: previouslyOpen)
    }

    /// The Finished rows an expanded Finished draws: the page, grown to hold
    /// the selected row.
    private func finishedPage(_ list: UnifiedList) -> FinishedPage {
        FinishedPage(list.finished, limit: finishedLimit, holding: selection)
    }

    /// A route to a row the filter hides drops the filter, so the operator
    /// sees what they opened.
    private func revealSelection() {
        guard store.projectID != nil, let selection, !list.holds(selection) else { return }
        let unfiltered = UnifiedList(
            open: store.openSnapshots, closed: store.closedSnapshots, tasks: tasks, projectFilter: nil)
        if unfiltered.holds(selection) {
            store.selectProjectFilter(nil)
        }
    }

    /// One line: the filter's state, the filter, and on the Mac New Task (the
    /// iPhone's is in its bottom bar). A disabled New Task says why on a line
    /// of its own.
    private func tools(_ list: UnifiedList) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            if let newTaskBlockedReason {
                Text(newTaskBlockedReason)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
            HStack(spacing: 8) {
                Text(Self.filterState(projectID: store.projectID))
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(store.projectID == nil ? Color.inkDim : Color.ink)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .accessibilityHidden(true)
                Spacer(minLength: 8)
                Button {
                    projectFilterPresented = true
                } label: {
                    ToolsRowGlyphFace(
                        title: "Filter by project", symbol: "line.3.horizontal.decrease", glyphSize: 14,
                        isActive: Self.filterIsActive(
                            projectID: store.projectID, isPresented: projectFilterPresented))
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Filter by project")
                .accessibilityValue(Self.filterState(projectID: store.projectID))
                #if os(macOS)
                    .help("Filter by project")
                #endif
                .popover(isPresented: $projectFilterPresented) {
                    ProjectFilterPopover(
                        projects: list.knownProjects, offersNoProject: list.offersNoProject,
                        selection: store.projectID,
                        onSelect: { projectID in
                            projectFilterPresented = false
                            guard store.projectID != projectID else { return }
                            navigation.recordOperatorNavigation()
                            store.selectProjectFilter(projectID)
                            // The operator's filter beats a selection it
                            // hides, even a linked item's; a route does the
                            // opposite (`revealSelection`).
                            if let selection, !list.holds(selection) { navigation.select(nil) }
                        }
                    )
                    .presentationCompactAdaptation(.popover)
                }
                #if os(macOS)
                    Button {
                        navigation.newTaskComposerPresented = true
                    } label: {
                        ToolsRowGlyphFace(title: "New Task", symbol: "plus.square", glyphSize: 15)
                    }
                    .buttonStyle(.plain)
                    .help(newTaskBlockedReason ?? "New Task")
                    .disabled(newTaskBlockedReason != nil)
                #endif
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal)
        .padding(.bottom, 10)
    }

    @ViewBuilder
    private func rows(_ list: UnifiedList) -> some View {
        let names = TaskDisplay.ambiguousTaskIDs(list.rows.compactMap(\.taskSnapshot))
        #if os(iOS)
            List {
                needsYouLines(list, ambiguousTaskIDs: names)
                sectionLines(list, ambiguousTaskIDs: names)
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
        #else
            GeometryReader { geometry in
                ScrollViewReader { proxy in
                    ScrollView {
                        // At least the sidebar's height, so while everything
                        // fits the gap takes the slack and holds the sections
                        // at the foot; past that the gap is gone and Needs
                        // You pushes them down. Exact in one layout pass,
                        // where a spacer row in a List would be measured
                        // after the fact and jump for a frame.
                        VStack(spacing: 0) {
                            LazyVStack(spacing: 0) {
                                needsYouLines(list, ambiguousTaskIDs: names)
                            }
                            Spacer(minLength: 0)
                            LazyVStack(spacing: 0) {
                                sectionLines(list, ambiguousTaskIDs: names)
                            }
                        }
                        .padding(.vertical, 4)
                        .frame(minHeight: geometry.size.height, alignment: .top)
                        // The column's own effect is off below; a control
                        // inside that draws no ring of its own keeps the
                        // system's.
                        .focusEffectDisabled(false)
                    }
                    .focusable()
                    .focusEffectDisabled()
                    .focused($listFocused)
                    .onKeyPress(keys: [.upArrow, .downArrow], phases: [.down, .repeat]) { press in
                        // Arrow keys arrive flagged as function keys, so
                        // only a held modifier the operator chose declines.
                        guard press.modifiers.isDisjoint(with: [.command, .option, .control, .shift])
                        else { return .ignored }
                        navigation.moveSelection(
                            by: press.key == .downArrow ? 1 : -1, along: drawnRows(list), list: list)
                        return .handled
                    }
                    .onKeyPress(characters: CharacterSet(charactersIn: "jk"), phases: .down) { press in
                        guard press.modifiers.isEmpty else { return .ignored }
                        navigation.moveAttentionSelection(by: press.characters == "j" ? 1 : -1, list: list)
                        return .handled
                    }
                    .onKeyPress(.return, phases: .down) { press in
                        guard press.modifiers.isEmpty,
                            decisionCommandActions?.canTakeRecommendation == true
                        else { return .ignored }
                        decisionCommandActions?.takeRecommendation()
                        return .handled
                    }
                    .onChange(of: selection) { _, selected in
                        // The least scroll that shows the row, as the system
                        // list did, for a key step or a route from elsewhere.
                        guard let selected else { return }
                        withAnimation(.easeOut(duration: 0.15)) { proxy.scrollTo(selected) }
                    }
                }
            }
        #endif
    }

    #if os(macOS)
        /// The rows the column draws, top to bottom, that the arrow keys
        /// walk: Needs You, then each expanded section's rows.
        private func drawnRows(_ list: UnifiedList) -> [UnifiedRow] {
            list.needsYou + (preferences.workingExpanded ? list.working : [])
                + (preferences.finishedExpanded
                    ? Array(finishedPage(list).rows) : [])
        }

        private func select(_ row: UnifiedRow) {
            navigation.select(row.destination)
            listFocused = true
        }
    #endif

    /// The draft line and the Needs You rows: the top of the list.
    @ViewBuilder
    private func needsYouLines(_ list: UnifiedList, ambiguousTaskIDs: Set<String>) -> some View {
        if let draftStore {
            DraftListRow(draftStore: draftStore, navigation: navigation)
        }
        listRows(list.needsYou, ambiguousTaskIDs: ambiguousTaskIDs)
    }

    /// Working and Finished, each drawn only while it has rows; an expanded
    /// Finished draws a page of its newest rows, compact, and a Show More
    /// line while older ones remain.
    @ViewBuilder
    private func sectionLines(_ list: UnifiedList, ambiguousTaskIDs: Set<String>) -> some View {
        @Bindable var preferences = preferences
        if !list.working.isEmpty {
            ListSectionHead(
                title: "Working", count: list.working.count, isExpanded: $preferences.workingExpanded
            )
            .unifiedListLine()
            if preferences.workingExpanded {
                listRows(list.working, ambiguousTaskIDs: ambiguousTaskIDs)
            }
        }
        if !list.finished.isEmpty {
            ListSectionHead(
                title: "Finished", count: list.finished.count, isExpanded: $preferences.finishedExpanded
            )
            .unifiedListLine()
            if preferences.finishedExpanded {
                let page = finishedPage(list)
                listRows(Array(page.rows), ambiguousTaskIDs: ambiguousTaskIDs, compact: true)
                if page.hiddenCount > 0 {
                    ShowMoreLine(count: page.nextStep) { finishedLimit += FinishedPage.step }
                        .unifiedListLine()
                }
            }
        }
    }

    /// Rows that select: on iPhone, list rows that push what they open; on
    /// the Mac, rows of the column that select on a click. A tap rather
    /// than a Button there, which would merge the row's VoiceOver elements
    /// into one label; the container says which row is selected, as a
    /// list's cells do, and takes VoiceOver's activate. A compact row loads
    /// no timeline: it draws no stage.
    private func listRows(
        _ rows: [UnifiedRow], ambiguousTaskIDs: Set<String>, compact: Bool = false
    ) -> some View {
        ForEach(rows) { row in
            let drawn = rowView(row, ambiguousTaskIDs: ambiguousTaskIDs, compact: compact)
            #if os(iOS)
                loadingTimeline(
                    of: compact ? nil : row,
                    NavigationLink(value: NavigationModel.PhoneRoute.destination(row.destination)) { drawn }
                        .tag(row.id)
                )
                .listRowInsets(
                    EdgeInsets(top: compact ? 1 : 4, leading: 12, bottom: compact ? 1 : 4, trailing: 12)
                )
                .listRowSeparator(.hidden)
                .listRowBackground(Color.clear)
            #else
                loadingTimeline(
                    of: compact ? nil : row,
                    drawn
                        .contentShape(Rectangle())
                        .onTapGesture { select(row) }
                        .accessibilityElement(children: .contain)
                        .accessibilityAddTraits(selection == row.id ? .isSelected : [])
                        .accessibilityAction { select(row) }
                )
                .id(row.id)
                .padding(.horizontal, 12 + UnifiedListView.sideMargin)
                .padding(.vertical, compact ? 1 : 4)
            #endif
        }
    }

    /// Loads a task row's timeline, which its stage line reads; nil loads
    /// nothing.
    private func loadingTimeline(of row: UnifiedRow?, _ content: some View) -> some View {
        content.task(id: row.flatMap(timelineKey)) {
            if let taskID = row?.taskSnapshot?.task.id {
                await onLoadTimeline(taskID)
            }
        }
    }

    private func timelineKey(_ row: UnifiedRow) -> TaskPageView.TimelineRequestKey? {
        row.taskSnapshot.map {
            TaskPageView.TimelineRequestKey(
                snapshot: $0, cursors: cursors, cacheGeneration: cacheGeneration)
        }
    }

    @ViewBuilder
    private func rowView(
        _ row: UnifiedRow, ambiguousTaskIDs: Set<String>, compact: Bool = false, now: Date? = nil
    ) -> some View {
        let isSelected = selection == row.id
        if compact {
            switch row.content {
            case .item(let snapshot):
                FinishedRowView(
                    subject: .item(snapshot.item), isSelected: isSelected, now: now,
                    onRevealTechnicalDetails: { onRevealTechnicalDetails(snapshot.item.id) })
            case .task(let snapshot, _):
                FinishedRowView(
                    subject: .task(
                        snapshot.task,
                        status: TaskDisplay.finishedRow(snapshot.task, position: taskPosition(snapshot).position)
                            .keyword,
                        showsIdentifier: ambiguousTaskIDs.contains(snapshot.task.id)),
                    isSelected: isSelected, now: now)
            }
        } else {
            fullRowView(row, ambiguousTaskIDs: ambiguousTaskIDs, isSelected: isSelected, now: now)
        }
    }

    @ViewBuilder
    private func fullRowView(
        _ row: UnifiedRow, ambiguousTaskIDs: Set<String>, isSelected: Bool, now: Date?
    ) -> some View {
        switch row.content {
        case .item(let snapshot):
            itemRow(snapshot.item, task: nil, isSelected: isSelected, now: now)
        case .task(let snapshot, let openItems):
            let task = snapshot.task
            let (position, stage) = taskPosition(snapshot)
            let identifier = ambiguousTaskIDs.contains(task.id)
            if let leading = openItems.first {
                let context = TaskDisplay.rowContext(task, position: position)
                // A live row has no fixed clock; the marker reads `now` only
                // to decide whether to print the year.
                itemRow(
                    leading.item,
                    task: .init(
                        task: task,
                        context: identifier
                            ? "\(context) · \(TaskDisplay.lookAlikeMarker(task, now: now ?? .now, locale: locale, timeZone: timeZone))"
                            : context),
                    isSelected: isSelected, now: now)
            } else {
                TaskRowView(
                    task: task, position: position, isSelected: isSelected, now: now,
                    showsIdentifier: identifier,
                    sectionRow: TaskDisplay.isActive(task)
                        ? TaskDisplay.workingRow(task, position: position, stage: stage)
                        : TaskDisplay.finishedRow(task, position: position))
            }
        }
    }

    private func itemRow(
        _ item: Components.Schemas.AttentionItem, task: InboxRowView.TaskHeading?, isSelected: Bool, now: Date?
    ) -> some View {
        InboxRowView(
            item: item, task: task, isSelected: isSelected, now: now,
            onRevealTechnicalDetails: { onRevealTechnicalDetails(item.id) })
    }

    /// The stage and round the task detail header reads too, so the two
    /// agree (`TaskDisplay.currentStage`). No attention items: a row says
    /// where the task stands, and Needs You already says what it asks.
    private func taskPosition(
        _ snapshot: Components.Schemas.TaskSnapshot
    ) -> (position: TaskDisplay.Position?, stage: TaskDisplay.CurrentStage?) {
        let history = taskTimelines[snapshot.task.id]
        let stage = TaskDisplay.currentStage(
            snapshot.task, runs: runs, history: history,
            reviewRounds: history.map { TaskTimelinePresentation.reviewRounds($0, runTimelines: runTimelines) } ?? [:])
        return (TaskDisplay.position(snapshot.task, runs: runs, history: history, stage: stage), stage)
    }

    /// The list without List, whose AppKit-backed control ImageRenderer
    /// cannot draw off-screen: the sidebar as the operator sees it, each
    /// section capped at `rowLimit` rows. A `sidebarHeight` frames the Mac
    /// sidebar at that height, with Working and Finished at its foot.
    func screenshotContent(
        now: Date, rowLimit: Int = 4, sidebarHeight: CGFloat? = nil,
        finishedLimit: Int = FinishedPage.initialLimit
    ) -> some View {
        let list = list
        let names = TaskDisplay.ambiguousTaskIDs(list.rows.compactMap(\.taskSnapshot))
        let screenshotRows = { (rows: [UnifiedRow], compact: Bool) in
            ForEach(Array(rows.prefix(rowLimit))) { row in
                rowView(row, ambiguousTaskIDs: names, compact: compact, now: now)
            }
        }
        let page = FinishedPage(list.finished, limit: finishedLimit)
        return VStack(alignment: .leading, spacing: 8) {
            tools(list)
                .padding(.horizontal, -16)
            if let row = draftStore?.draft.row {
                DraftRowView(row: row)
                    .padding(.bottom, DraftRowView.gapBelow)
            }
            screenshotRows(list.needsYou, false)
            if sidebarHeight != nil {
                Spacer(minLength: 0)
            }
            if !list.working.isEmpty {
                ListSectionHead(
                    title: "Working", count: list.working.count,
                    isExpanded: .constant(preferences.workingExpanded))
                if preferences.workingExpanded {
                    screenshotRows(list.working, false)
                }
            }
            if !list.finished.isEmpty {
                ListSectionHead(
                    title: "Finished", count: list.finished.count,
                    isExpanded: .constant(preferences.finishedExpanded))
                if preferences.finishedExpanded {
                    screenshotRows(Array(page.rows), true)
                    if page.hiddenCount > 0 {
                        ShowMoreLine(count: page.nextStep) {}
                    }
                }
            }
        }
        .padding()
        .frame(height: sidebarHeight, alignment: .top)
        .background(Color.sidebarGround)
    }
}

/// The line under an expanded Finished's page that draws its next older
/// rows (`FinishedPage`), in the dim mono of the section counts.
struct ShowMoreLine: View {
    let count: Int
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Text("Show \(count) More")
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .frame(maxWidth: .infinity, minHeight: 22, alignment: .leading)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .freesideFocusRing(cornerRadius: 4)
        .accessibilityLabel("Show \(count) more finished")
    }
}

/// The Draft line of the list, present only while a draft exists. It opens
/// the composer on the draft; its menu, and a swipe on iPhone, discard it.
/// A line selection passes over, like the section heads.
private struct DraftListRow: View {
    let draftStore: NewTaskDraftStore
    let navigation: NavigationModel

    var body: some View {
        if let row = draftStore.draft.row {
            Button {
                navigation.newTaskComposerPresented = true
            } label: {
                DraftRowView(row: row)
            }
            .buttonStyle(.plain)
            .accessibilityHint("Opens New Task with this draft")
            .contextMenu {
                Button("Discard Draft", role: .destructive) { draftStore.clear() }
            }
            #if os(iOS)
                .swipeActions(edge: .trailing) {
                    Button("Discard", role: .destructive) { draftStore.clear() }
                }
            #endif
            .unifiedListLine(bottomInset: 4 + DraftRowView.gapBelow)
        }
    }
}

extension UnifiedListView {
    /// Where the system list drew its rows on the Mac, 8pt outside a row's
    /// own inset; the column keeps it, so rows sit where they did under
    /// the tools row.
    fileprivate static let sideMargin: CGFloat = {
        #if os(macOS)
            8
        #else
            0
        #endif
    }()
}

extension View {
    /// A line of the list that selection passes over.
    fileprivate func unifiedListLine(bottomInset: CGFloat = 4) -> some View {
        #if os(iOS)
            selectionDisabled()
                .listRowInsets(EdgeInsets(top: 4, leading: 16, bottom: bottomInset, trailing: 16))
                .listRowSeparator(.hidden)
                .listRowBackground(Color.clear)
        #else
            padding(
                EdgeInsets(
                    top: 4, leading: 16 + UnifiedListView.sideMargin, bottom: bottomInset,
                    trailing: 16 + UnifiedListView.sideMargin))
        #endif
    }
}

/// The unsent New Task draft on the shared row: the `Draft` keyword, the
/// first line of its text, and its project on the context line. Its frame
/// is dashed and unfilled, the way a decision card frames text that would be
/// sent, and a gap sets it apart from Needs You, so it never reads as an item
/// waiting on the operator.
struct DraftRowView: View {
    let row: NewTaskDraft.Row

    /// The space the row adds below itself, past the list's row gap.
    static let gapBelow: CGFloat = 12

    var body: some View {
        SidebarRow(keyword: .init(text: "Draft", color: .inkDim), isDraft: true) {
            EmptyView()
        } title: {
            Text(row.title)
                .font(FreesideFont.rowTitle)
                .foregroundStyle(Color.ink)
                .lineLimit(1)
        } context: {
            SidebarRowContext(text: row.project)
        } closing: {
            EmptyView()
        }
        .contentShape(Rectangle())
    }
}

/// The tools row's icon button face, as the 30 Oct handoff draws it (`glyphBtn`):
/// 30 by 26pt, growing with Dynamic Type rather than clipping the icon. The
/// tap target stays 28pt tall around the 26pt face. The title is the icon's
/// accessibility label.
private struct ToolsRowGlyphFace: View {
    let title: String
    let symbol: String
    let isActive: Bool

    @Environment(\.isEnabled) private var isEnabled
    @ScaledMetric private var glyphSize: CGFloat

    init(title: String, symbol: String, glyphSize: CGFloat, isActive: Bool = false) {
        self.title = title
        self.symbol = symbol
        self.isActive = isActive
        _glyphSize = ScaledMetric(
            wrappedValue: screenshotMetricBase(glyphSize, relativeTo: .subheadline), relativeTo: .subheadline)
    }

    private var glyphColor: Color {
        guard isEnabled else { return .inkFaint }
        return isActive ? .ground2 : .inkDim
    }

    var body: some View {
        Label(title, systemImage: symbol)
            .labelStyle(.iconOnly)
            .font(.system(size: glyphSize, weight: .medium))
            .foregroundStyle(glyphColor)
            .padding(4)
            .frame(minWidth: 30, minHeight: 26)
            .background(isActive ? Color.accentText : Color.ground2, in: RoundedRectangle(cornerRadius: 6))
            .overlay {
                if !isActive {
                    RoundedRectangle(cornerRadius: 6).strokeBorder(Color.secondaryBorder, lineWidth: 1)
                }
            }
            .frame(minHeight: 28)
            .contentShape(Rectangle())
    }
}

/// A section's head: the keyword, its disclosure when the section collapses,
/// and its count in the mono the row times use.
struct ListSectionHead: View {
    let title: String
    let count: Int
    var isExpanded: Binding<Bool>?

    var body: some View {
        if let isExpanded {
            Button {
                isExpanded.wrappedValue.toggle()
            } label: {
                head(isExpanded: isExpanded.wrappedValue)
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: 4)
            .accessibilityValue(isExpanded.wrappedValue ? "Expanded" : "Collapsed")
            .accessibilityAddTraits(.isHeader)
        } else {
            head(isExpanded: nil)
                .accessibilityAddTraits(.isHeader)
        }
    }

    private func head(isExpanded: Bool?) -> some View {
        HStack(spacing: 6) {
            if let isExpanded {
                Image(systemName: "arrowtriangle.right.fill")
                    .font(FreesideFont.disclosureGlyph)
                    .foregroundStyle(Color.accentText)
                    .rotationEffect(.degrees(isExpanded ? 90 : 0))
            }
            KeywordLabel(text: title)
            Spacer(minLength: 8)
            Text("\(count)")
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
        }
        .frame(minHeight: 22)
        .contentShape(Rectangle())
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(title), \(count)")
    }
}

/// The surface a sidebar row sits on (R19), shared by the inbox and task
/// rows so the two lists select and hover alike. An unselected row is a
/// bordered item on ground-2; the selected row drops the border for the
/// accent wash under a 4pt accent bar, so selection adds geometry as well as
/// color and reads the same with Differentiate Without Color. On macOS an
/// unselected row takes the hover cut under the pointer. The unsent draft's
/// row has no fill and a dashed strong-rule border, geometry again rather
/// than color alone.
struct SidebarRowSurface: ViewModifier {
    let isSelected: Bool
    var isDraft = false

    /// The row's padding as the frames draw it: 10 by 12 on the Mac, 11 by
    /// 13 on iPhone.
    #if os(macOS)
        static let padding = CGSize(width: 12, height: 10)
    #else
        static let padding = CGSize(width: 13, height: 11)
    #endif
    /// The gap between a row's lines, as the frames draw it.
    static let lineGap: CGFloat = 5

    /// From xxxLarge the chips leave the keyword's line for one of their own.
    static func stacksHeader(at dynamicTypeSize: DynamicTypeSize) -> Bool {
        dynamicTypeSize >= .xxxLarge
    }

    func body(content: Content) -> some View {
        HStack(spacing: 0) {
            if isSelected {
                Rectangle()
                    .fill(Color.accentText)
                    .frame(width: 4)
                    .accessibilityHidden(true)
            }
            hoverable(
                content
                    .padding(.vertical, Self.padding.height)
                    // The selected row gives 2pt of its inset to the bar.
                    .padding(.leading, Self.padding.width - (isSelected ? 2 : 0))
                    .padding(.trailing, Self.padding.width)
                    .frame(maxWidth: .infinity, alignment: .leading))
        }
        .background(
            RoundedRectangle(cornerRadius: 8).fill(isSelected ? Color.accentWash : isDraft ? .clear : .ground2)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(
                    isSelected ? Color.clear : isDraft ? .ruleStrong : .itemBorder,
                    style: StrokeStyle(lineWidth: 1, dash: isDraft ? [4, 3] : []))
        )
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder
    private func hoverable(_ row: some View) -> some View {
        #if os(macOS)
            if isSelected {
                row
            } else {
                row.freesideHover(cornerRadius: 8)
            }
        #else
            row
        #endif
    }
}

/// One sidebar row in the grammar both lists draw (R31), top to bottom: a
/// keyword line with the exceptional chips trailing, a serif line, one mono
/// context line with the time trailing, and at most one closing line. The
/// inbox and the task list differ only in what they put in each slot; the
/// layout, its stacking at the large text sizes, and the surface are this
/// view's, so the two lists cannot drift apart.
struct SidebarRow<Badges: View, Title: View, Context: View, Closing: View>: View {
    let keyword: SidebarRowKeyword
    var time: SidebarRowTime? = nil
    var isSelected = false
    /// True draws the unviewed dot at the keyword line's trailing end
    /// (#2930). It takes no width when absent, so no other row reserves any.
    var showsUnviewedDot = false
    /// False draws no chip line where the header stacks.
    var hasBadges = false
    /// True for a row inside a bordered group, which draws the border and
    /// the hairlines between rows: the row takes a group row's padding in
    /// place of its own surface.
    var grouped = false
    /// True for the unsent New Task draft, which takes the draft surface.
    var isDraft = false
    @ViewBuilder var badges: Badges
    @ViewBuilder var title: Title
    @ViewBuilder var context: Context
    @ViewBuilder var closing: Closing
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        if grouped {
            lines
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            lines.modifier(SidebarRowSurface(isSelected: isSelected, isDraft: isDraft))
        }
    }

    private var lines: some View {
        VStack(alignment: .leading, spacing: SidebarRowSurface.lineGap) {
            if SidebarRowSurface.stacksHeader(at: dynamicTypeSize) {
                HStack(alignment: .center, spacing: 10) {
                    keywordLine
                    if showsUnviewedDot {
                        Spacer(minLength: 0)
                        UnviewedDot()
                    }
                }
                if hasBadges {
                    badges
                }
            } else {
                HStack(alignment: .center, spacing: 10) {
                    keywordLine
                    Spacer(minLength: 0)
                    badges
                    if showsUnviewedDot {
                        UnviewedDot()
                    }
                }
            }
            title
            if dynamicTypeSize.isAccessibilitySize {
                // The time drops under the context line, which then has the
                // row's width to itself.
                context
                timeText
            } else {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    context
                    if time != nil {
                        Spacer(minLength: 8)
                        timeText
                            .fixedSize()
                    }
                }
            }
            closing
        }
        // The row's click and long press belong to the row: on the Mac a
        // selectable title takes the click that selects a task row, and on
        // iOS it takes the long press from the row's menu. Each row's menu
        // offers Copy for its text instead (app/README.md, Copyable Text).
        .textSelection(.disabled)
    }

    /// The keyword sized to its own height so a long one wraps at the large
    /// text sizes; left flexible, the stacked header truncated it. The
    /// Agent mark sits beside it where both fit on one line and under it
    /// otherwise, so a long status wraps at its words, never inside one.
    private var keywordLine: some View {
        let word = KeywordLabel(text: keyword.text, color: keyword.color)
            .fixedSize(horizontal: false, vertical: true)
        return Group {
            if keyword.marksAgent {
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        word
                        KeywordLabel(text: "Agent")
                    }
                    VStack(alignment: .leading, spacing: 2) {
                        word
                        KeywordLabel(text: "Agent")
                    }
                }
            } else {
                word
            }
        }
        .accessibilityHidden(!keyword.isSpoken)
    }

    @ViewBuilder
    private var timeText: some View {
        if let time {
            let text = Text(time.text)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .lineLimit(1)
                .accessibilityHidden(!time.isSpoken)
            #if os(macOS)
                if let exact = time.exact {
                    text.help(exact)
                } else {
                    text
                }
            #else
                text
            #endif
        }
    }
}

/// The unviewed mark (#2930): an accent dot at the top right of the row, so
/// it neither indents the row nor costs the title any width. Its color is the
/// accent for every item, never the item's posture (devlog 2026-11-09-0820).
/// VoiceOver reads it as Unviewed after the type and any chips.
struct UnviewedDot: View {
    static let diameter: CGFloat = 8

    var body: some View {
        Circle()
            .fill(Color.accentText)
            .frame(width: Self.diameter, height: Self.diameter)
            .accessibilityLabel("Unviewed")
    }
}

/// A sidebar row's keyword line: the leading word, with `AGENT` after it
/// when the row's serif line is agent prose (plan §9).
struct SidebarRowKeyword {
    let text: String
    var color: Color = .inkDim
    var marksAgent = false
    /// False where the row speaks this line elsewhere, in its own order.
    var isSpoken = true
}

/// The time trailing a sidebar row's context line; `exact` is the macOS
/// hover help.
struct SidebarRowTime {
    let text: String
    var exact: String? = nil
    /// False where the context line already speaks the time.
    var isSpoken = true
}

/// One attention row (R31) on the shared row: the type as a keyword with its
/// chips trailing, the summary, and one context line. An item that stands for
/// its task in Needs You takes the task's name and context in their place.
struct InboxRowView: View {
    struct TaskHeading {
        let task: Components.Schemas.Task
        let context: String
    }

    let item: Components.Schemas.AttentionItem
    var task: TaskHeading?
    var isSelected = false
    var now: Date?
    var onRevealTechnicalDetails: () -> Void = {}
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if let now {
                row(at: now)
            } else {
                TimelineView(.periodic(from: .now, by: 60)) { context in
                    row(at: context.date)
                }
            }
        }
        .contextMenu { contextMenu }
    }

    private func row(at now: Date) -> some View {
        let context = AttentionDisplay.rowContext(item)
        let time = AttentionDisplay.relativeRowTime(item, now: now).map {
            SidebarRowTime(text: $0, exact: AttentionDisplay.exactRowTimestamp(item, now: now))
        }
        // The keyword is the word the decision card's eyebrow uses for this
        // item (R27), so the row and the card it opens name the type alike.
        return SidebarRow(
            keyword: .init(text: DecisionCardComposition.eyebrow(for: item).keyword),
            time: time, isSelected: isSelected, showsUnviewedDot: isUnviewed, hasBadges: hasBadges
        ) {
            rowBadges
        } title: {
            // The summary says what needs attention, so it takes the serif
            // and the main ink; the type above it is the quiet line (visual
            // audit D01).
            // Two lines at the standard sizes. An accessibility size fits
            // fewer words on a line, so the cap lifts there rather than hide
            // what the default size shows (R22).
            if let task {
                TaskNameLabel(
                    name: task.task.display_names.task, font: FreesideFont.rowTitle,
                    monoFont: FreesideFont.monoValue,
                    lineLimit: dynamicTypeSize.isAccessibilitySize ? nil : 2)
            } else {
                Text(AttentionDisplay.rowSummary(item))
                    .font(FreesideFont.rowTitle)
                    .foregroundStyle(Color.ink)
                    .lineLimit(dynamicTypeSize.isAccessibilitySize ? nil : 2)
            }
        } context: {
            if let task {
                SidebarRowContext(text: task.context)
            } else {
                contextLine(context)
            }
        } closing: {
            EmptyView()
        }
    }

    private func contextLine(_ context: AttentionDisplay.RowContext) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            contextSegment(context.project)
            if let workUnit = context.workUnit {
                separator
                contextSegment(workUnit)
                if workUnit.isAgentClaim {
                    CompactMark(text: "Agent")
                }
            }
        }
    }

    private var rowBadges: some View {
        HStack(spacing: 5) {
            if AttentionDisplay.showsLifecycleBadge(item.status) {
                StatusBadge(status: item.status)
            }
            if AttentionDisplay.showsDegradedBadge(item) {
                StateChip(label: "Degraded", color: .waxText)
            }
        }
    }

    /// The unviewed mark (#2930): an open item whose current decision surface
    /// no device has opened. A closed item never shows it.
    private var isUnviewed: Bool {
        AttentionDisplay.isUnviewed(item)
    }

    private var hasBadges: Bool {
        AttentionDisplay.showsLifecycleBadge(item.status)
            || AttentionDisplay.showsDegradedBadge(item)
    }

    static func stacksHeader(at dynamicTypeSize: DynamicTypeSize) -> Bool {
        SidebarRowSurface.stacksHeader(at: dynamicTypeSize)
    }

    private var separator: some View {
        Text("·")
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .accessibilityHidden(true)
    }

    @ViewBuilder
    private func contextSegment(_ segment: AttentionDisplay.RowContext.Segment) -> some View {
        let text = Text(segment.value)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .lineLimit(1)
            .truncationMode(.middle)
        #if os(macOS)
            text.help(segment.value)
        #else
            text
        #endif
    }

    private var contextMenu: some View {
        AttentionItemMenu(
            item: item, taskName: task?.task.display_names.task.text,
            onRevealTechnicalDetails: onRevealTechnicalDetails)
    }
}

/// An item row's context menu, the same for a full row and a compact
/// Finished row: what it can copy, then Reveal in Identifiers.
struct AttentionItemMenu: View {
    let item: Components.Schemas.AttentionItem
    var taskName: String?
    var onRevealTechnicalDetails: () -> Void

    var body: some View {
        let evidenceDigests = AttentionDisplay.uniqueEvidenceDigests(item)
        if let taskName {
            Button("Copy task name") { Clipboard.copy(taskName) }
        }
        Button("Copy summary") { Clipboard.copy(AttentionDisplay.rowSummary(item)) }
        Button("Copy item ID") { Clipboard.copy(item.id) }
        if let subject = AttentionDisplay.copyableSubjectReference(item) {
            Button(subject.label) { Clipboard.copy(subject.value) }
        }
        if evidenceDigests.count == 1, let digest = evidenceDigests.first {
            Button("Copy evidence digest") { Clipboard.copy(digest) }
        } else if evidenceDigests.count > 1 {
            Menu("Copy evidence digest") {
                ForEach(evidenceDigests, id: \.self) { digest in
                    Button(digest) { Clipboard.copy(digest) }
                }
            }
        }
        Divider()
        Button("Reveal in Identifiers") { onRevealTechnicalDetails() }
    }
}

struct StatusBadge: View {
    let status: Components.Schemas.ItemStatus

    var body: some View {
        // A lifecycle status is history, so it takes the faint cut (#2097).
        StateChip(label: AttentionDisplay.label(status), cut: .faint)
            .fixedSize()
    }
}
