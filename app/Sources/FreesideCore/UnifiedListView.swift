import Foundation
import FreesideAPI
import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

/// The one list both clients draw: Needs you, then Working and Finished
/// behind their disclosures, under a tools row. The list is a projection of
/// what the client already holds (`UnifiedList`).
struct UnifiedListView: View {
    /// The Needs you count: the rows the filtered list draws. Absent until
    /// the list has loaded, when a zero would read as nothing needing you.
    static func needsYouCount(in list: UnifiedList, store: InboxStore) -> Int? {
        store.loadState == .loaded ? list.needsYou.count : nil
    }

    /// What an empty Needs you says (R13), naming the project when one is
    /// filtered; the detail pane beside it says the same two lines.
    static func emptyNeedsYou(projectID: String?) -> (title: String, description: String) {
        (
            projectID == nil ? "Nothing needs you." : "Nothing in this project needs you.",
            "Decisions you make will show up here."
        )
    }

    /// The tools row's filter state.
    static func filterState(projectID: String?) -> String {
        switch projectID {
        case nil: "All projects"
        case UnifiedList.noProject: "No project"
        case let projectID?: projectID
        }
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
    var lastUpdatedAt: Date?
    var onRefresh: @MainActor () async -> Void = {}
    var onRevealTechnicalDetails: (String) -> Void = { _ in }
    @State private var projectFilterPresented = false

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
                UnavailableStateView(glyph: .inbox, title: "Couldn't load the inbox", description: message)
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
            // The tools row sits between the bar and the list, so a large
            // title would hold its height through the list's scroll.
            .navigationBarTitleDisplayMode(.inline)
        #endif
        .task {
            if let launchProjectID { store.applyLaunchProjectFilter(launchProjectID) }
        }
        .onAppear { revealSelection() }
        .onChange(of: navigation.path) { revealSelection() }
        .onChange(of: store.projectID) { repairSelection() }
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

    /// One line: the filter's state, the filter, and New Task. A disabled
    /// New Task says why on a line of its own.
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
                    FreesideMenuTriggerLabel(title: "Project")
                        .fixedSize()
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Filter by project")
                .accessibilityValue(Self.filterState(projectID: store.projectID))
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
                Button {
                    navigation.newTaskComposerPresented = true
                } label: {
                    Label("New Task", systemImage: "plus")
                        .labelStyle(.iconOnly)
                        .font(FreesideFont.sans(.subheadline, weight: .medium))
                        .foregroundStyle(Color.accentText)
                        .frame(minWidth: 28, minHeight: 28)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .help(newTaskBlockedReason ?? "New Task")
                .disabled(newTaskBlockedReason != nil)
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
                sections(list, ambiguousTaskIDs: names)
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
        #else
            List(selection: listSelection) {
                sections(list, ambiguousTaskIDs: names)
            }
            .listStyle(.plain)
            .scrollContentBackground(.hidden)
            .onKeyPress(
                characters: CharacterSet(charactersIn: "jk"), phases: .down
            ) { press in
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
        #endif
    }

    #if os(macOS)
        private var listSelection: Binding<UnifiedSelection?> {
            Binding(
                get: { selection },
                set: { id in
                    navigation.select(list.rows.first { $0.id == id }?.destination)
                })
        }
    #endif

    @ViewBuilder
    private func sections(_ list: UnifiedList, ambiguousTaskIDs: Set<String>) -> some View {
        @Bindable var preferences = preferences
        sectionRow {
            ListSectionHead(title: "Needs you", count: list.needsYou.count)
        }
        if list.needsYou.isEmpty {
            sectionRow { needsYouEmptyState }
        } else {
            listRows(list.needsYou, ambiguousTaskIDs: ambiguousTaskIDs)
        }
        sectionRow {
            ListSectionHead(
                title: "Working", count: list.working.count, isExpanded: $preferences.workingExpanded)
        }
        if preferences.workingExpanded {
            listRows(list.working, ambiguousTaskIDs: ambiguousTaskIDs)
        }
        sectionRow {
            ListSectionHead(
                title: "Finished", count: list.finished.count, isExpanded: $preferences.finishedExpanded)
        }
        if preferences.finishedExpanded {
            listRows(list.finished, ambiguousTaskIDs: ambiguousTaskIDs)
        }
    }

    /// A line of the list that selection passes over.
    private func sectionRow(@ViewBuilder content: () -> some View) -> some View {
        content()
            .selectionDisabled()
            .listRowInsets(EdgeInsets(top: 4, leading: 16, bottom: 4, trailing: 16))
            .listRowSeparator(.hidden)
            .listRowBackground(Color.clear)
    }

    private var needsYouEmptyState: some View {
        let empty = Self.emptyNeedsYou(projectID: store.projectID)
        return SidebarEmptyState(glyph: .inbox, title: empty.title, description: empty.description)
            .padding(.bottom, 16)
    }

    private func listRows(_ rows: [UnifiedRow], ambiguousTaskIDs: Set<String>) -> some View {
        ForEach(rows) { row in
            Group {
                #if os(iOS)
                    NavigationLink(value: row.destination) {
                        rowView(row, ambiguousTaskIDs: ambiguousTaskIDs)
                    }
                #else
                    rowView(row, ambiguousTaskIDs: ambiguousTaskIDs)
                        .hidesSystemListSelection()
                #endif
            }
            .tag(row.id)
            .task(id: timelineKey(row)) {
                if let taskID = row.taskSnapshot?.task.id {
                    await onLoadTimeline(taskID)
                }
            }
            .listRowInsets(EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12))
            .listRowSeparator(.hidden)
            .listRowBackground(Color.clear)
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
        _ row: UnifiedRow, ambiguousTaskIDs: Set<String>, now: Date? = nil
    ) -> some View {
        let isSelected = selection == row.id
        switch row.content {
        case .item(let snapshot):
            itemRow(snapshot.item, task: nil, isSelected: isSelected, now: now)
        case .task(let snapshot, let openItems):
            let task = snapshot.task
            let (position, stage) = taskPosition(snapshot)
            let identifier = ambiguousTaskIDs.contains(task.id)
            if let leading = openItems.first {
                let context = TaskDisplay.rowContext(task, position: position)
                itemRow(
                    leading.item,
                    task: .init(
                        task: task,
                        context: identifier ? "\(context) · \(ShortIdentifier.short(task.id))" : context),
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
    /// where the task stands, and Needs you already says what it asks.
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
    /// section capped at `rowLimit` rows.
    func screenshotContent(now: Date, rowLimit: Int = 4) -> some View {
        let list = list
        let names = TaskDisplay.ambiguousTaskIDs(list.rows.compactMap(\.taskSnapshot))
        let screenshotRows = { (rows: [UnifiedRow]) in
            ForEach(Array(rows.prefix(rowLimit))) { row in
                rowView(row, ambiguousTaskIDs: names, now: now)
            }
        }
        return VStack(alignment: .leading, spacing: 8) {
            tools(list)
                .padding(.horizontal, -16)
            ListSectionHead(title: "Needs you", count: list.needsYou.count)
            if list.needsYou.isEmpty {
                needsYouEmptyState
            }
            screenshotRows(list.needsYou)
            ListSectionHead(
                title: "Working", count: list.working.count,
                isExpanded: .constant(preferences.workingExpanded))
            if preferences.workingExpanded {
                screenshotRows(list.working)
            }
            ListSectionHead(
                title: "Finished", count: list.finished.count,
                isExpanded: .constant(preferences.finishedExpanded))
            if preferences.finishedExpanded {
                screenshotRows(list.finished)
            }
        }
        .padding()
        .background(Color.sidebarGround)
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
/// unselected row takes the hover cut under the pointer.
struct SidebarRowSurface: ViewModifier {
    let isSelected: Bool

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
        .background(RoundedRectangle(cornerRadius: 8).fill(isSelected ? Color.accentWash : .ground2))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(isSelected ? Color.clear : .itemBorder, lineWidth: 1)
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
    /// False draws no chip line where the header stacks.
    var hasBadges = false
    @ViewBuilder var badges: Badges
    @ViewBuilder var title: Title
    @ViewBuilder var context: Context
    @ViewBuilder var closing: Closing
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        VStack(alignment: .leading, spacing: SidebarRowSurface.lineGap) {
            if SidebarRowSurface.stacksHeader(at: dynamicTypeSize) {
                keywordLine
                if hasBadges {
                    badges
                }
            } else {
                HStack(alignment: .center, spacing: 10) {
                    keywordLine
                    Spacer(minLength: 0)
                    badges
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
        .modifier(SidebarRowSurface(isSelected: isSelected))
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
/// its task in Needs you takes the task's name and context in their place.
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
            time: time, isSelected: isSelected, hasBadges: hasBadges
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

    @ViewBuilder
    private var contextMenu: some View {
        let evidenceDigests = AttentionDisplay.uniqueEvidenceDigests(item)
        if let task {
            Button("Copy task name") { copy(task.task.display_names.task.text) }
        }
        Button("Copy summary") { copy(AttentionDisplay.rowSummary(item)) }
        Button("Copy item ID") { copy(item.id) }
        if let subject = AttentionDisplay.copyableSubjectReference(item) {
            Button(subject.label) { copy(subject.value) }
        }
        if evidenceDigests.count == 1, let digest = evidenceDigests.first {
            Button("Copy evidence digest") { copy(digest) }
        } else if evidenceDigests.count > 1 {
            Menu("Copy evidence digest") {
                ForEach(evidenceDigests, id: \.self) { digest in
                    Button(digest) { copy(digest) }
                }
            }
        }
        Divider()
        Button("Reveal in Technical Details") { onRevealTechnicalDetails() }
    }

    private func copy(_ value: String) {
        #if os(macOS)
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(value, forType: .string)
        #elseif os(iOS)
            UIPasteboard.general.string = value
        #endif
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
