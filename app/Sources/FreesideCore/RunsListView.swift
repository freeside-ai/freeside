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
        runs.filter { projectID == nil || $0.run.project_id == projectID }
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
                ContentUnavailableView(
                    "No runs", systemImage: "point.3.connected.trianglepath.dotted",
                    description: Text("Runs for this project will appear here."))
            } else {
                List(visibleRuns, id: \.run.id, selection: $selection) { snapshot in
                    RunRowView(
                        run: snapshot.run,
                        schedules: schedules.filter {
                            $0.schedule.run_id == snapshot.run.id && $0.schedule.status == .armed
                        })
                }
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
}

private struct RunRowView: View {
    let run: Components.Schemas.Run
    let schedules: [Components.Schemas.ScheduleSnapshot]

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(alignment: .firstTextBaseline) {
                Text(run.project_id)
                    .font(.headline)
                Spacer()
                RunOutcomeBadge(outcome: run.outcome)
            }
            Text(run.id)
                .font(.caption.monospaced())
                .foregroundStyle(.secondary)
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
                    Label(RunDisplay.label(hold), systemImage: "pause.circle.fill")
                        .foregroundStyle(.orange)
                }
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            if !schedules.isEmpty {
                HStack(spacing: 6) {
                    ForEach(schedules, id: \.schedule.id) { snapshot in
                        ScheduleBadge(schedule: snapshot.schedule)
                    }
                }
            }
        }
        .padding(.vertical, 3)
    }
}

struct RunOutcomeBadge: View {
    let outcome: Components.Schemas.RunOutcome

    var body: some View {
        Text(RunDisplay.label(outcome))
            .font(.caption2.weight(.semibold))
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .foregroundStyle(color)
            .background(color.opacity(0.14), in: Capsule())
    }

    private var color: Color {
        switch outcome {
        case .unobserved: .secondary
        case .pending: .blue
        case .published: .green
        case .blocked: .orange
        case .failed, .lost: .red
        }
    }
}

private struct ScheduleBadge: View {
    let schedule: Components.Schemas.Schedule

    var body: some View {
        Label {
            if let fireAt = schedule.fire_at {
                Text("\(RunDisplay.label(schedule.kind)) \(fireAt.formatted(date: .omitted, time: .shortened))")
            } else {
                Text(RunDisplay.label(schedule.kind))
            }
        } icon: {
            Image(systemName: schedule.fire_at == nil ? "eye" : "clock")
        }
        .font(.caption2)
        .padding(.horizontal, 6)
        .padding(.vertical, 3)
        .background(.quaternary, in: Capsule())
    }
}

enum RunDisplay {
    static func round(_ stage: Components.Schemas.Stage) -> String? {
        guard !stage.attempts.isEmpty else { return nil }
        return "Round \(stage.attempts.count)"
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
