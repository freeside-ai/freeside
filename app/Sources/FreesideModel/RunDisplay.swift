import Foundation
import FreesideAPI

package enum RunDisplay {
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

    /// The stage's round: its attempt count. Specification has none, since
    /// its attempts are invocations (a research continuation, a Discuss
    /// reply, a revision, a retry) and the client can't tell them apart, so
    /// a count would read a reply as a new round (#1462).
    static func round(_ stage: Components.Schemas.Stage) -> String? {
        guard !stage.attempts.isEmpty, canonicalStageName(stage.name) != "specification" else { return nil }
        return "Round \(stage.attempts.count)"
    }

    package struct WorkflowPhase {
        package let stage: Components.Schemas.StageName
        let round: String?
        let completed: Set<Components.Schemas.StageName>
    }

    /// Production runs append implementation passes without recording review
    /// or verification stages. Publication progress supplies that position;
    /// Review is completed by position, never shown as a live review round.
    /// The holds that place a run in Verification: the publication cycle's
    /// (daemon/internal/domain/observation.go).
    package static let publicationHoldReasons: Set<Components.Schemas.RunHoldReason> = [
        .verification_findings, .trust_blocked, .base_advanced, .recipe_revoked,
        .scope_conflict, .publication_environment, .external_conflict, .verification_memory_limit,
    ]

    package static func workflowPhase(_ run: Components.Schemas.Run) -> WorkflowPhase? {
        guard let stage = run.stages.last, canonicalStageName(stage.name) == "implementation" else { return nil }
        let milestone = run.latest_milestone?.value1
        let completed = run.outcome == .completed || milestone == .work_unit_completed
        let published = run.outcome == .published || milestone == .publication_ready
        let publicationHold = run.hold_reason.map { publicationHoldReasons.contains($0.value1) } ?? false
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
    package static func stageHeading(
        _ run: Components.Schemas.Run, task: Components.Schemas.Task? = nil,
        attentionItems: [Components.Schemas.AttentionItemSnapshot] = []
    ) -> (label: String, round: String?)? {
        if let task,
            let dispute = TaskDisplay.reviewDisputeHeading(task, run: run, attentionItems: attentionItems)
        {
            return (dispute, nil)
        }
        if let task,
            let handoff = TaskDisplay.finalReviewHeading(task, run: run, attentionItems: attentionItems)
        {
            return (handoff, nil)
        }
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
    package static func canonicalStageName(_ name: String) -> String {
        name == "implement" ? Components.Schemas.StageName.implementation.rawValue : name
    }

    /// A workflow stage takes its display label; any other recorded name
    /// is shown capitalized.
    package static func stageLabel(_ name: String) -> String {
        Components.Schemas.StageName(rawValue: canonicalStageName(name))
            .map(AttentionDisplay.label) ?? name.capitalized
    }

    /// The timeline title names the run's phase, as the task page's run
    /// card does: `Specification` for a run whose only stage is
    /// specification, `Implementation · Attempt N` for any other campaign
    /// run that has a stage. `Run` carries no role, so a campaign run with
    /// no stage yet keeps `Attempt N`. A run outside a campaign, or missing
    /// its attempt number, is titled by its short id. The exact id is in
    /// the header's technical details.
    package static func timelineTitle(_ run: Components.Schemas.Run) -> String {
        guard run.campaign_id != nil, let attempt = run.attempt_number else {
            return "Run \(ShortIdentifier.short(run.id))"
        }
        if isSpecificationOnly(run) { return AttentionDisplay.label(.specification) }
        if run.stages.isEmpty { return "Attempt \(attempt)" }
        return "\(AttentionDisplay.label(.implementation)) · Attempt \(attempt)"
    }

    /// The daemon builds a specification run with a single specification
    /// stage, and an implementation run never records one.
    private static func isSpecificationOnly(_ run: Components.Schemas.Run) -> Bool {
        run.stages.count == 1 && canonicalStageName(run.stages[0].name) == "specification"
    }

    private static func projectName(_ run: Components.Schemas.Run) -> String {
        let project = run.display_names?.value1.project.text ?? ""
        return project.isEmpty ? run.project_id : project
    }

    /// The four workflow stages in order, then any stage the daemon
    /// recorded under another name. Production uses its derived workflow
    /// phase; other runs use the last recorded stage. Stages left behind read
    /// completed, the current stage carries outcome and lifecycle, and the rest are pending.
    /// A `stage` the task names over the run's own phase (`TaskDisplay.CurrentStage`)
    /// is current instead, with the stages before it completed, so the
    /// spoken rail says what the heading does.
    static func stageRail(
        _ run: Components.Schemas.Run, stage named: Components.Schemas.StageName? = nil
    ) -> DecisionStageRailPresentation {
        var names = Components.Schemas.StageName.allCases.map {
            (name: $0.rawValue, label: AttentionDisplay.label($0))
        }
        let recorded = run.stages.map { canonicalStageName($0.name) }
        for name in recorded where !names.contains(where: { $0.name == name }) {
            names.append((name: name, label: stageLabel(name)))
        }
        let phase = workflowPhase(run)
        let override = named.flatMap { $0 == phase?.stage ? nil : $0 }
        let current = override?.rawValue ?? phase?.stage.rawValue ?? recorded.last
        let completed =
            override.map { stage in
                Components.Schemas.StageName.allCases.prefix { $0 != stage }.map(\.rawValue)
            } ?? phase?.completed.map(\.rawValue) ?? []
        let entries = names.map { name, label in
            let state: DecisionStageRailPresentation.State =
                if completed.contains(name) {
                    .completed
                } else if name == current {
                    // A run that finished its own phase is under way again
                    // in the stage the task names.
                    override != nil && currentStageState(run) == .completed ? .current : currentStageState(run)
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
    /// milestone, so the rail must not claim a stage is under way. A stopped
    /// or abandoned run marks nothing current either: its stage neither
    /// finished nor failed. A handed-off specification run finished its stage.
    private static func currentStageState(
        _ run: Components.Schemas.Run
    ) -> DecisionStageRailPresentation.State {
        switch run.outcome {
        case .failed, .lost: .failed
        case .completed, .published, .handed_off: .completed
        case .pending:
            isSpecificationHandoff(run) ? .completed : (run.lifecycle == .active ? .current : .pending)
        case .blocked: run.lifecycle == .active ? .current : .pending
        case .unobserved, .stopped, .abandoned: .pending
        }
    }

    /// The daemon constructs a single specification stage and permits retry
    /// parents only on implementation runs. Classify from the source so a
    /// partially loaded list does not turn a handoff into retry wording.
    private static func isSpecificationHandoff(_ run: Components.Schemas.Run) -> Bool {
        run.campaign_id != nil && run.attempt_number != nil
            && run.lifecycle == .finished && run.superseded_by != nil
            && isSpecificationOnly(run)
    }

    static func identityLine(
        _ run: Components.Schemas.Run, runs: [Components.Schemas.RunSnapshot]
    ) -> String? {
        guard run.campaign_id != nil, let attempt = run.attempt_number else { return nil }
        let identity = "Attempt \(attempt)"
        if let successor = successorLabel(run, runs: runs) {
            if isSpecificationHandoff(run) {
                return "\(AttentionDisplay.label(.specification)) · handed off to implementation (\(successor))"
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

    package static func spendLine(_ run: Components.Schemas.Run) -> String? {
        run.billable_cost_so_far.map { AttentionDisplay.costSoFar($0.value1) }
    }

    /// The value a row shows for a fact the admission did not record. It is
    /// never filled from current configuration (#979).
    package static let unavailable = "Unavailable"

    /// One line naming what ran an invocation: harness, model, and effort,
    /// skipping any the admission did not record. Nil when it recorded none.
    package static func admissionLine(_ facts: Components.Schemas.InvocationAdmissionFacts) -> String? {
        let parts = [
            facts.harness.map { label($0.value1) },
            facts.model,
            facts.requested_effort.map { "effort \(label($0.value1).lowercased())" },
        ].compactMap { $0 }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }

    /// The admission's facts as labeled rows, with each value the admission
    /// did not record shown as unavailable: the profile, the operating mode,
    /// and, for a review, one independence row per writing role.
    package static func admissionRows(
        _ facts: Components.Schemas.InvocationAdmissionFacts
    ) -> [AttentionDisplay.BindingRow] {
        profileRows(facts)
            + [row("Operating Mode", facts.operating_mode.map { label($0.value1) })]
            + (facts.independence ?? []).map { row("Independence", independence($0)) }
    }

    /// What ran an invocation, who pays for it, and how it was contained.
    package static func profileRows(
        _ facts: Components.Schemas.InvocationAdmissionFacts
    ) -> [AttentionDisplay.BindingRow] {
        [
            row("Harness", facts.harness.map { label($0.value1) }),
            row("Model", facts.model),
            row("Effort", effort(facts)),
            row("Enrollment", facts.enrollment_id),
            row("Selection", facts.selection_digest.map { ShortIdentifier.short($0.value1) }),
            row("Cost Owner", facts.cost_owner),
            row("Credential Mode", facts.credential_mode.map { label($0.value1) }),
            row("Egress Profile", facts.egress_profile.map { label($0.value1) }),
            row("Isolation Class", facts.isolation_class.map(isolationLabel)),
        ]
    }

    private static func row(_ label: String, _ value: String?) -> AttentionDisplay.BindingRow {
        .init(label: label, value: value ?? unavailable)
    }

    private static func independence(_ entry: Components.Schemas.IndependenceEntry) -> String {
        let writer = label(entry.writing_role).lowercased()
        return switch entry.lineage {
        case .matched: "Matched \(writer)"
        case .differed: "Differed from \(writer)"
        case .unknown: "Unknown against \(writer)"
        }
    }

    /// The requested effort, with the harness's own name for it when the two
    /// differ, so a clamp (max passed as xhigh) is shown, never hidden.
    private static func effort(_ facts: Components.Schemas.InvocationAdmissionFacts) -> String? {
        guard let requested = facts.requested_effort?.value1 else { return facts.native_effort }
        let name = label(requested)
        guard let native = facts.native_effort, native != requested.rawValue else { return name }
        return "\(name) (passed as \(native))"
    }

    /// The admission of one invocation of the run, nil when the run's
    /// timeline records none for it.
    package static func admission(
        for invocationID: String, in timeline: Components.Schemas.RunTimeline
    ) -> Components.Schemas.InvocationAdmissionFacts? {
        timeline.admissions.last { $0.invocation_id == invocationID }
    }

    /// The latest execution admission for `role` among a run's admissions:
    /// the one the task page's Current Run reads, skipping reviews and the
    /// run's other roles.
    package static func latestExecutionAdmission(
        _ timeline: Components.Schemas.RunTimeline, role: Components.Schemas.RoleName
    ) -> Components.Schemas.InvocationAdmissionFacts? {
        timeline.admissions.last { $0.kind == .execution && $0.role?.value1 == role }
    }

    /// The admitting backend in words. The backend is an open string on the
    /// wire; a class this client does not know shows its daemon name, which
    /// is what `freesided doctor` prints.
    package static func isolationLabel(_ backend: String) -> String {
        switch backend {
        case "fresh_vm_read_only_volume_handoff": "Fresh VM, read-only volume handoff"
        default: backend
        }
    }

    package static func label(_ value: Components.Schemas.HarnessClientKind) -> String {
        switch value {
        case .claude_code: "Claude Code"
        case .codex_cli: "Codex CLI"
        }
    }

    package static func label(_ value: Components.Schemas.EffortLevel) -> String {
        switch value {
        case .harness_default: "Harness default"
        case .low: "Low"
        case .medium: "Medium"
        case .high: "High"
        case .max: "Max"
        }
    }

    package static func label(_ value: Components.Schemas.CredentialMode) -> String {
        switch value {
        case .subscription_contained: "Subscription, contained"
        case .api_key_isolated: "API key, isolated"
        case .local_trusted: "Local, trusted"
        }
    }

    package static func label(_ value: Components.Schemas.EgressProfile) -> String {
        switch value {
        case .provider_only: "Provider only"
        case .provider_registry: "Provider and package registries"
        case .provider_web_read: "Provider and web read"
        case .clean_verification: "Clean verification"
        }
    }

    package static func label(_ value: Components.Schemas.OperatingMode) -> String {
        switch value {
        case .attended_dev: "Attended"
        case .unattended: "Unattended"
        }
    }

    package static func label(_ value: Components.Schemas.RoleName) -> String {
        value.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
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
            return .hold(run.lifecycle == .finished ? "Recorded hold: \(label(hold))" : label(hold))
        }
        if let milestone = run.latest_milestone?.value1 {
            return .milestone(label(milestone))
        }
        return .milestone("No milestone recorded")
    }

    /// What a run's specification digest is, which the header says as a
    /// sentence and the Identifiers section says as a row label.
    private enum SpecificationStanding {
        case source
        case approved
        case unconfirmed
    }

    private static func specificationStanding(
        _ run: Components.Schemas.Run, approval: TaskDisplay.SpecificationApproval
    ) -> SpecificationStanding {
        if run.stages.contains(where: { canonicalStageName($0.name) == "specification" }) {
            return .source
        }
        switch approval {
        case .approved: return .approved
        case .unapproved: return .source
        case .unavailable: return .unconfirmed
        }
    }

    static func specificationLabel(_ run: Components.Schemas.Run, approval: TaskDisplay.SpecificationApproval) -> String
    {
        switch specificationStanding(run, approval: approval) {
        case .source: return "Source specification"
        case .approved: return "Approved specification"
        case .unconfirmed: return "Specification digest"
        }
    }

    /// The Identifiers row that carries the digest. A row label is
    /// Title Case (R9); the header's line is prose and keeps its own case.
    package static func specificationRowLabel(
        _ run: Components.Schemas.Run, approval: TaskDisplay.SpecificationApproval
    ) -> String {
        switch specificationStanding(run, approval: approval) {
        case .source: return "Source Specification"
        case .approved: return "Approved Specification"
        case .unconfirmed: return "Specification Digest"
        }
    }

    /// The run header's plain specification line, without the digest that moved
    /// to technical details. A source or approved run names itself; an
    /// unavailable approval says so rather than labelling a digest that is no
    /// longer beside it. `specificationRowLabel` names the technical-details
    /// row that still carries the digest.
    package static func specificationHeaderLabel(
        _ run: Components.Schemas.Run, approval: TaskDisplay.SpecificationApproval
    ) -> String {
        let label = specificationLabel(run, approval: approval)
        return label == "Specification digest" ? "Specification approval not confirmed" : label
    }

    package static func label(_ value: Components.Schemas.RunOutcome) -> String {
        switch value {
        case .unobserved: "Not Observed"
        case .pending: "In Progress"
        case .published: "Ready"
        case .completed: "Merged"
        case .blocked: "Blocked"
        case .failed: "Failed"
        case .lost: "Lost"
        case .handed_off: "Handed Off"
        case .stopped: "Stopped"
        case .abandoned: "Abandoned"
        }
    }

    package static func label(_ value: Components.Schemas.RunMilestoneKind) -> String {
        switch value {
        // The milestone says one agent invocation ended, whatever its result.
        // The raw kind read as "Terminal Recorded", as if the task were done.
        case .terminal_recorded: "Invocation Ended"
        default: value.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }

    package static func label(_ value: Components.Schemas.ReviewProgressState) -> String {
        value.rawValue.capitalized
    }

    package static func reviewIdentity(_ round: Components.Schemas.RunReviewRound) -> String {
        "\(round.provider ?? "Reviewer unknown") · \(round.model_configuration ?? "Model unknown")"
    }

    package static func label(_ value: Components.Schemas.RunHoldReason) -> String {
        switch value {
        case .scope_conflict: "Required work outside approved scope"
        case .operation_stopped: "Unattended operation stopped"
        case .blocking_system_health: "System issue preventing work"
        case .input_unavailable: "Required input unavailable"
        case .backend_not_conformant: "Runner checks not current"
        case .admission_policy_refused: "Current policy prevents execution"
        case .backup_protection_unready: "Backup protection not ready"
        case .repository_untrusted: "Repository trust not configured"
        case .provider_authority_unavailable: "GitHub App access unavailable"
        case .attended_mode_active: "Unattended work paused in attended mode"
        case .publication_environment: "Publication environment unavailable"
        case .external_conflict: "Conflicting branch or pull request"
        case .recipe_revoked: "Verification recipe no longer approved"
        case .verification_findings: "Verification findings block publication"
        case .trust_blocked: "Trust checks block publication"
        case .base_advanced: "Base branch changed"
        case .identity_parallelism, .usage_pool_exhausted: "Waiting for agent capacity"
        case .image_upkeep_pending: "Waiting for project image upkeep"
        case .verification_memory_limit: "Verification ran out of memory"
        case .quota_window_exhausted: "Waiting for quota window to reset"
        case .machine_capacity: "Waiting for machine capacity"
        case .update_drain: "Waiting for an update"
        }
    }

    /// A hold's typed fact in words, beside the hold's label, which stays one
    /// short phrase per reason: a capacity hold's pool or memory, and the
    /// selection part an admission hold refused. Reads only the fact that
    /// belongs to the hold's reason, and only when the hold is `runID`'s:
    /// another run's hold, another reason, or a missing fact (a stored
    /// `identity_parallelism` hold names no pool, and most admission holds
    /// name no part) has none. The words carry no tense, because a finished
    /// run's hold is a past observation. The memory figures are the
    /// reservations the hold recorded, not live use.
    package static func holdFact(_ hold: Components.Schemas.RunHold, runID: String) -> String? {
        guard hold.run_id == runID else { return nil }
        switch hold.reason {
        case .admission_policy_refused:
            return hold.detail?.value1.unserved.map { unservedFact($0.value1) }
        case .usage_pool_exhausted:
            return hold.usage_pool.map { "Pool \($0) at its limit of parallel runs" }
        case .machine_capacity:
            guard let memory = hold.machine_capacity?.value1 else { return nil }
            let reserved =
                "\(gibibytes(memory.reserved_mib)) of \(gibibytes(memory.budget_mib)) reserved"
                + " · \(gibibytes(memory.reservation_mib)) for this launch"
            switch memory.ahead {
            case ..<1: return reserved
            case 1: return reserved + " · 1 launch ahead"
            default: return reserved + " · \(memory.ahead) launches ahead"
            }
        default:
            return nil
        }
    }

    /// The part of a refused selection the daemon doesn't serve, naming the
    /// enrollment the daemon holds for a model or effort. The hold never
    /// carries what the operator typed, so neither does this.
    package static func unservedFact(_ unserved: Components.Schemas.AdmissionUnserved) -> String {
        let enrollment = unserved.enrollment_id ?? "the enrollment"
        switch unserved.part {
        case .enrollment: return "The enrollment isn't one the daemon holds"
        case .model: return "The model isn't one \(enrollment) serves"
        case .effort: return "The effort isn't one \(enrollment) serves for this model"
        }
    }

    /// MiB as GiB with one decimal. `String(format:)` ignores the device
    /// locale, so the figure and its screenshots read the same everywhere.
    private static func gibibytes(_ mib: Int) -> String {
        String(format: "%.1f GiB", Double(mib) / 1024)
    }

    package static func label(_ value: Components.Schemas.ScheduleKind) -> String {
        switch value {
        case .pr_checks_deadline: "Checks"
        case .review_wait_threshold: "Review"
        case .installation_poll: "Install watch"
        case .doctor: "Doctor"
        case .janitor: "Janitor"
        case .image_upkeep: "Image upkeep"
        }
    }
}
