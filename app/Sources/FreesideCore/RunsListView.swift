import FreesideAPI
import SwiftUI

struct RunsListView: View {
    let runs: [Components.Schemas.RunSnapshot]
    let schedules: [Components.Schemas.ScheduleSnapshot]
    @Binding var selection: String?
    @State private var filter: RunListFilter
    private let navigationPath: Binding<[String]>?
    private let onRefresh: @MainActor () async -> Void

    init(
        runs: [Components.Schemas.RunSnapshot],
        schedules: [Components.Schemas.ScheduleSnapshot],
        selection: Binding<String?>,
        initialScope: RunListFilter.Scope = .active,
        navigationPath: Binding<[String]>? = nil,
        onRefresh: @escaping @MainActor () async -> Void = {}
    ) {
        self.runs = runs
        self.schedules = schedules
        _selection = selection
        _filter = State(initialValue: RunListFilter(scope: initialScope))
        self.navigationPath = navigationPath
        self.onRefresh = onRefresh
    }

    private var projects: [String] {
        Array(Set(runs.map(\.run.project_id))).sorted()
    }

    private var visibleRuns: [Components.Schemas.RunSnapshot] {
        filter.rows(in: runs)
    }

    var body: some View {
        let rows = visibleRuns
        VStack(spacing: 0) {
            FreesideSegmentedControl(
                accessibilityLabel: "Scope",
                segments: RunListFilter.Scope.allCases.map {
                    .init(value: $0, label: $0.label, count: filter.count(in: runs, scope: $0))
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
                        title: filter.scope == .all
                            ? "No runs" : "No \(filter.scope.label.lowercased()) runs",
                        systemImage: "point.3.connected.trianglepath.dotted",
                        description: "Runs in this scope will appear here.")
                    Spacer(minLength: 0)
                #else
                    UnavailableStateView(
                        title: filter.scope == .all
                            ? "No runs" : "No \(filter.scope.label.lowercased()) runs",
                        systemImage: "point.3.connected.trianglepath.dotted",
                        description: "Runs in this scope will appear here.")
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
        .navigationTitle("Runs")
        .onAppear { revealSelectedRun() }
        .onChange(of: selection) { revealSelectedRun() }
        .onChange(of: navigationPath?.wrappedValue) { revealSelectedRun() }
        .onChange(of: filter.scope) {
            repairFilterAndSelection()
        }
        .onChange(of: filter.projectID) {
            repairFilterAndSelection()
        }
        .onChange(of: projects) {
            revealSelectedRun()
            repairFilterAndSelection()
        }
        .onChange(of: runs.map(\.run.id)) {
            // A launch link can arrive before its snapshot has loaded.
            revealSelectedRun()
            repairFilterAndSelection()
        }
        #if os(iOS)
            .refreshable { await onRefresh() }
        #endif
    }

    private func listRows(_ snapshots: [Components.Schemas.RunSnapshot]) -> some View {
        ForEach(snapshots, id: \.run.id) { snapshot in
            Group {
                #if os(iOS)
                    NavigationLink(value: snapshot.run.id) { row(snapshot) }
                #else
                    row(snapshot)
                        .hidesSystemListSelection()
                #endif
            }
            .tag(snapshot.run.id)
            .listRowInsets(EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12))
            .listRowSeparator(.hidden)
            .listRowBackground(Color.clear)
        }
    }

    private func row(
        _ snapshot: Components.Schemas.RunSnapshot,
        now: Date? = nil
    ) -> some View {
        RunRowView(
            run: snapshot.run,
            identityLine: RunDisplay.identityLine(snapshot.run, runs: runs),
            secondaryLine: RunDisplay.secondaryLine(snapshot.run, runs: runs),
            spendLine: RunDisplay.spendLine(snapshot.run),
            schedules: schedules.filter {
                $0.schedule.run_id == snapshot.run.id && $0.schedule.status == .armed
            },
            isSelected: selection == snapshot.run.id,
            now: now)
    }

    private func repairFilterAndSelection() {
        if let projectID = filter.projectID, !projects.contains(projectID) {
            filter.projectID = nil
        }
        let availableIDs = Set(visibleRuns.map(\.run.id))
        #if os(iOS)
            if let path = navigationPath?.wrappedValue {
                let repairedPath = NavigationModel.repairedPath(
                    path,
                    availableIDs: availableIDs)
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

    private func revealSelectedRun() {
        #if os(iOS)
            let selectedID = navigationPath?.wrappedValue.last
        #else
            let selectedID = selection
        #endif
        if let run = runs.first(where: { $0.run.id == selectedID })?.run {
            filter.reveal(run)
        }
    }

    /// The project-owned row composition without List and Picker, whose
    /// AppKit-backed controls ImageRenderer cannot draw off-screen.
    @ViewBuilder
    func screenshotContent(now: Date) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(visibleRuns.prefix(5)), id: \.run.id) { snapshot in
                row(snapshot, now: now)
            }
        }
        .padding()
    }
}

/// Scope counts and rows use the same lifecycle and project predicate.
struct RunListFilter {
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

        func includes(_ run: Components.Schemas.Run) -> Bool {
            switch self {
            case .active: run.lifecycle == .active
            case .finished: run.lifecycle == .finished
            case .all: true
            }
        }
    }

    var scope: Scope = .active
    var projectID: String?

    func rows(in runs: [Components.Schemas.RunSnapshot]) -> [Components.Schemas.RunSnapshot] {
        RunDisplay.sortedRuns(runs.filter { includes($0.run, scope: scope) })
    }

    func count(in runs: [Components.Schemas.RunSnapshot], scope: Scope) -> Int {
        runs.count { includes($0.run, scope: scope) }
    }

    private func includes(_ run: Components.Schemas.Run, scope: Scope) -> Bool {
        (projectID == nil || run.project_id == projectID) && scope.includes(run)
    }

    mutating func reveal(_ run: Components.Schemas.Run) {
        if !scope.includes(run) {
            scope = run.lifecycle == .active ? .active : .finished
        }
        if let projectID, projectID != run.project_id {
            self.projectID = nil
        }
    }
}

/// One run as a ground-2 card. Selection uses a leading bar and wash,
/// with a stronger wash under Differentiate Without Color.
struct RunRowView: View {
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    @Environment(\.accessibilityDifferentiateWithoutColor) private var differentiateWithoutColor
    let run: Components.Schemas.Run
    let identityLine: String?
    let secondaryLine: RunDisplay.SecondaryLine
    let spendLine: String?
    let schedules: [Components.Schemas.ScheduleSnapshot]
    var isSelected = false
    /// A fixed clock for tests and screenshots. A live row leaves it nil and
    /// ticks its own, so the relative last-active text ages without a data
    /// change, as `InboxRowView` does.
    var now: Date?
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
                .padding(12)
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
        let text = Text(RunDisplay.metaLine(run, now: now))
            .font(FreesideFont.monoCaption)
            .foregroundStyle(Color.inkDim)
        #if os(macOS)
            if let exact = RunDisplay.exactActivityTimestamp(run) {
                text.help(exact)
            } else {
                text
            }
        #else
            text
        #endif
    }

    private func content(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 7) {
            if dynamicTypeSize.isAccessibilitySize {
                VStack(alignment: .leading, spacing: 7) {
                    title
                    if showsOutcomeBadge {
                        RunOutcomeBadge(outcome: run.outcome)
                            .frame(maxWidth: .infinity, alignment: .trailing)
                    }
                }
            } else {
                HStack(alignment: .firstTextBaseline) {
                    title
                    Spacer()
                    if showsOutcomeBadge {
                        RunOutcomeBadge(outcome: run.outcome)
                    }
                }
            }
            metaText(at: now)
            StageRail(
                title: nil,
                presentation: RunDisplay.stageRail(run),
                axis: .horizontal,
                showsSummaryText: false,
                labelStyle: .compact)
            if let identityLine {
                Text(identityLine)
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
            Group {
                switch secondaryLine {
                case .hold(let label):
                    // A hold is attention; on a failed or lost run it
                    // reads as part of the failure and keeps wax.
                    Label(label, systemImage: "pause.circle.fill")
                        .foregroundStyle(holdIsFailure ? Color.waxText : Color.accentText)
                case .milestone(let label):
                    Text(label)
                        .foregroundStyle(Color.inkDim)
                case .completion(let label):
                    Text(label)
                        .foregroundStyle(Color.ink)
                case .supersession(let label):
                    // Campaign identity already names its successor on this line.
                    if identityLine == nil {
                        Text(label)
                            .foregroundStyle(Color.inkDim)
                    }
                }
            }
            .font(FreesideFont.caption)
            if let spendLine {
                Text(spendLine)
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
            }
            if !schedules.isEmpty {
                WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                    ForEach(schedules, id: \.schedule.id) { snapshot in
                        ScheduleBadge(schedule: snapshot.schedule)
                    }
                }
            }
        }
    }

    private var title: some View {
        Text(RunDisplay.title(run))
            .font(FreesideFont.itemTitle)
            .foregroundStyle(Color.ink)
    }

    /// In progress is the row's resting state: the current rail dot and
    /// the hold or milestone line already say so, and a chip repeating
    /// it would make every active row look flagged.
    private var showsOutcomeBadge: Bool {
        run.outcome != .pending
    }

    private var holdIsFailure: Bool {
        switch run.outcome {
        case .failed, .lost: true
        case .unobserved, .pending, .published, .blocked, .completed: false
        }
    }
}

/// Ready is quiet in hue but full contrast (a neutral tick in the text
/// color, never green or the accent), in progress is water, blocked is
/// the accent, failed and lost are wax, not observed is a dashed faint.
struct RunOutcomeBadge: View {
    let outcome: Components.Schemas.RunOutcome

    var body: some View {
        StateChip(
            label: RunDisplay.label(outcome), color: color, dashed: outcome == .unobserved,
            glyph: outcome == .published ? "✓" : nil)
    }

    private var color: Color {
        switch outcome {
        case .unobserved: .inkDim
        case .pending: .waterText
        case .published, .completed: .ink
        case .blocked: .accentText
        case .failed, .lost: .waxText
        }
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
