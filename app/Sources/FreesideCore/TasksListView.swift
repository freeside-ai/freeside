import FreesideAPI
import SwiftUI

struct TasksListView: View {
    let tasks: [Components.Schemas.TaskSnapshot]
    /// The run list, so a task's stage line can follow its newest run
    /// (`TaskDisplay.position`).
    let runs: [Components.Schemas.RunSnapshot]
    let attentionItems: [Components.Schemas.AttentionItemSnapshot]
    let taskTimelines: [String: Components.Schemas.TaskTimeline]
    let cursors: SyncCursors?
    private let onLoadTimeline: @MainActor (String) async -> Void
    @Binding var selection: String?
    @State private var filter: TaskListFilter
    private let navigationPath: Binding<[String]>?
    private let onRefresh: @MainActor () async -> Void
    /// Why New Task is disabled, shown above the list so the disabled action
    /// explains itself; nil while the composer is available.
    let newTaskBlockedReason: String?

    init(
        tasks: [Components.Schemas.TaskSnapshot],
        runs: [Components.Schemas.RunSnapshot],
        attentionItems: [Components.Schemas.AttentionItemSnapshot] = [],
        taskTimelines: [String: Components.Schemas.TaskTimeline] = [:],
        cursors: SyncCursors? = nil,
        onLoadTimeline: @escaping @MainActor (String) async -> Void = { _ in },
        selection: Binding<String?>,
        initialScope: TaskListFilter.Scope = .active,
        navigationPath: Binding<[String]>? = nil,
        onRefresh: @escaping @MainActor () async -> Void = {},
        newTaskBlockedReason: String? = nil
    ) {
        self.tasks = tasks
        self.runs = runs
        self.attentionItems = attentionItems
        self.taskTimelines = taskTimelines
        self.cursors = cursors
        self.onLoadTimeline = onLoadTimeline
        _selection = selection
        _filter = State(initialValue: TaskListFilter(scope: initialScope))
        self.navigationPath = navigationPath
        self.onRefresh = onRefresh
        self.newTaskBlockedReason = newTaskBlockedReason
    }

    private var projects: [String] {
        TaskDisplay.knownProjects(in: tasks)
    }

    private var visibleTasks: [Components.Schemas.TaskSnapshot] {
        filter.rows(in: tasks)
    }

    var body: some View {
        let rows = visibleTasks
        VStack(spacing: 0) {
            if let newTaskBlockedReason {
                Text(newTaskBlockedReason)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal)
                    .padding(.bottom, 8)
            }
            FreesideSegmentedControl(
                accessibilityLabel: "Scope",
                segments: TaskListFilter.Scope.allCases.map {
                    .init(value: $0, label: $0.label, count: filter.count(in: tasks, scope: $0))
                },
                selection: $filter.scope
            )
            .padding(.horizontal)
            .padding(.bottom, 8)

            Menu {
                Picker("Project", selection: $filter.projectID) {
                    Text("All projects").tag(String?.none)
                    ForEach(projects, id: \.self) { project in
                        Text(project).tag(String?.some(project))
                    }
                }
                .pickerStyle(.inline)
            } label: {
                FreesideMenuTriggerLabel(title: filter.projectID ?? "All projects")
            }
            .menuStyle(.button)
            .buttonStyle(.plain)
            .menuIndicator(.hidden)
            .accessibilityLabel("Project")
            .accessibilityValue(filter.projectID ?? "All projects")
            .padding(.horizontal)
            .padding(.bottom, 8)

            if rows.isEmpty {
                #if os(macOS)
                    Spacer(minLength: 0)
                    SidebarEmptyState(
                        title: emptyTitle, description: "Tasks in this scope will appear here.")
                    Spacer(minLength: 0)
                #else
                    UnavailableStateView(
                        title: emptyTitle, description: "Tasks in this scope will appear here.")
                #endif
            } else {
                #if os(iOS)
                    List {
                        listRows(rows)
                    }
                    .listStyle(.plain)
                    .scrollContentBackground(.hidden)
                #else
                    List(selection: $selection) {
                        listRows(rows)
                    }
                    .listStyle(.plain)
                    .scrollContentBackground(.hidden)
                #endif
            }
        }
        .navigationTitle("Tasks")
        .onAppear { revealSelectedTask() }
        .onChange(of: selection) { revealSelectedTask() }
        .onChange(of: navigationPath?.wrappedValue) { revealSelectedTask() }
        .onChange(of: filter.scope) {
            repairFilterAndSelection()
        }
        .onChange(of: filter.projectID) {
            repairFilterAndSelection()
        }
        .onChange(of: projects) {
            revealSelectedTask()
            repairFilterAndSelection()
        }
        .onChange(of: tasks.map(\.task.id)) {
            // A launch link can arrive before its snapshot has loaded.
            revealSelectedTask()
            repairFilterAndSelection()
        }
        #if os(iOS)
            .refreshable { await onRefresh() }
        #endif
    }

    private var emptyTitle: String {
        filter.scope == .all ? "No tasks" : "No \(filter.scope.label.lowercased()) tasks"
    }

    private func listRows(_ snapshots: [Components.Schemas.TaskSnapshot]) -> some View {
        ForEach(snapshots, id: \.task.id) { snapshot in
            Group {
                #if os(iOS)
                    NavigationLink(value: snapshot.task.id) { row(snapshot) }
                #else
                    row(snapshot)
                        .hidesSystemListSelection()
                #endif
            }
            .tag(snapshot.task.id)
            .task(id: TaskTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: cursors)) {
                await onLoadTimeline(snapshot.task.id)
            }
            .listRowInsets(EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12))
            .listRowSeparator(.hidden)
            .listRowBackground(Color.clear)
        }
    }

    /// The visible tasks that share a name, so a row appends a short id only
    /// where two rows would otherwise read alike.
    private var ambiguousTaskIDs: Set<String> {
        TaskDisplay.ambiguousTaskIDs(visibleTasks)
    }

    private func row(
        _ snapshot: Components.Schemas.TaskSnapshot,
        now: Date? = nil
    ) -> some View {
        TaskRowView(
            task: snapshot.task,
            position: TaskDisplay.position(
                snapshot.task, runs: runs, attentionItems: attentionItems, history: taskTimelines[snapshot.task.id]),
            isSelected: selection == snapshot.task.id,
            now: now,
            showsIdentifier: ambiguousTaskIDs.contains(snapshot.task.id))
    }

    private func repairFilterAndSelection() {
        if let projectID = filter.projectID, !projects.contains(projectID) {
            filter.projectID = nil
        }
        let availableIDs = Set(visibleTasks.map(\.task.id))
        #if os(iOS)
            if let path = navigationPath?.wrappedValue {
                let repairedPath = NavigationModel.repairedTaskPath(
                    path,
                    availableTaskIDs: availableIDs)
                if repairedPath != path {
                    navigationPath?.wrappedValue = repairedPath
                }
            }
        #else
            if let selection, !availableIDs.contains(selection) {
                self.selection = nil
            }
        #endif
    }

    private func revealSelectedTask() {
        #if os(iOS)
            let selectedID = navigationPath?.wrappedValue.first
        #else
            let selectedID = selection
        #endif
        if let task = tasks.first(where: { $0.task.id == selectedID })?.task {
            filter.reveal(task)
        }
    }

    /// The project-owned row composition without List and Picker, whose
    /// AppKit-backed controls ImageRenderer cannot draw off-screen.
    @ViewBuilder
    func screenshotContent(now: Date) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(visibleTasks.prefix(5)), id: \.task.id) { snapshot in
                row(snapshot, now: now)
            }
        }
        .padding()
    }
}

/// Scope counts and rows use the same lifecycle and project predicate.
struct TaskListFilter {
    enum Scope: String, CaseIterable, Identifiable {
        case active, finished, all

        var id: Self { self }
        var label: String {
            switch self {
            case .active: "Active"
            case .finished: "Finished"
            case .all: "All"
            }
        }

        func includes(_ task: Components.Schemas.Task) -> Bool {
            switch self {
            case .active: TaskDisplay.isActive(task)
            case .finished: !TaskDisplay.isActive(task)
            case .all: true
            }
        }
    }

    var scope: Scope = .active
    var projectID: String?

    func rows(in tasks: [Components.Schemas.TaskSnapshot]) -> [Components.Schemas.TaskSnapshot] {
        TaskDisplay.sortedTasks(tasks.filter { includes($0.task, scope: scope) })
    }

    func count(in tasks: [Components.Schemas.TaskSnapshot], scope: Scope) -> Int {
        tasks.count { includes($0.task, scope: scope) }
    }

    private func includes(_ task: Components.Schemas.Task, scope: Scope) -> Bool {
        (projectID == nil || task.project_id == projectID) && scope.includes(task)
    }

    mutating func reveal(_ task: Components.Schemas.Task) {
        if !scope.includes(task) {
            scope = TaskDisplay.isActive(task) ? .active : .finished
        }
        if let projectID, projectID != task.project_id {
            self.projectID = nil
        }
    }
}

/// One task as a sidebar row (R31): the name, the status chip on its own
/// line beneath it, the project, issue, and last-active meta line, the phase
/// line, round and hold on one line, and a guidance sentence only when it
/// says more than "open this row". Armed schedules are the task timeline's
/// to show. Selection and hover are the inbox row's (`SidebarRowSurface`).
struct TaskRowView: View {
    let task: Components.Schemas.Task
    let position: TaskDisplay.Position?
    var isSelected = false
    /// A fixed clock for tests and screenshots. A live row leaves it nil and
    /// ticks its own, so the relative last-active text ages without a data
    /// change, as `InboxRowView` does.
    var now: Date?
    /// Set when another visible task shares this one's name, so the meta line
    /// carries a short task id to tell the two rows apart.
    var showsIdentifier = false

    var body: some View {
        if let now {
            card(at: now)
        } else {
            TimelineView(.periodic(from: .now, by: 60)) { context in
                card(at: context.date)
            }
        }
    }

    private func card(at now: Date) -> some View {
        content(at: now)
            .modifier(SidebarRowSurface(isSelected: isSelected, verticalPadding: 14))
    }

    /// The meta line, whose last-active segment is coarse; macOS hover
    /// carries the exact instant, as inbox rows do.
    @ViewBuilder
    private func metaText(at now: Date) -> some View {
        let identifier = showsIdentifier ? " · \(ShortIdentifier.short(task.id))" : ""
        // VoiceOver keeps "last active"; the eye reads the time alone.
        let text = Text(TaskDisplay.metaLine(task, now: now, labelsActivity: false) + identifier)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .accessibilityLabel(TaskDisplay.metaLine(task, now: now) + identifier)
        #if os(macOS)
            text.help(TaskDisplay.exactActivityTimestamp(task))
        #else
            text
        #endif
    }

    private func content(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            let lines = TaskDisplay.rowLines(task, position: position)
            TaskNameLabel(
                name: task.display_names.task, font: FreesideFont.statement,
                monoFont: FreesideFont.monoValue)
            // The chip sits above the meta line, but VoiceOver reads the
            // status with the progress it heads, as it always has: the chip
            // is hidden and the progress block speaks every string in order.
            StateChip(label: lines.status, cut: TaskDisplay.statusCut(task, position: position))
                .accessibilityHidden(true)
            metaText(at: now)
            let guidance = lines.visibleGuidance(attention: position?.attention == true)
            Group {
                if lines.phases == nil, lines.facts.isEmpty, guidance == nil {
                    // A task with no position draws no progress line, but
                    // VoiceOver still reads its status and guidance after the
                    // meta line, and an element needs a frame to be reached.
                    Color.clear.frame(height: 1)
                } else {
                    VStack(alignment: .leading, spacing: 6) {
                        if let phases = lines.phases {
                            progressText(phases)
                        }
                        if !lines.facts.isEmpty {
                            progressText(lines.facts.joined(separator: " · "))
                        }
                        switch guidance {
                        case .link(let title):
                            FreesideLink(title: title, face: FreesideFont.noticeAction)
                        case .sentence(let sentence): progressText(sentence)
                        case nil: EmptyView()
                        }
                    }
                }
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(lines.all.joined(separator: ", "))
        }
    }
}

extension TaskRowView {
    fileprivate func progressText(_ line: String) -> some View {
        Text(line)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }
}
