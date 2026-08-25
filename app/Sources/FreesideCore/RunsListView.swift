import FreesideAPI
import SwiftUI

struct RunsListView: View {
    let runs: [Components.Schemas.RunSnapshot]
    let schedules: [Components.Schemas.ScheduleSnapshot]
    @Binding var selection: String?
    @State private var projectID: String?

    private var projects: [String] {
        Array(Set(runs.map(\.run.project_id))).sorted()
    }

    private var visibleRuns: [Components.Schemas.RunSnapshot] {
        RunDisplay.sortedRuns(
            runs.filter { projectID == nil || $0.run.project_id == projectID })
    }

    var body: some View {
        VStack(spacing: 0) {
            Picker("Project", selection: $projectID) {
                Text("All projects").tag(String?.none)
                ForEach(projects, id: \.self) { project in
                    Text(project).tag(String?.some(project))
                }
            }
            .pickerStyle(.menu)
            .padding(.horizontal)
            .padding(.bottom, 8)

            if visibleRuns.isEmpty {
                ContentUnavailableView {
                    Label {
                        Text("No runs").font(FreesideFont.title)
                    } icon: {
                        Image(systemName: "point.3.connected.trianglepath.dotted")
                    }
                } description: {
                    Text("Runs for this project will appear here.").font(FreesideFont.callout)
                }
                .foregroundStyle(Color.inkDim)
            } else {
                List(visibleRuns, id: \.run.id, selection: $selection) { snapshot in
                    RunRowView(
                        run: snapshot.run,
                        schedules: schedules.filter {
                            $0.schedule.run_id == snapshot.run.id && $0.schedule.status == .armed
                        },
                        isSelected: selection == snapshot.run.id
                    )
                    .listRowInsets(EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12))
                    .listRowSeparator(.hidden)
                    .listRowBackground(Color.clear)
                }
                .listStyle(.plain)
                .scrollContentBackground(.hidden)
            }
        }
        .navigationTitle("Runs")
        .onChange(of: projectID) {
            repairFilterAndSelection()
        }
        .onChange(of: projects) {
            repairFilterAndSelection()
        }
        .onChange(of: runs.map(\.run.id)) {
            repairFilterAndSelection()
        }
    }

    private func repairFilterAndSelection() {
        if let projectID, !projects.contains(projectID) {
            self.projectID = nil
        }
        let effectiveProject = projectID.flatMap { projects.contains($0) ? $0 : nil }
        if let selection,
            !runs.contains(where: {
                $0.run.id == selection
                    && (effectiveProject == nil || $0.run.project_id == effectiveProject)
            })
        {
            self.selection = nil
        }
    }

    /// The project-owned row composition without List and Picker, whose
    /// AppKit-backed controls ImageRenderer cannot draw off-screen.
    @ViewBuilder
    func screenshotContent() -> some View {
        VStack(spacing: 8) {
            ForEach(Array(visibleRuns.prefix(5)), id: \.run.id) { snapshot in
                RunRowView(
                    run: snapshot.run,
                    schedules: schedules.filter {
                        $0.schedule.run_id == snapshot.run.id && $0.schedule.status == .armed
                    },
                    isSelected: selection == snapshot.run.id
                )
            }
        }
        .padding()
    }
}

/// One run as a ground-2 card; the selected row's border turns
/// accent-dim in place of the platform selection highlight.
private struct RunRowView: View {
    let run: Components.Schemas.Run
    let schedules: [Components.Schemas.ScheduleSnapshot]
    var isSelected = false

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(alignment: .firstTextBaseline) {
                Text(run.project_id)
                    .font(FreesideFont.itemTitle)
                    .foregroundStyle(Color.ink)
                Spacer()
                RunOutcomeBadge(outcome: run.outcome)
            }
            Text(run.id)
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
            if let campaign = RunDisplay.campaign(run) {
                Text(campaign)
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }
            HStack(spacing: 8) {
                if let stage = run.stages.last {
                    Label(stage.name.capitalized, systemImage: "square.stack.3d.up")
                    if let round = RunDisplay.round(stage) {
                        Text(round)
                    }
                }
                if let milestone = run.latest_milestone?.value1 {
                    Text(RunDisplay.label(milestone))
                }
                if let hold = run.hold_reason?.value1 {
                    // A hold is attention; on a failed or lost run it
                    // reads as part of the failure and keeps wax.
                    Label(RunDisplay.label(hold), systemImage: "pause.circle.fill")
                        .foregroundStyle(holdIsFailure ? Color.waxText : Color.accentText)
                }
            }
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            if !schedules.isEmpty {
                HStack(spacing: 6) {
                    ForEach(schedules, id: \.schedule.id) { snapshot in
                        ScheduleBadge(schedule: snapshot.schedule)
                    }
                }
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .freesideCard(border: isSelected ? .accentBorder : .rule)
    }

    private var holdIsFailure: Bool {
        switch run.outcome {
        case .failed, .lost: true
        case .unobserved, .pending, .published, .blocked: false
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
        case .published: .ink
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
