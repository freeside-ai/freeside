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
    /// The schedule list, so a row can show the armed watches and deadlines
    /// attached to the task's runs.
    let schedules: [Components.Schemas.ScheduleSnapshot]
    @Binding var selection: String?
    @State private var filter: TaskListFilter
    private let navigationPath: Binding<[String]>?
    private let onRefresh: @MainActor () async -> Void

    init(
        tasks: [Components.Schemas.TaskSnapshot],
        runs: [Components.Schemas.RunSnapshot],
        schedules: [Components.Schemas.ScheduleSnapshot],
        attentionItems: [Components.Schemas.AttentionItemSnapshot] = [],
        taskTimelines: [String: Components.Schemas.TaskTimeline] = [:],
        cursors: SyncCursors? = nil,
        onLoadTimeline: @escaping @MainActor (String) async -> Void = { _ in },
        selection: Binding<String?>,
        initialScope: TaskListFilter.Scope = .active,
        navigationPath: Binding<[String]>? = nil,
        onRefresh: @escaping @MainActor () async -> Void = {}
    ) {
        self.tasks = tasks
        self.runs = runs
        self.attentionItems = attentionItems
        self.taskTimelines = taskTimelines
        self.cursors = cursors
        self.onLoadTimeline = onLoadTimeline
        self.schedules = schedules
        _selection = selection
        _filter = State(initialValue: TaskListFilter(scope: initialScope))
        self.navigationPath = navigationPath
        self.onRefresh = onRefresh
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
                        title: emptyTitle,
                        systemImage: "checklist",
                        description: "Tasks in this scope will appear here.")
                    Spacer(minLength: 0)
                #else
                    UnavailableStateView(
                        title: emptyTitle,
                        systemImage: "checklist",
                        description: "Tasks in this scope will appear here.")
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
            schedules: TaskDisplay.armedSchedules(for: snapshot.task, in: schedules),
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

/// One task as a ground-2 card: the name, the status chip on its own line
/// beneath it, the project, issue, and last-active meta line, the phase
/// line, round and hold on one line, a guidance sentence only when it says
/// more than "open this row", and the armed watches and deadlines of the
/// task's runs. Selection uses a leading bar and wash, with a stronger wash
/// under Differentiate Without Color.
struct TaskRowView: View {
    @Environment(\.accessibilityDifferentiateWithoutColor) private var differentiateWithoutColor
    let task: Components.Schemas.Task
    let position: TaskDisplay.Position?
    var schedules: [Components.Schemas.ScheduleSnapshot] = []
    var isSelected = false
    /// A fixed clock for tests and screenshots. A live row leaves it nil and
    /// ticks its own, so the relative last-active text ages without a data
    /// change, as `InboxRowView` does.
    var now: Date?
    /// Set when another visible task shares this one's name, so the meta line
    /// carries a short task id to tell the two rows apart.
    var showsIdentifier = false
    var differentiateWithoutColorOverride: Bool?

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
        HStack(spacing: 0) {
            if isSelected {
                Rectangle()
                    .fill(Color.accentText)
                    .frame(width: 4)
                    .accessibilityHidden(true)
            }
            content(at: now)
                .padding(14)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(
                    isSelected
                        ? (effectiveDifferentiateWithoutColor ? Color.accentWash : .accentWashSoft)
                        : .ground2)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(isSelected ? Color.clear : .rule, lineWidth: 1)
        )
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    private var effectiveDifferentiateWithoutColor: Bool {
        differentiateWithoutColorOverride ?? differentiateWithoutColor
    }

    /// The meta line, whose last-active segment is coarse; macOS hover
    /// carries the exact instant, as inbox rows do.
    @ViewBuilder
    private func metaText(at now: Date) -> some View {
        let identifier = showsIdentifier ? " · \(ShortIdentifier.short(task.id))" : ""
        // VoiceOver keeps "last active"; the eye reads the time alone.
        let text = Text(TaskDisplay.metaLine(task, now: now, labelsActivity: false) + identifier)
            .font(FreesideFont.monoCaption)
            .foregroundStyle(Color.inkDim)
            .accessibilityLabel(TaskDisplay.metaLine(task, now: now) + identifier)
        #if os(macOS)
            text.help(TaskDisplay.exactActivityTimestamp(task))
        #else
            text
        #endif
    }

    private func content(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 7) {
            let lines = TaskDisplay.rowLines(task, position: position)
            TaskNameLabel(name: task.display_names.task)
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
                    VStack(alignment: .leading, spacing: 7) {
                        if let phases = lines.phases {
                            progressText(phases)
                        }
                        if !lines.facts.isEmpty {
                            progressText(lines.facts.joined(separator: " · "))
                        }
                        switch guidance {
                        case .link(let title): FreesideLink(title: title, style: .caption)
                        case .sentence(let sentence): progressText(sentence)
                        case nil: EmptyView()
                        }
                    }
                }
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(lines.all.joined(separator: ", "))
            if !schedules.isEmpty {
                WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                    ForEach(schedules, id: \.schedule.id) { snapshot in
                        ScheduleBadge(schedule: snapshot.schedule)
                    }
                }
            }
        }
    }
}

extension TaskRowView {
    fileprivate func progressText(_ line: String) -> some View {
        Text(line)
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }
}

/// A ground-3 pill: mono, kind and fire time joined by a middle dot.
private struct ScheduleBadge: View {
    let schedule: Components.Schemas.Schedule

    var body: some View {
        Group {
            if let fireAt = schedule.fire_at {
                Text("\(RunDisplay.label(schedule.kind)) · \(fireAt.formatted(date: .omitted, time: .shortened))")
            } else {
                Text(RunDisplay.label(schedule.kind))
            }
        }
        .font(FreesideFont.chip)
        .textCase(.lowercase)
        .lineLimit(1)
        .fixedSize()
        .foregroundStyle(Color.inkDim)
        .padding(.horizontal, 7)
        .padding(.vertical, 3)
        .background(Color.ground3, in: Capsule())
    }
}
