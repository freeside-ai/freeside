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

    struct SubjectLine: Equatable {
        let lead: String
        let identifier: String?
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
        case .adjudicate: return "Adjudicate"
        case .retry: return "Retry"
        case .retry_with_capabilities: return "Retry with capabilities"
        case .answer_and_retry: return "Answer and retry"
        case .answer_without_retry: return "Answer without retry"
        case .rerun_trust_evaluation: return "Rerun trust evaluation"
        case .choose_alternate_profile: return "Choose alternate profile"
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
            .continue_under_policy, .convert_to_policy, .adjudicate,
            .retry_with_capabilities, .answer_and_retry, .answer_without_retry,
            .rerun_trust_evaluation, .choose_alternate_profile,
            .inspect_trust_failure, .mark_seen, .dismiss, .start,
            .start_with_changes, .decline, .acknowledge, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route:
            return nil
        }
    }

    static func confirmationConsequence(_ action: Components.Schemas.Action) -> String? {
        switch action {
        case .stop:
            return "The current invocation is discarded. Work already exported stays; the round in flight does not."
        case .stop_unattended:
            return "New unattended work will not start until unattended operation is resumed."
        case .decline:
            return "The proposal is dismissed and no run starts."
        case .dismiss:
            return "The item closes without taking the requested action."
        case .approve, .request_changes, .discuss, .finish_now, .apply_then_finish,
            .continue_under_policy, .convert_to_policy, .adjudicate, .retry,
            .retry_with_capabilities, .answer_and_retry, .answer_without_retry,
            .rerun_trust_evaluation, .choose_alternate_profile,
            .inspect_trust_failure, .open_pr, .return_to_agent, .mark_seen,
            .start, .start_with_changes, .snooze, .acknowledge, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route:
            return nil
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

    static func detailBindingRows(
        _ item: Components.Schemas.AttentionItem,
        priorProposalDigest: String? = nil,
        proposalDigest: String? = nil
    ) -> [BindingRow] {
        var rows = [BindingRow(label: "Item version", value: "\(item.item_version)")]
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
        return [
            BindingRow(label: "Readiness", value: verdict),
            BindingRow(label: "Evaluation set", value: readiness.evaluation_set_digest),
        ]
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
