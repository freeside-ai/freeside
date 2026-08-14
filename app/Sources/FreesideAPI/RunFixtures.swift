import Foundation

/// Deterministic daemon-observation fixtures for the runs list and timeline screens.
/// They contain no agent-authored text, matching the production wire shape.
public enum RunFixtures {
    public static let activeRunID = "run-freeside-657"
    public static let readyRunID = "run-freeside-654"
    public static let legacyRunID = "run-freeside-540"

    public static func defaultRuns() -> [Components.Schemas.RunSnapshot] {
        [
            snapshot(
                id: activeRunID, projectID: "freeside", stage: "implementation",
                attempt: 2, milestone: .invocation_started, outcome: .pending,
                hold: .verification_findings),
            snapshot(
                id: readyRunID, projectID: "freeside", stage: "publication",
                attempt: 1, milestone: .publication_ready, outcome: .published),
            snapshot(
                id: "run-oriole-121", projectID: "oriole", stage: "verification",
                attempt: 1, milestone: .terminal_recorded, outcome: .failed),
            // A pre-migration-0024 legacy run: structural stages but no
            // observation milestones, so an unobserved outcome and no timeline.
            snapshot(
                id: legacyRunID, projectID: "freeside", stage: "implementation",
                attempt: 1, milestone: nil, outcome: .unobserved),
        ]
    }

    public static func defaultSchedules() -> [Components.Schemas.ScheduleSnapshot] {
        let created = date(0)
        return [
            schedule(
                id: "schedule-checks-657", kind: .pr_checks_deadline,
                runID: activeRunID, fireAt: date(7_200), createdAt: created),
            schedule(
                id: "schedule-review-657", kind: .review_wait_threshold,
                runID: activeRunID, fireAt: date(3_600), createdAt: created),
            schedule(
                id: "schedule-base-654", kind: .base_advance_watch,
                runID: readyRunID, createdAt: created,
                baseWatch: .init(
                    value1: .init(
                        repo: "freeside-ai/freeside", base_ref: "main",
                        admitted_base_sha: String(repeating: "a", count: 40)))),
        ]
    }

    public static func defaultTimelines() -> [Components.Schemas.RunTimeline] {
        let activeMilestones: [Components.Schemas.RunMilestone] = [
            milestone(.run_submitted, runID: activeRunID, minute: 0),
            milestone(.invocation_admitted, runID: activeRunID, minute: 1),
            milestone(.invocation_started, runID: activeRunID, minute: 2),
            milestone(.execution_export_recorded, runID: activeRunID, minute: 18),
            milestone(.invocation_admitted, runID: activeRunID, attempt: 2, minute: 20),
            milestone(.invocation_started, runID: activeRunID, attempt: 2, minute: 21),
        ]
        let hold = Components.Schemas.RunHold(
            run_id: activeRunID,
            invocation_id: "inv-\(activeRunID)-2",
            reason: .verification_findings,
            first_observed_at: date(1_800),
            last_observed_at: date(2_100))
        return [
            .init(
                as_of_revision: 12, as_of: date(2_100), run_id: activeRunID,
                milestones: activeMilestones,
                hold: .init(value1: hold),
                invocations: [
                    .init(
                        invocation_id: "inv-\(activeRunID)-2", run_id: activeRunID,
                        status: .running, live: true, observed_at: date(2_100))
                ]),
            .init(
                as_of_revision: 12, as_of: date(1_080), run_id: readyRunID,
                milestones: [
                    milestone(.run_submitted, runID: readyRunID, minute: 0),
                    milestone(.invocation_started, runID: readyRunID, minute: 1),
                    milestone(.terminal_recorded, runID: readyRunID, minute: 12),
                    milestone(.publication_ready, runID: readyRunID, minute: 18),
                ],
                invocations: [
                    .init(
                        invocation_id: "inv-\(readyRunID)-1", run_id: readyRunID,
                        status: .completed, live: false, observed_at: date(1_080))
                ]),
            // The legacy run's timeline is empty: no milestones synthesized,
            // matching the daemon's no-backfill projection of an unobserved run.
            .init(
                as_of_revision: 12, as_of: date(0), run_id: legacyRunID,
                milestones: [],
                invocations: []),
        ]
    }

    public static func defaultRunIDs() -> [String] {
        defaultRuns().map(\.run.id)
    }

    private static func snapshot(
        id: String,
        projectID: String,
        stage: String,
        attempt: Int,
        milestone: Components.Schemas.RunMilestoneKind?,
        outcome: Components.Schemas.RunOutcome,
        hold: Components.Schemas.RunHoldReason? = nil
    ) -> Components.Schemas.RunSnapshot {
        let stageID = "stage-\(id)"
        return .init(
            as_of_revision: 12,
            entity_version: 1,
            run: .init(
                id: id,
                project_id: projectID,
                spec_digest: "sha256:\(String(repeating: "1", count: 64))",
                policy_digest: "sha256:\(String(repeating: "2", count: 64))",
                stages: [
                    .init(
                        id: stageID, run_id: id, name: stage,
                        attempts: (1...attempt).map {
                            .init(
                                id: "attempt-\(id)-\($0)", stage_id: stageID,
                                number: $0, invocation_id: "inv-\(id)-\($0)")
                        })
                ],
                latest_milestone: milestone.map { .init(value1: $0) },
                outcome: outcome,
                hold_reason: hold.map { .init(value1: $0) }))
    }

    private static func schedule(
        id: String,
        kind: Components.Schemas.ScheduleKind,
        runID: String,
        fireAt: Date? = nil,
        createdAt: Date,
        baseWatch: Components.Schemas.Schedule.base_watchPayload? = nil
    ) -> Components.Schemas.ScheduleSnapshot {
        .init(
            as_of_revision: 12,
            entity_version: 1,
            schedule: .init(
                id: id,
                project_id: "freeside",
                kind: kind,
                subject: .init(
                    _type: .attention_item, item_id: "item-\(runID)", item_version: 1),
                run_id: runID,
                policy_digest: "sha256:\(String(repeating: "2", count: 64))",
                generation: 1,
                created_at: createdAt,
                fire_at: fireAt,
                base_watch: baseWatch,
                status: .armed))
    }

    private static func milestone(
        _ kind: Components.Schemas.RunMilestoneKind,
        runID: String,
        attempt: Int = 1,
        minute: Int
    ) -> Components.Schemas.RunMilestone {
        let terminal: Components.Schemas.RunMilestone.terminalPayload? =
            kind == .terminal_recorded ? .init(value1: .completed) : nil
        return .init(
            run_id: runID,
            kind: kind,
            invocation_id: "inv-\(runID)-\(attempt)",
            terminal: terminal,
            recorded_at: date(TimeInterval(minute * 60)))
    }

    private static func date(_ offset: TimeInterval) -> Date {
        Date(timeIntervalSince1970: 1_786_502_400 + offset)
    }
}
