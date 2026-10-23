import Foundation
import FreesideAPI

/// What a closed card says about how it closed, read from the item's
/// daemon-stamped `conclusion` (#2097). Every function here is pure over the
/// item, so the card, the inbox row, and the tests read one answer. An item
/// closed before the field existed carries no conclusion; it keeps the
/// status sentence it always had and draws no outcome facts.
package enum ConclusionDisplay {
    /// The sentence a closed card leads with in place of its ask, and its
    /// inbox row repeats; nil for an open item.
    package static func leadSentence(_ item: Components.Schemas.AttentionItem) -> String? {
        guard item.status != .open else { return nil }
        guard let conclusion = item.conclusion?.value1 else {
            return "This \(AttentionDisplay.title(item._type).lowercased()) item is "
                + "\(AttentionDisplay.label(item.status).lowercased())."
        }
        switch conclusion.cause {
        case .decision:
            guard let action = conclusion.decision?.value1.action else { return "Decided." }
            return decidedSentence(action, item: item)
        case .expired:
            return "Expired before a decision."
        case .superseded:
            return conclusion.successor_item_id == nil
                ? "Superseded."
                : "Superseded by a newer \(subjectNoun(item._type))."
        case .resolved_by_daemon:
            return "Resolved by the daemon."
        }
    }

    /// The decision's outcome in past tense. A revision supersedes the
    /// item it revises, so a decision that superseded says so.
    private static func decidedSentence(
        _ action: Components.Schemas.Action, item: Components.Schemas.AttentionItem
    ) -> String {
        if item.status == .superseded { return "Superseded by a revised \(subjectNoun(item._type))." }
        switch action {
        case .approve: return "\(subjectNoun(item._type).capitalizedFirst) approved."
        case .approve_with_changes: return "Approved with changes."
        case .request_changes: return "Changes requested."
        case .discuss: return "Sent back for discussion."
        case .stop: return "Stopped."
        case .finish_now: return "Finished now."
        case .apply_then_finish: return "Applied, then finished."
        case .continue_under_policy: return "Continued under policy."
        case .convert_to_policy: return "Converted to policy."
        case .retry: return "Retried."
        case .retry_with_capabilities: return "Retried with another profile."
        case .answer_and_retry: return "Answered and retried."
        case .answer_without_retry: return "Answered without a retry."
        case .rerun_trust_evaluation: return "Trust evaluation rerun."
        case .inspect_trust_failure: return "Trust failure inspected."
        case .open_pr: return "Pull request opened."
        case .return_to_agent: return "Returned to the agent."
        case .mark_seen: return "Marked seen."
        case .dismiss: return "Dismissed."
        case .start: return "Started."
        case .start_with_changes: return "Started with changes."
        case .decline: return "Declined."
        case .snooze: return "Snoozed."
        case .acknowledge: return "Acknowledged."
        case .run_doctor: return "Doctor run."
        case .stop_unattended: return "Unattended operation stopped."
        case .resume_unattended: return "Unattended operation resumed."
        case .recover_review: return "Review recovered."
        case .adopt_review_configuration: return "Review configuration adopted."
        case .resolve_reenrollment: return "Re-enrollment resolved."
        case .accept_recommended_route: return "Recommended route accepted."
        case .choose_alternative_route: return "Another route chosen."
        case .adjudicate_findings: return "Findings adjudicated."
        }
    }

    private static func subjectNoun(_ type: Components.Schemas.AttentionType) -> String {
        switch type {
        case .spec_approval: "specification"
        case .effect_proposal: "proposal"
        case .task_proposal: "task proposal"
        case .execution_failure, .agent_question, .review_diminishing_returns, .review_dispute,
            .review_contradiction, .review_configuration, .finding_adjudication, .ready_for_final_review,
            .publish_blocked, .system_health, .blocked:
            "item"
        }
    }

    /// The OUTCOME facts, in card order: who or what closed the item,
    /// when, why, and what replaced it. A decision names its action and the
    /// deciding device, "This device" when it is the one reading; a daemon
    /// close names the daemon and its reason. Empty for an open item and
    /// for one closed before conclusions existed. The time prints in
    /// `locale` and `timeZone`, the view's environment.
    package static func facts(
        _ item: Components.Schemas.AttentionItem, thisDeviceID: String?, now: Date,
        locale: Locale, timeZone: TimeZone
    ) -> [AttentionDisplay.FactRow] {
        guard item.status != .open, let conclusion = item.conclusion?.value1 else { return [] }
        var rows: [AttentionDisplay.FactRow] = []
        if let decision = conclusion.decision?.value1 {
            rows.append(.init("Action", AttentionDisplay.label(decision.action)))
            rows.append(
                .init(
                    "Decided by",
                    decision.device_id == thisDeviceID ? "This device" : decision.device_display_name))
        } else {
            rows.append(.init("Closed by", "Daemon"))
        }
        rows.append(
            .init("When", instant: conclusion.concluded_at, now: now, locale: locale, timeZone: timeZone))
        if !conclusion.reason.isEmpty {
            rows.append(.init("Reason", prose: conclusion.reason))
        }
        if let successor = conclusion.successor_item_id {
            rows.append(.init("Replaced by", AttentionDisplay.title(item._type), linkingItem: successor))
        }
        return rows
    }

    /// The task whose timeline tells what followed the item's close: the
    /// task its subject names. Nil when no synced task holds it, so the
    /// card draws no link to a task the app cannot show.
    package static func timelineTaskID(
        for item: Components.Schemas.AttentionItem, in tasks: [Components.Schemas.TaskSnapshot]
    ) -> String? {
        let taskID: String?
        switch item.subject {
        case .task(let task): taskID = task.task_id
        case .run(let run), .proposal_batch(let run): taskID = run.task_id
        case .project, .system: taskID = nil
        }
        guard let taskID, tasks.contains(where: { $0.task.id == taskID }) else { return nil }
        return taskID
    }

    /// The route the concluding decision applied to one finding, which a
    /// closed card outlines in its inert route list. Adjudication routes
    /// every finding; choosing alternatives routes the findings it names
    /// and accepts `proposed` for the rest, as accepting the recommendation
    /// does for all. Nil when no decision routed the finding: a daemon
    /// close, or a decision that stopped or discussed instead.
    package static func appliedRoute(
        for findingID: String, proposed: Components.Schemas.AdjudicationRoute?,
        in item: Components.Schemas.AttentionItem
    ) -> Components.Schemas.AdjudicationRoute? {
        guard let decision = item.conclusion?.value1.decision?.value1 else { return nil }
        let chosen = decision.finding_routes?.first { $0.finding_id == findingID }?.route
        switch decision.action {
        case .adjudicate_findings: return chosen
        case .accept_recommended_route, .choose_alternative_route: return chosen ?? proposed
        default: return nil
        }
    }
}

extension String {
    fileprivate var capitalizedFirst: String {
        prefix(1).uppercased() + dropFirst()
    }
}
