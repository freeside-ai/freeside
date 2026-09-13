import Foundation
import FreesideAPI
import SwiftUI

/// The task presentation vocabulary the task row, the task timeline, and
/// the run timeline's eyebrow share.
enum TaskDisplay {
    /// The row's stage line: the phase heading the run row carried as its
    /// title (#1174), the rail under it, and the current hold.
    struct Position: Equatable {
        struct Heading: Equatable {
            let label: String
            let round: String?

            var text: String {
                [label, round].compactMap { $0 }.joined(separator: " · ")
            }
        }

        let heading: Heading?
        let rail: DecisionStageRailPresentation
        let hold: String?
    }

    /// Newest activity first, the id as the tie-breaker. A task always
    /// carries its last activity, so unlike the run list no row is undated.
    static func sortedTasks(
        _ tasks: [Components.Schemas.TaskSnapshot]
    ) -> [Components.Schemas.TaskSnapshot] {
        tasks.sorted { lhs, rhs in
            if lhs.task.last_activity_at != rhs.task.last_activity_at {
                return lhs.task.last_activity_at > rhs.task.last_activity_at
            }
            return lhs.task.id < rhs.task.id
        }
    }

    /// A task with no lifecycle yet (no run) is work that exists and has
    /// not finished, so it counts as active.
    static func isActive(_ task: Components.Schemas.Task) -> Bool {
        task.lifecycle != .finished
    }

    /// The stage line derives from the newest run when the list holds it,
    /// so the row and that run's timeline agree on phase and round
    /// (`RunDisplay.workflowPhase`). When the run is not listed, the
    /// daemon's own position supplies the stage, round, and hold; the rail
    /// then marks that stage current and the rest pending, because the
    /// position records nothing about which stages ran. A task with no
    /// position has no stage line.
    static func position(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot]
    ) -> Position? {
        guard let position = task.current_position?.value1 else { return nil }
        if let run = runs.first(where: { $0.run.id == position.run_id })?.run {
            return Position(
                heading: RunDisplay.stageHeading(run).map { .init(label: $0.label, round: $0.round) },
                rail: RunDisplay.stageRail(run),
                hold: run.hold_reason.map { RunDisplay.label($0.value1) })
        }
        let stage = position.stage.map(RunDisplay.canonicalStageName)
        var names = Components.Schemas.StageName.allCases.map {
            (name: $0.rawValue, label: AttentionDisplay.label($0))
        }
        if let stage, !names.contains(where: { $0.name == stage }) {
            names.append((name: stage, label: RunDisplay.stageLabel(stage)))
        }
        let entries = names.map { name, label in
            DecisionStageRailPresentation.Entry(
                id: name, title: label, state: name == stage ? .current : .pending)
        }
        return Position(
            heading: stage.map {
                .init(label: RunDisplay.stageLabel($0), round: position.round.map { "Round \($0)" })
            },
            rail: .init(
                entries: entries,
                summary: entries.map { "\($0.title) \($0.state.accessibilityLabel)" }
                    .joined(separator: ", ")),
            hold: position.hold_reason.map { RunDisplay.label($0.value1) })
    }

    /// The task name a run's timeline shows: the task snapshot's current
    /// name when the coordinator lists the task, else the name the run
    /// itself carries, else the task id as an identifier fallback, so a run
    /// reached before its task loaded still names its work.
    static func name(
        for run: Components.Schemas.Run, tasks: [Components.Schemas.TaskSnapshot]
    ) -> Components.Schemas.DisplayName {
        tasks.first { $0.task.id == run.task_id }?.task.display_names.task
            ?? run.display_names?.value1.task
            ?? .init(text: run.task_id, source: .identifier)
    }

    /// The armed watches and deadlines attached to any of the task's runs
    /// (plan §11: the tasks list shows attached watches and deadlines). A
    /// schedule belongs to a run, so the task shows the union over its runs.
    static func armedSchedules(
        for task: Components.Schemas.Task, in schedules: [Components.Schemas.ScheduleSnapshot]
    ) -> [Components.Schemas.ScheduleSnapshot] {
        schedules.filter { snapshot in
            snapshot.schedule.status == .armed
                && snapshot.schedule.run_id.map(task.run_ids.contains) == true
        }
    }

    static func projectName(_ task: Components.Schemas.Task) -> String {
        let project = task.display_names.project.text
        return project.isEmpty ? task.project_id : project
    }

    /// The intake issue behind the task, when its source is an issue.
    static func issueReference(_ task: Components.Schemas.Task) -> String? {
        guard case .issue_subject(let source)? = task.source?.value1 else { return nil }
        return "#\(source.issue_subject.issue_number)"
    }

    /// The row's meta line: project, the issue when the source names one,
    /// and when the task was last active, in the run row's time grammar
    /// (coarse under a day, dated from a day on).
    static func metaLine(_ task: Components.Schemas.Task, now: Date) -> String {
        var parts = [projectName(task)]
        if let issue = issueReference(task) {
            parts.append(issue)
        }
        parts.append(RunDisplay.lastActiveSegment(task.last_activity_at, now: now))
        return parts.joined(separator: " · ")
    }

    /// The exact last-activity instant behind the row's coarse segment,
    /// for the macOS hover help.
    static func exactActivityTimestamp(_ task: Components.Schemas.Task) -> String {
        task.last_activity_at.formatted(.iso8601)
    }

    /// The timeline header's source line. A null source is legacy or demo
    /// work without a recoverable intake reference (the Task contract).
    static func sourceLine(_ task: Components.Schemas.Task) -> String {
        switch task.source?.value1 {
        case .issue_subject(let source)?:
            "Source: \(source.issue_subject.repo)#\(source.issue_subject.issue_number)"
        case .work_item_artifact(let artifact)?:
            "Source: artifact \(artifact.work_item_artifact_id)"
        case nil:
            "Source: none recorded"
        }
    }
}

/// A task's name as the daemon labels it: a chosen name in the given face,
/// an identifier fallback in the mono face the inbox context line uses, and
/// an agent-proposed name followed by the "Agent" keyword, so agent prose
/// stays a labeled claim (plan §9) wherever the name appears. The keyword is
/// part of the same text run, so a name that wraps carries its mark after
/// its last word rather than beside its first line.
struct TaskNameLabel: View {
    let name: Components.Schemas.DisplayName
    var font: Font = FreesideFont.itemTitle
    var monoFont: Font = FreesideFont.monoCallout
    var color: Color = .ink
    var lineLimit: Int? = 2

    var body: some View {
        var text = Text(name.text)
            .font(name.source == .identifier ? monoFont : font)
            .foregroundStyle(color)
        if name.source == .agent {
            text =
                text
                + Text("  AGENT")
                .font(FreesideFont.keyword)
                .tracking(0.8)
                .foregroundStyle(Color.inkDim)
        }
        return
            text
            .lineLimit(lineLimit)
            .truncationMode(.middle)
    }
}
