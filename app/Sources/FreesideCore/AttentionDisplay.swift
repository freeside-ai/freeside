import Foundation
import FreesideAPI

/// Human-readable labels for the contract's enums. Behaviour-dispatch
/// switches omit `default` on purpose: a new enum member must be handled
/// here before the code compiles.
enum AttentionDisplay {
    struct BindingRow: Equatable {
        let label: String
        let value: String
    }

    /// One Section 9 card fact: a daemon-produced value with the label the
    /// card shows it under. Identifiers and digests render monospaced so a
    /// fact that must be compared by eye reads as one.
    struct FactRow: Equatable, Identifiable {
        let label: String
        let value: String
        let monospaced: Bool

        var id: String { label }

        init(_ label: String, _ value: String, monospaced: Bool = false) {
            self.label = label
            self.value = value
            self.monospaced = monospaced
        }
    }

    struct SubjectLine: Equatable {
        let lead: String
        let identifier: String?
    }

    struct RowContext: Equatable {
        struct Segment: Equatable {
            let value: String
            let isIdentifier: Bool
        }

        let project: Segment
        let workUnit: Segment?
    }

    struct CopyableSubjectReference: Equatable {
        let label: String
        let value: String
    }

    static func title(_ type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .spec_approval: return "Spec approval"
        case .execution_failure: return "Execution failure"
        case .agent_question: return "Agent question"
        case .review_diminishing_returns: return "Diminishing returns"
        case .review_dispute: return "Review dispute"
        case .review_contradiction: return "Review contradiction"
        case .review_configuration: return "Review configuration"
        case .finding_adjudication: return "Finding adjudication"
        case .ready_for_final_review: return "Ready for final review"
        case .publish_blocked: return "Publish blocked"
        case .run_proposal: return "Run proposal"
        case .system_health: return "System health"
        case .blocked: return "Blocked"
        }
    }

    static func title(_ item: Components.Schemas.AttentionItem) -> String {
        guard item._type == .ready_for_final_review,
            item.readiness?.value1._class == .ready_degraded
        else { return title(item._type) }
        return "Ready for final review (degraded)"
    }

    static func ask(_ item: Components.Schemas.AttentionItem) -> String {
        switch item._type {
        case .spec_approval:
            return "Approve this specification for implementation?"
        case .execution_failure:
            return "How should this failed execution continue?"
        case .agent_question:
            return "How should the agent proceed with this question?"
        case .review_diminishing_returns:
            return "How should review conclude after diminishing returns?"
        case .review_dispute:
            return "How should this review dispute be resolved?"
        case .review_contradiction:
            return "Recover review under the approved execution contract?"
        case .review_configuration:
            return "Adopt this reviewer configuration and resume review?"
        case .finding_adjudication:
            return "Which disposition should apply to these review findings?"
        case .ready_for_final_review:
            return "Is this change ready for final GitHub review?"
        case .publish_blocked:
            return "How should publication recover from this trust failure?"
        case .run_proposal:
            return "Start this proposed run?"
        case .system_health:
            return "How should this system-health condition be handled?"
        case .blocked:
            return "What is keeping this run blocked?"
        }
    }

    static func rowSummary(_ item: Components.Schemas.AttentionItem) -> String {
        if item.status != .open {
            return "This \(title(item._type).lowercased()) item is "
                + "\(label(item.status).lowercased())."
        }
        switch item._type {
        case .spec_approval:
            return "A specification is ready for approval."
        case .execution_failure:
            return "An execution failed and needs a recovery choice."
        case .agent_question:
            return "The agent needs an answer before it can continue."
        case .review_diminishing_returns:
            return "Review has reached diminishing returns."
        case .review_dispute:
            return "A review finding needs a disposition."
        case .review_contradiction:
            return "Review contradicted the approved execution contract."
        case .review_configuration:
            return "Reviewer configuration no longer matches approved policy."
        case .finding_adjudication:
            if let round = item.finding_adjudication?.value1.round {
                return "Review round \(round) has findings to adjudicate."
            }
            return "Review findings need adjudication."
        case .ready_for_final_review:
            guard let readiness = item.readiness?.value1 else {
                return "Verification status is unavailable; final review is requested."
            }
            if readiness._class == .ready_degraded {
                return "Verification is degraded and needs final review."
            }
            return "Verification is clean and ready for final review."
        case .publish_blocked:
            return "Publication is blocked by a trust-policy check."
        case .run_proposal:
            return "A proposed run is ready to start."
        case .system_health:
            guard let diagnostic = item.health_diagnostic?.value1 else {
                return "A system-health condition needs attention."
            }
            guard diagnostic.impairs != Components.Schemas.ImpairedCapability.none else {
                return "Diagnostic \(diagnostic.code) is open; no capability is impaired."
            }
            return "\(label(diagnostic.impairs)) is impaired by diagnostic \(diagnostic.code)."
        case .blocked:
            guard let wait = item.blocked_on?.value1 else {
                return "A run is waiting on a blocker."
            }
            return "Waiting on \(phrase(wait.kind))."
        }
    }

    /// The Section 9 "leads with" facts for one item type, read from the
    /// item's typed fact fields. Every value is a daemon-produced card fact;
    /// nothing here is derived from prose, logs, or event names, and a type
    /// whose lead is a claim, an artifact, or its own module contributes no
    /// row.
    static func cardFacts(
        _ item: Components.Schemas.AttentionItem,
        now: Date
    ) -> [FactRow] {
        switch item._type {
        case .execution_failure:
            guard let failure = item.execution_failure?.value1 else { return [] }
            return [
                .init("Outcome", label(failure.outcome)),
                .init("Failing stage", label(failure.stage)),
                .init("Invocation", failure.invocation_id, monospaced: true),
            ]
        case .review_diminishing_returns:
            guard let cost = item.billable_cost_so_far?.value1 else { return [] }
            return [.init("Cost so far", costSoFar(cost))]
        case .review_dispute:
            guard let dispute = item.review_dispute?.value1 else { return [] }
            return [
                .init("Run", dispute.run_id, monospaced: true),
                .init("Round", "\(dispute.round)"),
                .init(
                    "Disputed findings",
                    dispute.finding_ids.joined(separator: ", "), monospaced: true),
                .init("Completion evidence", dispute.completion_evidence, monospaced: true),
            ]
        case .ready_for_final_review:
            guard let diff = item.diff_stats?.value1 else { return [] }
            return [
                .init("Diff", diffStats(diff)),
                .init("Base", diff.base_sha, monospaced: true),
                .init("Head", diff.head_sha, monospaced: true),
            ]
        case .publish_blocked:
            guard let block = item.publish_block?.value1 else { return [] }
            if let rule = block.trust_rule?.value1 {
                return [.init("Failed trust rule", label(rule))]
            }
            if let hold = block.hold_reason?.value1 {
                return [.init("Hold reason", label(hold))]
            }
            return []
        case .system_health:
            guard let diagnostic = item.health_diagnostic?.value1 else { return [] }
            return [
                .init("Diagnostic", diagnostic.code, monospaced: true),
                .init("Impairs", label(diagnostic.impairs)),
            ]
        case .blocked:
            guard let wait = item.blocked_on?.value1 else { return [] }
            var rows: [FactRow] = [
                .init("Waiting on", label(wait.kind)),
                // The wait reads as the duration the inbox row already uses.
                // Its exact start is an audit coordinate, so it stays with the
                // other technical bindings rather than leading the card as a
                // monospaced timestamp.
                .init("Waiting for", relativeRowTime(wait.since, now: now)),
            ]
            if let blockingItem = wait.item_id {
                rows.append(.init("Blocking item", blockingItem, monospaced: true))
            }
            if let pull = wait.pr_reference?.value1 {
                rows.append(
                    .init("Pull request", "\(pull.repo)#\(pull.number)", monospaced: true))
            }
            return rows
        // The ask leads a spec approval and an agent question, the adjudication
        // artifact leads finding_adjudication, the authenticated proposal
        // snapshot leads run_proposal, and the recovery bindings these two
        // recovery types lead with are already their own rows.
        case .spec_approval, .agent_question, .finding_adjudication, .run_proposal,
            .review_contradiction, .review_configuration:
            return []
        }
    }

    private static func costSoFar(_ cost: Components.Schemas.CostSoFar) -> String {
        let invocations =
            cost.invocations == 1 ? "1 invocation" : "\(cost.invocations) invocations"
        return "\(cost.currency) \(cost.amount) across \(invocations)"
            + (cost.complete ? "" : ", still accruing")
    }

    private static func diffStats(_ diff: Components.Schemas.DiffStats) -> String {
        let files = diff.files_changed == 1 ? "1 file" : "\(diff.files_changed) files"
        return "\(files), +\(diff.additions) -\(diff.deletions)"
    }

    static func label(_ action: Components.Schemas.Action) -> String {
        switch action {
        case .approve: return "Approve"
        case .request_changes: return "Request changes"
        case .discuss: return "Discuss"
        case .stop: return "Stop"
        case .finish_now: return "Finish now"
        case .apply_then_finish: return "Apply, then finish"
        case .continue_under_policy: return "Continue under policy"
        case .convert_to_policy: return "Convert to policy"
        case .retry: return "Retry"
        case .retry_with_capabilities: return "Retry with profile"
        case .answer_and_retry: return "Answer and retry"
        case .answer_without_retry: return "Answer without retry"
        case .rerun_trust_evaluation: return "Rerun trust evaluation"
        case .inspect_trust_failure: return "Inspect trust failure"
        case .open_pr: return "View PR"
        case .return_to_agent: return "Return to agent"
        case .mark_seen: return "Mark seen"
        case .dismiss: return "Dismiss"
        case .start: return "Start"
        case .start_with_changes: return "Start with changes"
        case .decline: return "Decline"
        case .snooze: return "Snooze"
        case .acknowledge: return "Acknowledge"
        case .run_doctor: return "Run doctor"
        case .stop_unattended: return "Stop unattended"
        case .resume_unattended: return "Resume unattended"
        case .recover_review: return "Recover review"
        case .adopt_review_configuration: return "Adopt review configuration"
        case .resolve_reenrollment: return "Resolve re-enrollment"
        case .accept_recommended_route: return "Accept recommended route"
        case .choose_alternative_route: return "Choose selected alternative"
        }
    }

    static func systemImage(_ action: Components.Schemas.Action) -> String? {
        switch action {
        case .open_pr: return "arrow.up.right.square"
        case .retry: return "arrow.clockwise"
        case .snooze: return "clock"
        case .stop, .stop_unattended: return "stop.fill"
        case .return_to_agent: return "return"
        case .approve, .request_changes, .discuss, .finish_now, .apply_then_finish,
            .continue_under_policy, .convert_to_policy,
            .retry_with_capabilities, .answer_and_retry, .answer_without_retry,
            .rerun_trust_evaluation,
            .inspect_trust_failure, .mark_seen, .dismiss, .start,
            .start_with_changes, .decline, .acknowledge, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route:
            return nil
        }
    }

    static func confirmationConsequence(
        _ action: Components.Schemas.Action,
        for item: Components.Schemas.AttentionItem
    ) -> String? {
        switch action {
        case .stop:
            switch item._type {
            case .finding_adjudication:
                return "The run stays parked without accepting or choosing an adjudication route."
            case .review_configuration:
                return "The run concludes as a configuration failure; no replacement review configuration is adopted."
            case .spec_approval, .review_diminishing_returns, .review_dispute,
                .review_contradiction, .execution_failure, .agent_question,
                .publish_blocked, .ready_for_final_review, .run_proposal,
                .system_health, .blocked:
                break
            }
            return "The current invocation is discarded. Work already exported stays; the round in flight does not."
        case .stop_unattended:
            return "New unattended work will not start until unattended operation is resumed."
        case .decline:
            return "The proposal is dismissed and no run starts."
        case .dismiss:
            return "The item closes without taking the requested action."
        case .approve, .request_changes, .discuss, .finish_now, .apply_then_finish,
            .continue_under_policy, .convert_to_policy, .retry,
            .retry_with_capabilities, .answer_and_retry, .answer_without_retry,
            .rerun_trust_evaluation,
            .inspect_trust_failure, .open_pr, .return_to_agent, .mark_seen,
            .start, .start_with_changes, .snooze, .acknowledge, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route:
            return nil
        }
    }

    static func label(_ outcome: Components.Schemas.ExecutionOutcomeStatus) -> String {
        switch outcome {
        case .failed: return "Failed"
        case .canceled: return "Canceled"
        case .lost: return "Lost"
        case .blocked: return "Blocked"
        }
    }

    static func label(_ stage: Components.Schemas.StageName) -> String {
        switch stage {
        case .specification: return "Specification"
        case .implementation: return "Implementation"
        case .review: return "Review"
        case .verification: return "Verification"
        }
    }

    static func label(_ rule: Components.Schemas.TrustRule) -> String {
        switch rule {
        case .recipe_unapproved: return "Verification recipe not approved"
        case .verification_failed: return "Verification failed"
        case .trust_profile_drift: return "Trust profile drifted"
        case .target_base_advanced: return "Target base advanced"
        }
    }

    static func label(_ reason: Components.Schemas.RunHoldReason) -> String {
        switch reason {
        case .operation_stopped: return "Unattended operation stopped"
        case .blocking_system_health: return "Blocking system-health item"
        case .input_unavailable: return "Input unavailable"
        case .backend_not_conformant: return "Runner backend not conformant"
        case .admission_policy_refused: return "Admission policy refused"
        case .backup_protection_unready: return "Backup protection not ready"
        case .repository_untrusted: return "Repository untrusted"
        case .provider_authority_unavailable: return "Provider authority unavailable"
        case .attended_mode_active: return "Attended mode active"
        case .publication_environment: return "Publication environment"
        case .external_conflict: return "External conflict"
        case .recipe_revoked: return "Verification recipe revoked"
        case .verification_findings: return "Verification findings"
        case .trust_blocked: return "Trust blocked"
        case .base_advanced: return "Base advanced"
        case .identity_parallelism: return "Identity parallelism limit"
        }
    }

    static func label(_ capability: Components.Schemas.ImpairedCapability) -> String {
        switch capability {
        case .unattended_admission: return "Unattended admission"
        case .run_visibility: return "Run visibility"
        case .agent_credential: return "Agent credential"
        case .none: return "No capability"
        }
    }

    static func label(_ kind: Components.Schemas.BlockedWaitKind) -> String {
        switch kind {
        case .spec_approval: return "Specification approval"
        case .pr_checks: return "PR checks"
        case .external_review: return "External review"
        }
    }

    /// The same wait, mid-sentence: "PR checks" must not be lowercased into
    /// "pr checks" by a caller.
    private static func phrase(_ kind: Components.Schemas.BlockedWaitKind) -> String {
        switch kind {
        case .spec_approval: return "specification approval"
        case .pr_checks: return "PR checks"
        case .external_review: return "external review"
        }
    }

    static func label(_ priority: Components.Schemas.Priority) -> String {
        switch priority {
        case .low: return "Low"
        case .normal: return "Normal"
        case .high: return "High"
        case .urgent: return "Urgent"
        }
    }

    static func label(_ status: Components.Schemas.ItemStatus) -> String {
        switch status {
        case .open: return "Open"
        case .resolved: return "Resolved"
        case .superseded: return "Superseded"
        case .dismissed: return "Dismissed"
        case .expired: return "Expired"
        }
    }

    static func label(_ posture: Components.Schemas.HealthPosture) -> String {
        switch posture {
        case .blocking: return "Blocking"
        case .advisory: return "Advisory"
        }
    }

    static func label(_ notice: Components.Schemas.CommitPlanNoticeReason) -> String {
        switch notice {
        case .absent: return "No plan provided"
        case .structural: return "Plan rejected (structure)"
        case .screening: return "Plan rejected (message screening)"
        case .present_but_not_honored: return "Plan present, not honored"
        }
    }

    static func subject(_ item: Components.Schemas.AttentionItem) -> SubjectLine {
        switch item.subject {
        case .run(let run), .proposal_batch(let run):
            return SubjectLine(lead: item.project_id, identifier: run.subject_id)
        case .project(let unscoped), .system(let unscoped):
            return SubjectLine(lead: unscoped.subject_id, identifier: nil)
        }
    }

    /// Row context comes from the daemon's own labels (`display_names`), which
    /// carry whether each one is a chosen name or an identifier fallback. A
    /// legacy item without them falls back to its raw identifiers.
    static func rowContext(_ item: Components.Schemas.AttentionItem) -> RowContext {
        let names = item.display_names?.value1
        let project = RowContext.Segment(
            value: nonempty(names?.project.text) ?? item.project_id,
            isIdentifier: names.map { $0.project.source == .identifier } ?? true
        )
        let workUnit: RowContext.Segment?
        switch item.subject {
        case .run(let run), .proposal_batch(let run):
            workUnit = .init(
                value: nonempty(names?.work_unit.text) ?? run.subject_id,
                isIdentifier: names.map { $0.work_unit.source == .identifier } ?? true
            )
        case .project, .system:
            workUnit = nil
        }
        return .init(project: project, workUnit: workUnit)
    }

    static func copyableSubjectReference(
        _ item: Components.Schemas.AttentionItem
    ) -> CopyableSubjectReference? {
        switch item.subject {
        case .run(let run):
            return .init(label: "Copy run reference", value: run.subject_id)
        case .proposal_batch(let batch):
            return .init(label: "Copy proposal batch reference", value: batch.subject_id)
        case .project, .system:
            return nil
        }
    }

    static func relativeRowTime(
        _ item: Components.Schemas.AttentionItem,
        now: Date
    ) -> String? {
        if item.status == .open, let due = item.expires_when {
            let remaining = due.timeIntervalSince(now)
            if abs(remaining) < 60 {
                return "due now"
            }
            if remaining > 0 {
                return "due in \(relativeDuration(remaining))"
            }
            return "overdue \(relativeDuration(-remaining))"
        }
        let created = rowTimeOrigin(item)
        guard let created else { return nil }
        let duration = relativeDuration(max(0, now.timeIntervalSince(created)))
        return item.status == .open && item._type == .blocked ? "blocked \(duration)" : duration
    }

    static func relativeRowTime(_ date: Date, now: Date) -> String {
        relativeDuration(max(0, now.timeIntervalSince(date)))
    }

    static func exactRowTimestamp(
        _ item: Components.Schemas.AttentionItem,
        now: Date
    ) -> String? {
        if item.status == .open, let due = item.expires_when {
            return due.formatted(.iso8601)
        }
        return rowTimeOrigin(item)?.formatted(.iso8601)
    }

    static func showsPriorityBadge(_ priority: Components.Schemas.Priority) -> Bool {
        switch priority {
        case .urgent, .high: return true
        case .normal, .low: return false
        }
    }

    static func showsLifecycleBadge(_ status: Components.Schemas.ItemStatus) -> Bool {
        status != .open
    }

    static func showsDegradedBadge(_ item: Components.Schemas.AttentionItem) -> Bool {
        item.readiness?.value1._class == .ready_degraded
    }

    static func uniqueEvidenceDigests(
        _ item: Components.Schemas.AttentionItem
    ) -> [String] {
        var seen: Set<String> = []
        return item.evidence_snapshot.compactMap { artifact in
            seen.insert(artifact.digest).inserted ? artifact.digest : nil
        }
    }

    static func attachmentDigestRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        var rows: [BindingRow] = []
        var representedDigests: Set<String> = []
        var seenEvidenceDigests: Set<String> = []
        var seenClaimDigests: Set<String> = []

        func append(_ label: String, _ digest: String, seen: inout Set<String>) {
            guard seen.insert(digest).inserted else { return }
            rows.append(.init(label: label, value: digest))
            representedDigests.insert(digest)
        }

        for artifact in item.evidence_snapshot {
            append("Evidence digest", artifact.digest, seen: &seenEvidenceDigests)
        }
        for claim in item.agent_claims {
            append("Claim digest", claim.digest, seen: &seenClaimDigests)
        }
        for digest in item.artifact_digests {
            guard representedDigests.insert(digest).inserted else { continue }
            rows.append(.init(label: "Artifact digest", value: digest))
        }
        return rows
    }

    /// Section 9's audit record for actions this client cannot faithfully
    /// collect and execute: they are omitted from the action surface and
    /// listed in the drill-down, so an audit still shows what the daemon
    /// asked for.
    static func unavailableActionRows(
        _ actions: [Components.Schemas.Action]
    ) -> [BindingRow] {
        actions.map { .init(label: "Requested, not available here", value: label($0)) }
    }

    static func detailBindingRows(
        _ item: Components.Schemas.AttentionItem,
        priorProposalDigest: String? = nil,
        proposalDigest: String? = nil
    ) -> [BindingRow] {
        var rows: [BindingRow] = []
        if let created = item.created_at {
            rows.append(.init(label: "Created", value: created.formatted(.iso8601)))
        }
        if let due = item.expires_when {
            rows.append(.init(label: "Due", value: due.formatted(.iso8601)))
        }
        switch item.subject {
        case .project(let unscoped), .system(let unscoped):
            rows.append(.init(label: "Subject", value: unscoped.subject_id))
        case .run, .proposal_batch:
            break
        }
        if let wait = item.blocked_on?.value1 {
            rows.append(
                .init(label: "Waiting since", value: wait.since.formatted(.iso8601)))
        }
        rows.append(.init(label: "Item version", value: "\(item.item_version)"))
        if !item.pr_head_sha.isEmpty {
            rows.append(.init(label: "PR head", value: item.pr_head_sha))
        }
        rows.append(contentsOf: attachmentDigestRows(item))
        if let priorProposalDigest {
            rows.append(.init(label: "Prior proposal", value: priorProposalDigest))
        }
        if let proposalDigest {
            rows.append(.init(label: "Proposal", value: proposalDigest))
        }
        rows.append(contentsOf: reviewRecoveryBindingRows(item))
        rows.append(contentsOf: reviewConfigurationRecoveryRows(item))
        rows.append(contentsOf: codexReenrollmentRecoveryRows(item))
        rows.append(contentsOf: findingAdjudicationRows(item))
        rows.append(contentsOf: readinessSummaryRows(item))
        rows.append(contentsOf: reviewYieldRows(item))
        return rows
    }

    private static func nonempty(_ value: String?) -> String? {
        guard let value, !value.isEmpty else { return nil }
        return value
    }

    /// A blocked row counts from the daemon's recorded wait start, not the
    /// item's creation: the wait predates the card.
    private static func rowTimeOrigin(_ item: Components.Schemas.AttentionItem) -> Date? {
        guard item.status == .open, item._type == .blocked else { return item.created_at }
        return item.blocked_on?.value1.since ?? item.created_at
    }

    private static func relativeDuration(_ interval: TimeInterval) -> String {
        if interval < 3_600 {
            return "\(max(1, Int(interval / 60)))m"
        }
        if interval < 86_400 {
            return "\(Int(interval / 3_600))h"
        }
        return "\(Int(interval / 86_400))d"
    }

    static func reviewYieldRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let history = item.yield_history?.value1 else { return [] }
        var rows = history.rounds.map { round in
            BindingRow(
                label: "Review round \(round.round)",
                value: "\(round.findings_ingested) findings · \(round.new_findings) new · "
                    + "\(round.recurring_findings) recurring · \(round.fixed) fixed · "
                    + "\(round.declined) declined · \(round.deferred) deferred · "
                    + reviewOutcomeLabel(round.outcome)
            )
        }
        rows.append(
            .init(
                label: "Terminal review",
                value: reviewOutcomeLabel(history.terminal_outcome)))
        return rows
    }

    private static func reviewOutcomeLabel(
        _ outcome: Components.Schemas.ReviewOutcome
    ) -> String {
        switch outcome {
        case .clean: return "Clean"
        case .findings: return "Findings"
        }
    }

    static func readinessSummaryRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let readiness = item.readiness?.value1 else { return [] }
        let verdict: String
        switch readiness._class {
        case .ready_clean: verdict = "Clean"
        case .ready_degraded: verdict = "Degraded"
        }
        var rows = [
            BindingRow(label: "Readiness", value: verdict),
            BindingRow(label: "Evaluation set", value: readiness.evaluation_set_digest),
        ]
        guard let detail = item.readiness_detail?.value1 else { return rows }
        rows.append(BindingRow(label: "Bound head", value: detail.candidate_head))
        rows.append(
            BindingRow(label: "Bound base", value: "\(detail.base.base_ref)@\(detail.base.base_sha)"))
        for requirement in detail.requirements {
            var value = [
                label(requirement.check_class), label(requirement.kind), label(requirement.state),
            ].joined(separator: ", ")
            if let proof = requirement.proof_recipe_digest?.value1 {
                value += ", proof \(proof)"
            }
            rows.append(BindingRow(label: "Requirement \(requirement.requirement_key)", value: value))
            if let waiver = requirement.waiver?.value1 {
                rows.append(
                    BindingRow(
                        label: "Waiver \(waiver.id)",
                        value:
                            "\(waiver.dimension), \(label(waiver.authority)), "
                            + waiver.granted_at.formatted(.iso8601)))
            }
        }
        return rows
    }

    /// A revision shortened for a card row; the inspector keeps the full
    /// daemon value. Only a hex object name is abbreviated. The other
    /// coordinates a readiness invalidation carries on its axis, a base ref
    /// and a "repository_id#pr_number" identity, put their meaning in the
    /// tail, so truncating them can render two different values identically
    /// and hide the very change the row exists to name.
    static func shortRevision(_ value: String) -> String {
        guard value.count > 12, value.allSatisfy(\.isHexDigit) else { return value }
        return String(value.prefix(12))
    }

    static func label(_ state: Components.Schemas.ReadinessRequirementState) -> String {
        switch state {
        case .passed: return "Passed"
        case .failed: return "Failed"
        case .not_run: return "Not run"
        case .not_applicable: return "Not applicable"
        }
    }

    static func label(_ checkClass: Components.Schemas.VerificationCheckClass) -> String {
        switch checkClass {
        case .clean_verification: return "Clean verification"
        case .independent_review: return "Independent review"
        case .repo_change_policy: return "Repo change policy"
        }
    }

    static func label(_ kind: Components.Schemas.RequirementKind) -> String {
        switch kind {
        case .required: return "Required"
        case .optional: return "Optional"
        }
    }

    static func label(_ authority: Components.Schemas.WaiverGrantingAuthority) -> String {
        switch authority {
        case .explicit_human_approval: return "Explicit human approval"
        case .daemon_trusted_configuration: return "Daemon trusted configuration"
        }
    }

    static func label(_ reason: Components.Schemas.ReadinessInvalidationReason) -> String {
        switch reason {
        case .head_changed: return "Head changed"
        case .base_advanced: return "Base advanced"
        case .retargeted: return "Retargeted"
        case .identity_changed: return "Identity changed"
        }
    }

    static func findingAdjudicationRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.finding_adjudication?.value1 else { return [] }
        return [
            BindingRow(label: "Adjudication digest", value: binding.adjudication_digest),
            BindingRow(label: "Adjudication run", value: binding.run_id),
            BindingRow(label: "Adjudication round", value: "\(binding.round)"),
        ]
    }

    static func label(_ route: Components.Schemas.AdjudicationRoute) -> String {
        switch route {
        case .remediate: return "Remediate"
        case .park_revision: return "Revise this work unit"
        case .park_separate_work: return "Create separate work"
        case .attention_human_decision: return "Human decision"
        case .park_unknown: return "Park as unknown"
        case ._defer: return "Defer"
        case .decline: return "Decline"
        case .dispute: return "Dispute"
        case .attention_unclear: return "Clarify"
        }
    }

    static func label(_ site: Components.Schemas.JudgmentSite) -> String {
        switch site {
        case .finding_adjudicator: return "Finding adjudicator"
        }
    }

    static func label(_ relationship: Components.Schemas.GoalRelationship) -> String {
        switch relationship {
        case .required: return "Required"
        case .adjacent: return "Adjacent"
        case .contradictory: return "Contradictory"
        case .unclear: return "Unclear"
        }
    }

    static func label(_ confidence: Components.Schemas.AdjudicationConfidence) -> String {
        switch confidence {
        case .low: return "Low"
        case .medium: return "Medium"
        case .high: return "High"
        }
    }

    /// Renders a finding location the way the daemon's canonical location string
    /// does: "path" for a whole-file location, "path:line" for a single line, and
    /// "path:start-end" for a range.
    static func findingLocation(_ location: Components.Schemas.FindingLocation) -> String {
        if location.start_line == 0 && location.end_line == 0 {
            return location.path
        }
        if location.start_line == location.end_line {
            return "\(location.path):\(location.start_line)"
        }
        return "\(location.path):\(location.start_line)-\(location.end_line)"
    }

    static func adjudicationProducerPresentation(
        _ producer: Components.Schemas.AdjudicationProducer
    ) -> (label: String, modelBacked: Bool) {
        switch producer {
        case .engine: return ("Daemon recommendation", false)
        case .model: return ("Model proposal (unverified)", true)
        case .engine_model:
            return ("Model judgment with engine-authorized remediation", true)
        }
    }

    static func label(_ compatibility: Components.Schemas.WorkUnitCompatibility?) -> String {
        guard let compatibility else { return "Not assessed" }
        switch compatibility {
        case .allowed: return "Allowed"
        case .work_unit_revision_required: return "Work-unit revision required"
        case .separate_work_required: return "Separate work required"
        case .human_decision_required: return "Human decision required"
        case .unknown: return "Unknown"
        }
    }

    static func reviewRecoveryBindingRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.review_recovery_binding?.value1 else { return [] }
        return [
            BindingRow(label: "Recovery run", value: binding.run_id),
            BindingRow(label: "Invocation", value: binding.invocation_id),
            BindingRow(label: "Round", value: "\(binding.round)"),
            BindingRow(label: "Base", value: binding.base_sha),
            BindingRow(label: "Head", value: binding.head_sha),
            BindingRow(label: "Failure digest", value: binding.failure_digest),
        ]
    }

    static func reviewConfigurationRecoveryRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.review_configuration_recovery?.value1 else { return [] }
        return [
            BindingRow(label: "Recovery run", value: binding.run_id),
            BindingRow(label: "Invocation", value: binding.invocation_id),
            BindingRow(label: "Round", value: "\(binding.round)"),
            BindingRow(label: "Base", value: binding.base_sha),
            BindingRow(label: "Head", value: binding.head_sha),
            BindingRow(label: "Failure digest", value: binding.failure_digest),
            BindingRow(label: "Repository", value: binding.repo),
            BindingRow(label: "Superseded profile", value: binding.superseded_profile_digest),
        ]
    }

    static func codexReenrollmentRecoveryRows(
        _ item: Components.Schemas.AttentionItem
    ) -> [BindingRow] {
        guard let binding = item.codex_reenrollment_recovery_binding?.value1 else { return [] }
        return [
            BindingRow(label: "Auth identity", value: binding.auth_identity_id),
            BindingRow(label: "Lease fence", value: "\(binding.lease_fence)"),
            BindingRow(label: "Auth store digest", value: binding.auth_store_digest),
            BindingRow(
                label: "Token expires",
                value: binding.access_token_expires_at.formatted(.iso8601)
            ),
        ]
    }
}
