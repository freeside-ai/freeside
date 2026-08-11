/// What accepting an action does beyond recording the command itself,
/// mirroring the signet boundary's actionOutcome table
/// decision-for-decision (daemon/internal/signet/service.go). Concluding
/// actions flip the item's status in the accepting transaction;
/// record-only actions leave the item untouched (viewing a PR and
/// inspect_trust_failure navigate, acknowledge and mark_seen mean seen,
/// run_doctor leaves a system_health item blocking); the unattended
/// stop/resume transactions conclude the decided item and durably move the
/// daemon's operating state (#319: stop also raises the resume-offering
/// notice, resume resolves it); recover_review concludes its exact
/// contradiction carrier and authorizes the daemon's append-only recovery
/// transition; pending actions are rejected until the unit
/// that owns their transaction lands (conversations, timing, proposal
/// revision, parameter-carrying payloads). A provisional client mirror
/// pending a queryable contract representation (#22); no `default`, so a new
/// Action member must declare its outcome here.
public enum ActionOutcome: Equatable {
    case concludes(Components.Schemas.ItemStatus)
    case records
    case pending
    case stopsUnattended
    case resumesUnattended
    case recoversReview
    case adoptsReviewConfiguration
    case resolvesReenrollment

    public static func of(_ action: Components.Schemas.Action) -> ActionOutcome {
        switch action {
        case .dismiss, .decline:
            return .concludes(.dismissed)
        case .approve, .stop, .finish_now, .apply_then_finish, .retry,
            .rerun_trust_evaluation, .start:
            return .concludes(.resolved)
        case .stop_unattended:
            return .stopsUnattended
        case .resume_unattended:
            return .resumesUnattended
        case .recover_review:
            return .recoversReview
        case .adopt_review_configuration:
            return .adoptsReviewConfiguration
        case .resolve_reenrollment:
            return .resolvesReenrollment
        case .open_pr, .mark_seen, .acknowledge, .inspect_trust_failure, .run_doctor:
            return .records
        case .discuss, .snooze, .start_with_changes, .continue_under_policy,
            .convert_to_policy, .adjudicate, .retry_with_capabilities,
            .choose_alternate_profile, .request_changes, .answer_and_retry,
            .answer_without_retry, .return_to_agent:
            return .pending
        }
    }
}
