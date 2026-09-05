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
            Picker("Scope", selection: $filter.scope) {
                ForEach(RunListFilter.Scope.allCases) { scope in
                    Text("\(scope.label) \(filter.count(in: runs, scope: scope))").tag(scope)
                }
            }
            .pickerStyle(.segmented)
            .padding(.horizontal)
            .padding(.bottom, 8)

            Picker("Project", selection: $filter.projectID) {
                Text("All projects").tag(String?.none)
                ForEach(projects, id: \.self) { project in
                    Text(project).tag(String?.some(project))
                }
            }
            .pickerStyle(.menu)
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

    private func row(_ snapshot: Components.Schemas.RunSnapshot) -> some View {
        RunRowView(
            run: snapshot.run,
            identityLine: RunDisplay.identityLine(snapshot.run, runs: runs),
            secondaryLine: RunDisplay.secondaryLine(snapshot.run, runs: runs),
            spendLine: RunDisplay.spendLine(snapshot.run),
            schedules: schedules.filter {
                $0.schedule.run_id == snapshot.run.id && $0.schedule.status == .armed
            },
            isSelected: selection == snapshot.run.id)
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
    func screenshotContent() -> some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(visibleRuns.prefix(5)), id: \.run.id) { snapshot in
                row(snapshot)
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
    var differentiateWithoutColorOverride: Bool?

    var body: some View {
        HStack(spacing: 0) {
            if isSelected {
                Rectangle()
                    .fill(Color.accentText)
                    .frame(width: 4)
                    .accessibilityHidden(true)
            }
            content
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

    private var content: some View {
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
            Text(RunDisplay.metaLine(run))
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
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

enum RunDisplay {
    enum SecondaryLine: Equatable {
        case hold(String)
        case milestone(String)
        case supersession(String)
        case completion(String)
    }

    static func sortedRuns(
        _ runs: [Components.Schemas.RunSnapshot]
    ) -> [Components.Schemas.RunSnapshot] {
        runs.sorted { lhs, rhs in
            let lhsActivity = lhs.run.last_activity_at ?? lhs.run.created_at
            let rhsActivity = rhs.run.last_activity_at ?? rhs.run.created_at
            switch (lhsActivity, rhsActivity) {
            case (let lhs?, let rhs?) where lhs != rhs:
                return lhs > rhs
            case (_?, nil):
                return true
            case (nil, _?):
                return false
            default:
                return lhs.run.id < rhs.run.id
            }
        }
    }

    static func round(_ stage: Components.Schemas.Stage) -> String? {
        guard !stage.attempts.isEmpty else { return nil }
        return "Round \(stage.attempts.count)"
    }

    /// The row title: the current stage and its round. A run that has no
    /// stage yet is titled by its project so the row is never blank.
    static func title(_ run: Components.Schemas.Run) -> String {
        guard let stage = run.stages.last else { return projectName(run) }
        var parts = [stageLabel(stage.name)]
        if let round = round(stage) {
            parts.append(round)
        }
        return parts.joined(separator: " · ")
    }

    /// The row's meta line: project, work unit when named, and the start
    /// clock time when the daemon recorded one.
    static func metaLine(_ run: Components.Schemas.Run) -> String {
        var parts = [projectName(run)]
        if let workUnit = run.display_names?.value1.work_unit.text, !workUnit.isEmpty {
            parts.append(workUnit)
        }
        if let created = run.created_at {
            parts.append("started \(created.formatted(date: .omitted, time: .shortened))")
        }
        return parts.joined(separator: " · ")
    }

    /// The daemon's production lane records its implementation stage as
    /// `implement` (daemon/internal/engine/production_workflow.go), so that
    /// name reads as the implementation stage rather than a fifth one.
    static func canonicalStageName(_ name: String) -> String {
        name == "implement" ? Components.Schemas.StageName.implementation.rawValue : name
    }

    /// A workflow stage takes its display label; any other recorded name
    /// is shown capitalized.
    static func stageLabel(_ name: String) -> String {
        Components.Schemas.StageName(rawValue: canonicalStageName(name))
            .map(AttentionDisplay.label) ?? name.capitalized
    }

    /// The timeline title is the run's campaign identity; a run outside a
    /// campaign, or missing its attempt number, is titled by its id.
    static func timelineTitle(_ run: Components.Schemas.Run) -> String {
        campaign(run) ?? run.id
    }

    private static func projectName(_ run: Components.Schemas.Run) -> String {
        let project = run.display_names?.value1.project.text ?? ""
        return project.isEmpty ? run.project_id : project
    }

    /// The four workflow stages in order, then any stage the daemon
    /// recorded under another name. A stage that exists and is not the
    /// last one has been left behind, so it reads completed; the last
    /// existing stage carries the run's outcome; the rest are pending.
    static func stageRail(_ run: Components.Schemas.Run) -> DecisionStageRailPresentation {
        var names = Components.Schemas.StageName.allCases.map {
            (name: $0.rawValue, label: AttentionDisplay.label($0))
        }
        let recorded = run.stages.map { canonicalStageName($0.name) }
        for name in recorded where !names.contains(where: { $0.name == name }) {
            names.append((name: name, label: stageLabel(name)))
        }
        let current = recorded.last
        let entries = names.map { name, label in
            let state: DecisionStageRailPresentation.State =
                if name == current {
                    currentStageState(run.outcome)
                } else if recorded.contains(name) {
                    .completed
                } else {
                    .pending
                }
            return DecisionStageRailPresentation.Entry(id: name, title: label, state: state)
        }
        return .init(
            entries: entries,
            summary: entries.map { "\($0.title) \($0.state.accessibilityLabel)" }
                .joined(separator: ", "))
    }

    /// An unobserved run marks nothing current: the daemon recorded no
    /// milestone, so the rail must not claim a stage is under way.
    private static func currentStageState(
        _ outcome: Components.Schemas.RunOutcome
    ) -> DecisionStageRailPresentation.State {
        switch outcome {
        case .failed, .lost: .failed
        case .completed, .published: .completed
        case .pending, .blocked: .current
        case .unobserved: .pending
        }
    }

    static func identityLine(
        _ run: Components.Schemas.Run, runs: [Components.Schemas.RunSnapshot]
    ) -> String? {
        guard run.campaign_id != nil, let attempt = run.attempt_number else { return nil }
        let identity = "Attempt \(attempt)"
        if let successor = successorLabel(run, runs: runs) {
            return "\(identity) · superseded by \(successor)"
        }
        return identity
    }

    private static func successorLabel(
        _ run: Components.Schemas.Run, runs: [Components.Schemas.RunSnapshot]
    ) -> String? {
        guard let successorID = run.superseded_by else { return nil }
        if let attempt = runs.first(where: { $0.run.id == successorID })?.run.attempt_number {
            return "attempt \(attempt)"
        }
        return successorID
    }

    static func spendLine(_ run: Components.Schemas.Run) -> String? {
        run.billable_cost_so_far.map { AttentionDisplay.costSoFar($0.value1) }
    }

    static func secondaryLine(
        _ run: Components.Schemas.Run, runs: [Components.Schemas.RunSnapshot] = []
    ) -> SecondaryLine {
        if let successor = successorLabel(run, runs: runs) {
            return .supersession("Superseded by \(successor)")
        }
        if let completion = run.completion?.value1 {
            return .completion("Merged PR #\(completion.pr_number)")
        }
        if let hold = run.hold_reason?.value1 {
            return .hold(label(hold))
        }
        if let milestone = run.latest_milestone?.value1 {
            return .milestone(label(milestone))
        }
        return .milestone("No milestone recorded")
    }

    static func campaign(_ run: Components.Schemas.Run) -> String? {
        guard let campaignID = run.campaign_id, let attempt = run.attempt_number else { return nil }
        return "Campaign \(campaignID) · Attempt \(attempt)"
    }

    static func specificationLabel(_ run: Components.Schemas.Run) -> String {
        run.stages.contains { $0.name == "implement" } ? "Approved specification" : "Source specification"
    }

    static func label(_ value: Components.Schemas.RunOutcome) -> String {
        switch value {
        case .unobserved: "Not observed"
        case .pending: "In progress"
        case .published: "Ready"
        case .completed: "Merged"
        case .blocked: "Blocked"
        case .failed: "Failed"
        case .lost: "Lost"
        }
    }

    static func label(_ value: Components.Schemas.RunMilestoneKind) -> String {
        value.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
    }

    static func label(_ value: Components.Schemas.RunHoldReason) -> String {
        value.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
    }

    static func label(_ value: Components.Schemas.ScheduleKind) -> String {
        switch value {
        case .pr_checks_deadline: "Checks"
        case .review_wait_threshold: "Review"
        case .base_advance_watch: "Base watch"
        case .installation_poll: "Install watch"
        case .doctor: "Doctor"
        case .janitor: "Janitor"
        }
    }
}
