/// What accepting an action does beyond recording the command itself,
/// mirroring the signet boundary's actionOutcome table
/// (daemon/internal/signet/service.go) for every action. Concluding
/// actions flip the item's status in the accepting transaction;
/// record-only actions leave the item untouched (viewing a PR and
/// inspect_trust_failure navigate, acknowledge and mark_seen mean seen,
/// run_doctor leaves a system_health item blocking); the unattended
/// stop/resume transactions conclude the decided item and durably move the
/// daemon's operating state (#319: stop also raises the resume-offering
/// notice, resume resolves it); recover_review concludes its exact
/// contradiction carrier and authorizes the daemon's append-only recovery
/// transition; discuss appends to a conversation without concluding the
/// item; pending actions are rejected until the unit that owns their
/// transaction lands: convert_to_policy.
/// The daemon has more outcome kinds than this enum has cases; the
/// ActionOutcomeTests mapping says which collapse and why, and checks this
/// table against daemon/internal/signet/testdata/action-outcomes.golden.
/// No `default`, so a new Action member must declare its outcome here.
public enum ActionOutcome: Equatable {
    case concludes(Components.Schemas.ItemStatus)
    case discusses
    case records
    case pending
    case stopsUnattended
    case resumesUnattended
    case recoversReview
    case adoptsReviewConfiguration
    case resolvesReenrollment
    case revisesProposal
    case snoozesProposal

    public static func of(_ action: Components.Schemas.Action) -> ActionOutcome {
        switch action {
        case .dismiss, .decline:
            return .concludes(.dismissed)
        case .approve, .stop, .finish_now, .apply_then_finish, .retry,
            .continue_under_policy,
            .accept_recommended_route, .choose_alternative_route, .adjudicate_findings,
            .rerun_trust_evaluation, .start:
            return .concludes(.resolved)
        case .request_changes:
            return .concludes(.superseded)
        case .answer_and_retry, .return_to_agent, .retry_with_capabilities, .retry_with_alternate_agent:
            return .concludes(.superseded)
        case .answer_without_retry:
            return .concludes(.resolved)
        case .discuss:
            return .discusses
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
        case .start_with_changes, .approve_with_changes:
            return .revisesProposal
        case .snooze:
            return .snoozesProposal
        case .open_pr, .mark_seen, .acknowledge, .inspect_trust_failure, .run_doctor:
            return .records
        case .convert_to_policy:
            return .pending
        case .unknown:
            // An action this build doesn't know grants nothing: it is never
            // offered to the daemon as presentable, and a card never submits
            // it (DecisionModel.isSubmittable).
            return .pending
        }
    }
}
