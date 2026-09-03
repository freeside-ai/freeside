import FreesideAPI

/// A pure presentation partition over the daemon-offered decision set.
/// The optional recommendation must come from an authoritative contract
/// projection. Callers never infer it from offer order.
struct DecisionActionRanking: Equatable {
    let recommended: Components.Schemas.Action?
    let principal: [Components.Schemas.Action]
    let reviewing: Components.Schemas.Action?
    let overflow: [Components.Schemas.Action]
    let unavailable: [Components.Schemas.Action]
    let notDecidableHere: Bool

    init(
        requested: [Components.Schemas.Action],
        recommendedAction: Components.Schemas.Action? = nil,
        reservesRecommendedAction: Bool = true,
        servedActions: [Components.Schemas.Action]? = nil
    ) {
        // The daemon-served action surface is authoritative when present: it is
        // the item's requested decisions already intersected with this device's
        // capability contract (plan §8), so an action the surface omits is
        // unavailable here. Without a surface (the fetch failed, or an older
        // build) the local not-`.pending` filter stands.
        let available: [Components.Schemas.Action]
        if let servedActions {
            let served = Set(servedActions)
            available = requested.filter { served.contains($0) }
            unavailable = requested.filter { !served.contains($0) }
        } else {
            unavailable = requested.filter { ActionOutcome.of($0) == .pending }
            available = requested.filter { ActionOutcome.of($0) != .pending }
        }
        let authoritativeRecommendation =
            reservesRecommendedAction
            ? recommendedAction.flatMap { recommendation in
                available.contains(recommendation) ? recommendation : nil
            }
            : nil
        recommended = authoritativeRecommendation

        let secondaryActions = available.filter { $0 != authoritativeRecommendation }

        let reviewingAction = secondaryActions.first(where: Self.isReviewing)
        reviewing = reviewingAction

        let overflowActions = secondaryActions.filter(Self.isOverflow).sorted {
            Self.overflowRank($0) < Self.overflowRank($1)
        }
        overflow = overflowActions

        principal = secondaryActions.filter { action in
            action != reviewingAction
                && !Self.isOverflow(action)
        }
        notDecidableHere =
            !requested.isEmpty
            && authoritativeRecommendation == nil
            && principal.isEmpty
            && reviewingAction == nil
            && overflowActions.isEmpty
    }

    private static func isReviewing(_ action: Components.Schemas.Action) -> Bool {
        switch action {
        case .open_pr, .inspect_trust_failure: return true
        case .approve, .request_changes, .discuss, .stop, .finish_now,
            .apply_then_finish, .continue_under_policy, .convert_to_policy,
            .retry, .retry_with_capabilities, .answer_and_retry,
            .answer_without_retry, .rerun_trust_evaluation,
            .return_to_agent, .mark_seen, .dismiss,
            .start, .start_with_changes, .decline, .snooze, .acknowledge,
            .run_doctor, .stop_unattended, .resume_unattended, .recover_review,
            .adopt_review_configuration, .resolve_reenrollment,
            .accept_recommended_route, .choose_alternative_route:
            return false
        }
    }

    private static func isOverflow(_ action: Components.Schemas.Action) -> Bool {
        switch action {
        case .snooze, .mark_seen, .acknowledge, .dismiss, .decline, .stop,
            .stop_unattended:
            return true
        case .approve, .request_changes, .discuss, .finish_now,
            .apply_then_finish, .continue_under_policy, .convert_to_policy,
            .retry, .retry_with_capabilities, .answer_and_retry,
            .answer_without_retry, .rerun_trust_evaluation,
            .inspect_trust_failure, .open_pr,
            .return_to_agent, .start, .start_with_changes, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route:
            return false
        }
    }

    private static func overflowRank(_ action: Components.Schemas.Action) -> Int {
        switch action {
        case .snooze: return 0
        case .mark_seen: return 1
        case .acknowledge: return 2
        case .dismiss: return 10
        case .decline: return 11
        case .stop: return 12
        case .stop_unattended: return 13
        case .approve, .request_changes, .discuss, .finish_now,
            .apply_then_finish, .continue_under_policy, .convert_to_policy,
            .retry, .retry_with_capabilities, .answer_and_retry,
            .answer_without_retry, .rerun_trust_evaluation,
            .inspect_trust_failure, .open_pr,
            .return_to_agent, .start, .start_with_changes, .run_doctor,
            .resume_unattended, .recover_review, .adopt_review_configuration,
            .resolve_reenrollment, .accept_recommended_route,
            .choose_alternative_route:
            return .max
        }
    }
}
