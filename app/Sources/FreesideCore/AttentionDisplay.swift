import FreesideAPI

/// Human-readable labels for the contract's enums. Behaviour-dispatch
/// switches omit `default` on purpose: a new enum member must be handled
/// here before the code compiles.
enum AttentionDisplay {
    static func title(_ type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .spec_approval: return "Spec approval"
        case .execution_failure: return "Execution failure"
        case .agent_question: return "Agent question"
        case .review_diminishing_returns: return "Diminishing returns"
        case .review_dispute: return "Review dispute"
        case .ready_for_final_review: return "Ready for final review"
        case .publish_blocked: return "Publish blocked"
        case .run_proposal: return "Run proposal"
        case .system_health: return "System health"
        case .blocked: return "Blocked"
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
        case .open_pr: return "Open PR"
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

    static func subject(_ subject: Components.Schemas.Subject) -> String {
        switch subject {
        case .run(let run), .proposal_batch(let run):
            return run.subject_id
        case .project(let unscoped), .system(let unscoped):
            return unscoped.subject_id
        }
    }
}
