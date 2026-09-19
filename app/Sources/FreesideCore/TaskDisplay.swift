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
        let holdReason: Components.Schemas.RunHoldReason?
        var hold: String? { holdReason.map(RunDisplay.label) }
        var qualification: String? = nil
        var status: String? = nil
        var guidance: String = Position.defaultGuidance
        var historical = false
        /// True iff `guidance` targets a current open Inbox item bound to
        /// this task: the same lookups that produce that guidance set it, so
        /// the accent chip and the Inbox sentence can never disagree.
        var attention = false
        /// True iff `status` names history rather than a state (a superseded
        /// run, an abandoned task). Narrower than `historical`, which also
        /// covers a stopped or finished task whose status the daemon spoke.
        var statusIsHistorical = false

        /// The sentence every row carries for VoiceOver. The visible row
        /// omits it: the row itself is the affordance.
        static let defaultGuidance = "Open task details."

        /// The status chip's cut, from what the operator can do and never
        /// from the status word.
        var cut: StateChip.Cut {
            attention ? .attention : statusIsHistorical ? .faint : .ink
        }
    }

    enum SpecificationApproval {
        case approved, unapproved, unavailable

        var qualification: String? {
            self == .unavailable ? "Specification approval history unavailable" : nil
        }
    }

    /// Approval belongs to the displayed run's campaign, not the task's newest
    /// event or the stage order. A retry shares the initial attempt's approval.
    static func specificationApproval(
        _ task: Components.Schemas.Task, runID: String, run: Components.Schemas.Run?,
        history: Components.Schemas.TaskTimeline?
    ) -> SpecificationApproval {
        guard task.run_ids.contains(runID), let history,
            history.task_id == task.id, history.project_id == task.project_id
        else { return .unavailable }
        let sections = history.sections.filter { $0.runs.contains { $0.run_id == runID } }
        guard sections.count == 1, let section = sections.first,
            let campaign = section.campaign_id, task.campaign_ids.contains(campaign),
            let member = section.runs.first(where: { $0.run_id == runID }), let role = member.role?.value1
        else { return .unavailable }
        if let run {
            guard run.id == runID, run.task_id == task.id, run.project_id == task.project_id,
                run.campaign_id == campaign
            else { return .unavailable }
        }
        let approvals = section.events.filter { $0.kind == .specification_approved }
        if approvals.isEmpty { return role == .specification ? .unapproved : .unavailable }
        guard approvals.count == 1, let event = approvals.first,
            event.campaign_id == campaign, let digest = event.approved_spec_digest?.value1, !digest.isEmpty,
            let specificationID = event.specification_run_id, task.run_ids.contains(specificationID),
            let initialID = event.run_id, task.run_ids.contains(initialID),
            section.runs.contains(where: { $0.run_id == specificationID && $0.role?.value1 == .specification }),
            section.runs.contains(where: {
                $0.run_id == initialID && $0.role?.value1 == .implementation && $0.attempt_number == 1
            })
        else { return .unavailable }
        switch role {
        case .specification:
            return runID == specificationID ? .approved : .unavailable
        case .implementation:
            guard let attempt = member.attempt_number, attempt >= 1,
                (attempt == 1) == (runID == initialID),
                run == nil || run?.spec_digest == digest
            else { return .unavailable }
            return .approved
        }
    }

    private static func approvalRail(
        _ rail: DecisionStageRailPresentation, approval: SpecificationApproval
    ) -> DecisionStageRailPresentation {
        let entries = rail.entries.map { entry in
            guard entry.id == "specification" else { return entry }
            let state: DecisionStageRailPresentation.State =
                approval == .approved ? .completed : (entry.state == .completed ? .pending : entry.state)
            return .init(id: entry.id, title: entry.title, state: state)
        }
        return .init(
            entries: entries,
            summary: entries.map { entry in
                if entry.id == "specification", approval == .unavailable {
                    return entry.state == .pending
                        ? "Specification approval history unavailable"
                        : "Specification \(entry.state.accessibilityLabel), approval history unavailable"
                }
                return "\(entry.title) \(entry.state.accessibilityLabel)"
            }.joined(separator: ", "))
    }

    /// The sorted, unique project ids the synced tasks name: the set the
    /// Tasks filter offers and the same set the New Task composer's project
    /// picker draws from, so a task's home project appears once the client
    /// has synced a task in it. A configured project with no synced task is
    /// not offered until #1332 lists configured projects in the bootstrap.
    static func knownProjects(in tasks: [Components.Schemas.TaskSnapshot]) -> [String] {
        Array(Set(tasks.map(\.task.project_id))).sorted()
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

    /// The ids of tasks whose displayed name text equals another visible
    /// task's, so the row can append a short id only where two rows would
    /// otherwise read alike. An identifier-fallback name is the task id, which
    /// is unique, so those rows never collide.
    static func ambiguousTaskIDs(_ tasks: [Components.Schemas.TaskSnapshot]) -> Set<String> {
        var idsByName: [String: [String]] = [:]
        for snapshot in tasks {
            idsByName[snapshot.task.display_names.task.text, default: []].append(snapshot.task.id)
        }
        return Set(idsByName.values.filter { $0.count > 1 }.flatMap { $0 })
    }

    /// A task with no lifecycle yet (no run) is work that exists and has
    /// not finished, so it counts as active.
    static func isActive(_ task: Components.Schemas.Task) -> Bool {
        switch task.lifecycle {
        case .finished, .stopped, .abandoned: false
        case .active, ._empty, nil: true
        }
    }

    static func lifecycleLabel(_ task: Components.Schemas.Task) -> (text: String, systemImage: String) {
        switch task.lifecycle {
        case .finished: ("Finished", "checkmark.circle")
        case .stopped: ("Stopped", "stop.circle")
        case .abandoned: ("Abandoned", "minus.circle")
        case .active, ._empty, nil: ("Active", "circle.dotted")
        }
    }

    /// The stage line derives from the named current run when the list holds it,
    /// so the row and that run's timeline agree on phase and round
    /// (`RunDisplay.workflowPhase`). When the run is not listed, the
    /// daemon's own position supplies the stage, round, and hold; the rail
    /// then marks that stage current and other stages pending. Recorded
    /// campaign approval independently completes Specification in either
    /// path. A task with no position has no stage line.
    static func position(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot],
        attentionItems: [Components.Schemas.AttentionItemSnapshot] = [],
        history: Components.Schemas.TaskTimeline? = nil
    ) -> Position? {
        guard let current = task.current_position?.value1,
            var position = phasePosition(task, runs: runs, attentionItems: attentionItems, history: history)
        else { return nil }
        let run = runs.first { $0.run.id == current.run_id }?.run
        let status = statusAndTone(task, run: run)
        position.status = status.text
        position.statusIsHistorical = status.isHistorical
        position.historical = !isActive(task) || run?.superseded_by != nil || run?.lifecycle == .finished
        // A current cancellation fence suppresses action guidance even when
        // older snapshots still contain an open approval or published handoff.
        if !suppressesGuidanceForCancellation(task), !position.historical,
            run?.outcome != .failed, run?.outcome != .lost
        {
            if position.holdReason == .identity_parallelism, run == nil || run?.outcome == .pending {
                position.guidance =
                    "The configured agent account is busy. This work is queued; no action is needed for this wait."
            }
            if let handoff = finalReviewHeading(task, run: run, attentionItems: attentionItems, titleCase: true) {
                position.status = handoff
                position.guidance = "Review the pull request from Inbox."
                position.attention = true
            } else if run?.lifecycle != .finished,
                run == nil || (run?.task_id == task.id && run?.project_id == task.project_id),
                specificationApproval(task, runID: current.run_id, run: run, history: history)
                    != .approved,
                task.run_ids.contains(current.run_id),
                attentionItems.contains(where: { snapshot in
                    let item = snapshot.item
                    guard item._type == .spec_approval, item.status == .open,
                        item.project_id == task.project_id, case .run(let subject) = item.subject
                    else { return false }
                    return subject.subject_id == current.run_id && subject.run_id == current.run_id
                        && subject.task_id == task.id
                })
            {
                position.status = "Specification Approval Required"
                position.guidance = "Review the specification in Inbox."
                position.attention = true
            }
        }
        return position
    }

    private static func suppressesGuidanceForCancellation(_ task: Components.Schemas.Task) -> Bool {
        guard let cancellation = task.cancellation?.value1 else { return false }
        // The daemon's active projection means a historical confirmation must
        // not hide attention bound to the current run. This grants no execution.
        return cancellation.state != .confirmed || task.lifecycle != .active
    }

    /// Task lifecycle remains the daemon's projection. A run's terminal
    /// outcome and a cancellation request are separate facts, not success.
    static func rowStatus(_ task: Components.Schemas.Task, run: Components.Schemas.Run? = nil) -> String {
        statusAndTone(task, run: run).text
    }

    /// The status chip's cut for a row with or without a position.
    static func statusCut(_ task: Components.Schemas.Task, position: Position?) -> StateChip.Cut {
        position?.cut ?? (statusAndTone(task, run: nil).isHistorical ? .faint : .ink)
    }

    /// The status and whether it names history rather than a state. One
    /// chain decides both, so the chip's faint cut cannot drift from the
    /// branch that chose the words: an abandoned task and a superseded run
    /// are historical; every other status is the daemon working or having
    /// spoken, a stopped or finished task included.
    private static func statusAndTone(
        _ task: Components.Schemas.Task, run: Components.Schemas.Run?
    ) -> (text: String, isHistorical: Bool) {
        if task.lifecycle == .abandoned { return (lifecycleLabel(task).text, true) }
        if task.lifecycle == .stopped { return (lifecycleLabel(task).text, false) }
        if let cancellation = task.cancellation?.value1 {
            switch cancellation.state {
            case .requested: return ("Stop Requested · Awaiting Confirmation", false)
            case .failed_to_stop: return ("Failed to Stop · Execution May Continue", false)
            case .confirmed: break
            }
        }
        if run?.superseded_by != nil { return ("Superseded Run · Historical", true) }
        return (liveStatus(task, run: run), false)
    }

    private static func liveStatus(_ task: Components.Schemas.Task, run: Components.Schemas.Run?) -> String {
        if run?.outcome == .failed { return "Execution Failed" }
        if run?.outcome == .lost { return "Execution Lost" }
        if task.lifecycle == .finished { return "Finished · See Recorded Outcome" }
        if run?.lifecycle == .finished { return "Run Finished · See Recorded Outcome" }
        let hold = run.map { $0.hold_reason?.value1 } ?? task.current_position?.value1.hold_reason?.value1
        if hold == .identity_parallelism { return "Queued" }
        if hold != nil { return "On Hold" }
        if run?.outcome == .blocked { return "Publication Blocked" }
        if run?.outcome == .unobserved { return "Execution Status Unavailable" }
        if run?.outcome == .published { return "Published · See Review Status" }
        guard task.current_position != nil else { return "No Execution Position Recorded" }
        return run == nil ? "Run Details Unavailable" : "In Progress"
    }

    /// The row's progress strings by the slot the row draws them in. The
    /// row shows `status` as its chip, `phases` as a line, `facts` joined on
    /// one line, and `guidance` only when it says something the row itself
    /// does not; VoiceOver reads all of them, in this order.
    struct RowLines: Equatable {
        let status: String
        let phases: String?
        /// Round, hold, and a recorded stop confirmation, in that order.
        let facts: [String]
        let guidance: String

        var all: [String] { [status] + [phases].compactMap { $0 } + facts + [guidance] }

        enum VisibleGuidance: Equatable {
            /// Names an Inbox destination: drawn in the link style, without
            /// the sentence's full stop.
            case link(String)
            /// Says something the row does not (a capacity wait), neutrally.
            case sentence(String)
        }

        /// What the row draws for `guidance`. The default sentence draws
        /// nothing: the row itself is that affordance.
        func visibleGuidance(attention: Bool) -> VisibleGuidance? {
            if attention {
                return .link(guidance.hasSuffix(".") ? String(guidance.dropLast()) : guidance)
            }
            return guidance == Position.defaultGuidance ? nil : .sentence(guidance)
        }
    }

    /// These exact strings are the row's combined accessibility content, in
    /// this order. Unknown approval is never rendered as pending.
    static func progressLines(_ task: Components.Schemas.Task, position: Position?) -> [String] {
        rowLines(task, position: position).all
    }

    static func rowLines(_ task: Components.Schemas.Task, position: Position?) -> RowLines {
        var phaseLine: String?
        var facts: [String] = []
        if let position {
            let phases = position.rail.entries.map { entry in
                let state =
                    position.historical && entry.state == .current
                    ? "Last Recorded Phase" : entry.state.accessibilityLabel.capitalized
                if entry.id == "specification", position.qualification != nil {
                    return entry.state == .pending
                        ? "Specification Approval History Unavailable"
                        : "Specification \(state), Approval History Unavailable"
                }
                return "\(entry.title) \(state)"
            }
            phaseLine = phases.joined(separator: " · ")
            if let round = position.heading?.round { facts.append(round) }
            if let hold = position.hold, !position.historical {
                let prefix = suppressesGuidanceForCancellation(task) ? "Last recorded hold" : "Hold"
                facts.append("\(prefix): \(hold)")
            }
        }
        if task.cancellation?.value1.state == .confirmed, task.lifecycle == .finished {
            facts.append("Stop Confirmation Recorded")
        }
        return RowLines(
            status: position?.status ?? rowStatus(task), phases: phaseLine, facts: facts,
            guidance: position?.guidance ?? Position.defaultGuidance)
    }

    private static func phasePosition(
        _ task: Components.Schemas.Task, runs: [Components.Schemas.RunSnapshot],
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        history: Components.Schemas.TaskTimeline?
    ) -> Position? {
        guard let position = task.current_position?.value1 else { return nil }
        let run = runs.first(where: { $0.run.id == position.run_id })?.run
        let approval = specificationApproval(task, runID: position.run_id, run: run, history: history)
        let handoff =
            (!suppressesGuidanceForCancellation(task)
            ? finalReviewHeading(task, run: run, attentionItems: attentionItems) : nil)
            .map { Position.Heading(label: $0, round: nil) }
        if let run {
            return Position(
                heading: handoff ?? RunDisplay.stageHeading(run).map { .init(label: $0.label, round: $0.round) },
                rail: approvalRail(RunDisplay.stageRail(run), approval: approval),
                holdReason: run.hold_reason?.value1,
                qualification: approval.qualification)
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
            heading: handoff
                ?? stage.map {
                    .init(label: RunDisplay.stageLabel($0), round: position.round.map { "Round \($0)" })
                },
            rail: approvalRail(
                .init(
                    entries: entries,
                    summary: entries.map { "\($0.title) \($0.state.accessibilityLabel)" }
                        .joined(separator: ", ")), approval: approval),
            holdReason: position.hold_reason?.value1,
            qualification: approval.qualification)
    }

    /// A current daemon handoff overrides the live phase, without changing
    /// lifecycle or stage history. Cached snapshots use this same projection
    /// under the existing freshness banner; this never certifies their age.
    static func finalReviewHeading(
        _ task: Components.Schemas.Task, run: Components.Schemas.Run?,
        attentionItems: [Components.Schemas.AttentionItemSnapshot], titleCase: Bool = false
    ) -> String? {
        guard task.lifecycle == .active,
            let position = task.current_position?.value1,
            task.run_ids.contains(position.run_id), position.hold_reason == nil
        else { return nil }
        if let run {
            guard run.id == position.run_id, run.task_id == task.id, run.project_id == task.project_id,
                run.lifecycle == .active, run.outcome == .published,
                run.hold_reason == nil, run.superseded_by == nil
            else { return nil }
        }
        let item = attentionItems.first { snapshot in
            let item = snapshot.item
            guard item._type == .ready_for_final_review, item.status == .open,
                item.project_id == task.project_id,
                case .run(let subject) = item.subject,
                subject.subject_id == position.run_id, subject.run_id == position.run_id,
                subject.task_id == task.id,
                let pr = item.pr_reference?.value1, !pr.repo.isEmpty, pr.number > 0,
                !item.pr_head_sha.isEmpty,
                let readiness = item.readiness?.value1,
                let detail = item.readiness_detail?.value1,
                detail.candidate_head == item.pr_head_sha,
                detail.evaluation_set_digest == readiness.evaluation_set_digest,
                item.readiness_invalidation == nil, item.base_freshness?.value1.advanced != true
            else { return false }
            return true
        }
        return item.map {
            if titleCase {
                return $0.item.readiness?.value1._class == .ready_degraded
                    ? "Ready for Final Review (Degraded)" : "Ready for Final Review"
            }
            return AttentionDisplay.title($0.item)
        }
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
    ///
    /// `labelsActivity` false is the visible row: the time alone, in the one
    /// short format from a day on. True is the sentence VoiceOver keeps.
    static func metaLine(_ task: Components.Schemas.Task, now: Date, labelsActivity: Bool = true) -> String {
        var parts = [projectName(task)]
        if let issue = issueReference(task) {
            parts.append(issue)
        }
        if labelsActivity {
            parts.append(RunDisplay.lastActiveSegment(task.last_activity_at, now: now))
        } else if now.timeIntervalSince(task.last_activity_at) < 86_400 {
            parts.append("\(AttentionDisplay.relativeRowTime(task.last_activity_at, now: now)) ago")
        } else {
            parts.append(FreesideFormat.shortTime(task.last_activity_at, now: now))
        }
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
