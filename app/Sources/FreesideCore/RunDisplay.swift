import Foundation
import FreesideAPI

enum RunDisplay {
    enum SecondaryLine: Equatable {
        case hold(String)
        case milestone(String)
        case supersession(String)
        case completion(String)
    }

    /// Newest activity first, falling back to submission, with undated runs
    /// last and the id as the tie-breaker. Ordering by last activity lets an
    /// operator find a run relative to the last thing that happened to it,
    /// which is what the list is scanned for; the cost is that a late
    /// observation can lift a finished run above later-started ones, so the
    /// row shows the instant that decided its place.
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

    struct WorkflowPhase {
        let stage: Components.Schemas.StageName
        let round: String?
        let completed: Set<Components.Schemas.StageName>
    }

    /// Production runs append implementation passes without recording review
    /// or verification stages. Publication progress supplies that position;
    /// Review is completed by position, never shown as a live review round.
    static func workflowPhase(_ run: Components.Schemas.Run) -> WorkflowPhase? {
        guard let stage = run.stages.last, canonicalStageName(stage.name) == "implementation" else { return nil }
        let milestone = run.latest_milestone?.value1
        let completed = run.outcome == .completed || milestone == .work_unit_completed
        let published = run.outcome == .published || milestone == .publication_ready
        // Publication-cycle holds: daemon/internal/domain/observation.go.
        let publicationHolds: Set<Components.Schemas.RunHoldReason> = [
            .verification_findings, .trust_blocked, .base_advanced, .recipe_revoked,
            .scope_conflict, .publication_environment, .external_conflict,
        ]
        let publicationHold = run.hold_reason.map { publicationHolds.contains($0.value1) } ?? false
        if completed || published || publicationHold || milestone == .publication_blocked || run.outcome == .blocked {
            let passes = run.stages.filter { canonicalStageName($0.name) == "implementation" }.count
            return WorkflowPhase(
                stage: .verification, round: "Round \(passes)",
                completed: completed || published
                    ? [.implementation, .review, .verification] : [.implementation, .review])
        }
        return WorkflowPhase(stage: .implementation, round: round(stage), completed: [])
    }

    /// Shared by the row title and timeline header so their phase and round agree.
    static func stageHeading(_ run: Components.Schemas.Run) -> (label: String, round: String?)? {
        if let phase = workflowPhase(run) {
            return (AttentionDisplay.label(phase.stage), phase.round)
        }
        guard let stage = run.stages.last else { return nil }
        return (stageLabel(stage.name), round(stage))
    }

    /// A run that has no stage yet is titled by its project so the row is never blank.
    static func title(_ run: Components.Schemas.Run) -> String {
        guard let heading = stageHeading(run) else { return projectName(run) }
        return [heading.label, heading.round].compactMap { $0 }.joined(separator: " · ")
    }

    /// The row's meta line: project, work unit when named, and when the run
    /// was last active. The activity instant is the one `sortedRuns` orders
    /// by, so the list's order can be read off the cards. Under a day it
    /// reads as the inbox's coarse relative time; from a day on it carries
    /// the date, so two runs at the same clock time on different days never
    /// read alike. The submission instant stays on the run timeline.
    static func metaLine(_ run: Components.Schemas.Run, now: Date) -> String {
        var parts = [projectName(run)]
        if let workUnit = run.display_names?.value1.task.text, !workUnit.isEmpty {
            parts.append(workUnit)
        }
        if let activity = run.last_activity_at {
            parts.append(lastActiveSegment(activity, now: now))
        }
        return parts.joined(separator: " · ")
    }

    static func lastActiveSegment(_ activity: Date, now: Date) -> String {
        guard now.timeIntervalSince(activity) < 86_400 else {
            return "last active \(activity.formatted(date: .abbreviated, time: .shortened))"
        }
        return "last active \(AttentionDisplay.relativeRowTime(activity, now: now)) ago"
    }

    /// The exact last-activity instant behind the row's coarse segment,
    /// for the macOS hover help.
    static func exactActivityTimestamp(_ run: Components.Schemas.Run) -> String? {
        run.last_activity_at?.formatted(.iso8601)
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

    /// The timeline title is the attempt number; a run outside a
    /// campaign, or missing its attempt number, is titled by its id.
    static func timelineTitle(_ run: Components.Schemas.Run) -> String {
        guard run.campaign_id != nil, let attempt = run.attempt_number else { return run.id }
        return "Attempt \(attempt)"
    }

    private static func projectName(_ run: Components.Schemas.Run) -> String {
        let project = run.display_names?.value1.project.text ?? ""
        return project.isEmpty ? run.project_id : project
    }

    /// The four workflow stages in order, then any stage the daemon
    /// recorded under another name. Production uses its derived workflow
    /// phase; other runs use the last recorded stage. Stages left behind read
    /// completed, the current stage carries outcome and lifecycle, and the rest are pending.
    static func stageRail(_ run: Components.Schemas.Run) -> DecisionStageRailPresentation {
        var names = Components.Schemas.StageName.allCases.map {
            (name: $0.rawValue, label: AttentionDisplay.label($0))
        }
        let recorded = run.stages.map { canonicalStageName($0.name) }
        for name in recorded where !names.contains(where: { $0.name == name }) {
            names.append((name: name, label: stageLabel(name)))
        }
        let phase = workflowPhase(run)
        let current = phase?.stage.rawValue ?? recorded.last
        let completed = phase?.completed.map(\.rawValue) ?? []
        let entries = names.map { name, label in
            let state: DecisionStageRailPresentation.State =
                if completed.contains(name) {
                    .completed
                } else if name == current {
                    currentStageState(run)
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
        _ run: Components.Schemas.Run
    ) -> DecisionStageRailPresentation.State {
        switch run.outcome {
        case .failed, .lost: .failed
        case .completed, .published: .completed
        case .pending:
            isSpecificationHandoff(run) ? .completed : (run.lifecycle == .active ? .current : .pending)
        case .blocked: run.lifecycle == .active ? .current : .pending
        case .unobserved: .pending
        }
    }

    /// The daemon constructs a single specification stage and permits retry
    /// parents only on implementation runs. Classify from the source so a
    /// partially loaded list does not turn a handoff into retry wording.
    private static func isSpecificationHandoff(_ run: Components.Schemas.Run) -> Bool {
        run.campaign_id != nil && run.attempt_number != nil
            && run.lifecycle == .finished && run.superseded_by != nil
            && run.stages.count == 1
            && canonicalStageName(run.stages[0].name) == "specification"
    }

    static func identityLine(
        _ run: Components.Schemas.Run, runs: [Components.Schemas.RunSnapshot]
    ) -> String? {
        guard run.campaign_id != nil, let attempt = run.attempt_number else { return nil }
        let identity = "Attempt \(attempt)"
        if let successor = successorLabel(run, runs: runs) {
            if isSpecificationHandoff(run) {
                return "\(identity) · handed off to implementation (\(successor))"
            }
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
            if isSpecificationHandoff(run) {
                return .supersession("Handed off to implementation (\(successor))")
            }
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

    static func label(_ value: Components.Schemas.ReviewProgressState) -> String {
        value.rawValue.capitalized
    }

    static func reviewIdentity(_ round: Components.Schemas.RunReviewRound) -> String {
        "\(round.provider ?? "Reviewer unknown") · \(round.model_configuration ?? "Model unknown")"
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
