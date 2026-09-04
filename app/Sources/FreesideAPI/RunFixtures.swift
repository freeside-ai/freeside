import Foundation

/// Deterministic daemon-observation fixtures for the runs list and timeline screens.
/// They contain no agent-authored text, matching the production wire shape.
public enum RunFixtures {
    public static let activeRunID = "run-freeside-657"
    public static let readyRunID = "run-freeside-654"
    public static let legacyRunID = "run-freeside-540"
    public static let completedRunID = "run-freeside-640"

    public static func defaultRuns() -> [Components.Schemas.RunSnapshot] {
        let timelines = Dictionary(
            uniqueKeysWithValues: defaultTimelines().map { ($0.run_id, $0) })
        return [
            snapshot(
                id: activeRunID, projectID: "freeside", stage: "implementation",
                attempt: 2, milestone: .invocation_started, outcome: .pending,
                lifecycle: .active, hold: .verification_findings,
                campaignID: "campaign-freeside-acceptance",
                campaignAttempt: 2, attemptReason: "Retry after repairing the acceptance rig",
                parentRunID: "run-freeside-656", cost: activeCost, workUnit: "#724"),
            snapshot(
                id: readyRunID, projectID: "freeside", stage: "publication",
                attempt: 1, milestone: .publication_ready, outcome: .published,
                lifecycle: .active, campaignID: "campaign-freeside-ready", campaignAttempt: 1,
                workUnit: "#654"),
            snapshot(
                id: "run-oriole-121", projectID: "oriole", stage: "verification",
                attempt: 1, milestone: .terminal_recorded, outcome: .failed, lifecycle: .finished),
            // A pre-migration-0024 legacy run: structural stages but no
            // observation milestones, so an unobserved outcome and no timeline.
            snapshot(
                id: legacyRunID, projectID: "freeside", stage: "implementation",
                attempt: 1, milestone: nil, outcome: .unobserved, lifecycle: .finished),
            snapshot(
                id: "run-freeside-656", projectID: "freeside", stage: "implementation",
                attempt: 1, milestone: .terminal_recorded, outcome: .failed, lifecycle: .finished,
                campaignID: "campaign-freeside-acceptance", campaignAttempt: 1,
                supersededBy: activeRunID, workUnit: "#724"),
            snapshot(
                id: "run-freeside-specification", projectID: "freeside", stage: "specification",
                attempt: 1, milestone: .execution_export_recorded, outcome: .pending,
                lifecycle: .active, workUnit: "#724"),
            completedRun(),
        ].map { projectingObservationTimes($0, from: timelines[$0.run.id]) }
    }

    /// A completed run (#1134): the work unit's PR merged and closed its
    /// bound issue, so the outcome is completed, the lifecycle finished, and
    /// the summary carries the completion facts and the spend figure.
    public static func completedRun() -> Components.Schemas.RunSnapshot {
        projectingObservationTimes(
            snapshot(
                id: completedRunID, projectID: "freeside", stage: "publication",
                attempt: 1, milestone: .work_unit_completed, outcome: .completed,
                lifecycle: .finished, campaignID: "campaign-freeside-completed", campaignAttempt: 1,
                completion: completedFacts, cost: completedCost, workUnit: "#80"),
            from: completedTimeline())
    }

    public static func completedTimeline() -> Components.Schemas.RunTimeline {
        .init(
            as_of_revision: 12, as_of: date(2_400), run_id: completedRunID,
            milestones: [
                milestone(.run_submitted, runID: completedRunID, minute: 0),
                milestone(.invocation_started, runID: completedRunID, minute: 1),
                milestone(.terminal_recorded, runID: completedRunID, minute: 12),
                milestone(.publication_ready, runID: completedRunID, minute: 18),
                milestone(.work_unit_completed, runID: completedRunID, minute: 40),
            ],
            invocations: [
                .init(
                    invocation_id: "inv-\(completedRunID)-1", run_id: completedRunID,
                    status: .completed, live: false, observed_at: date(720))
            ],
            completion: .init(value1: completedFacts),
            billable_cost_so_far: .init(value1: completedCost))
    }

    private static let completedFacts = Components.Schemas.WorkUnitCompletionFacts(
        pr_number: 105, merge_commit_sha: String(repeating: "5", count: 40),
        bound_issue: 80, recorded_at: date(2_400))
    private static let completedCost = Components.Schemas.CostSoFar(
        currency: "USD", amount: "23.75", invocations: 2, complete: true)
    private static let activeCost = Components.Schemas.CostSoFar(
        currency: "USD", amount: "8.5", invocations: 1, complete: false)

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
                ],
                completion: nil,
                billable_cost_so_far: .init(value1: activeCost)),
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
                ],
                completion: nil,
                billable_cost_so_far: nil),
            .init(
                as_of_revision: 12, as_of: date(600), run_id: "run-oriole-121",
                milestones: [
                    milestone(.run_submitted, runID: "run-oriole-121", minute: 0),
                    milestone(.invocation_started, runID: "run-oriole-121", minute: 1),
                    milestone(.terminal_recorded, runID: "run-oriole-121", minute: 10),
                ],
                invocations: [
                    .init(
                        invocation_id: "inv-run-oriole-121-1", run_id: "run-oriole-121",
                        status: .failed, live: false, observed_at: date(600))
                ],
                completion: nil,
                billable_cost_so_far: nil),
            // The legacy run's timeline is empty: no milestones synthesized,
            // matching the daemon's no-backfill projection of an unobserved run.
            .init(
                as_of_revision: 12, as_of: date(0), run_id: legacyRunID,
                milestones: [],
                invocations: [],
                completion: nil,
                billable_cost_so_far: nil),
            .init(
                as_of_revision: 12, as_of: date(900), run_id: "run-freeside-656",
                milestones: [
                    milestone(.run_submitted, runID: "run-freeside-656", minute: 0),
                    milestone(.terminal_recorded, runID: "run-freeside-656", minute: 15),
                ],
                invocations: [], completion: nil, billable_cost_so_far: nil),
            .init(
                as_of_revision: 12, as_of: date(300), run_id: "run-freeside-specification",
                milestones: [
                    milestone(.run_submitted, runID: "run-freeside-specification", minute: 0),
                    milestone(.execution_export_recorded, runID: "run-freeside-specification", minute: 5),
                ],
                invocations: [], completion: nil, billable_cost_so_far: nil),
            completedTimeline(),
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
        lifecycle: Components.Schemas.RunLifecycle,
        hold: Components.Schemas.RunHoldReason? = nil,
        campaignID: String? = nil,
        campaignAttempt: Int? = nil,
        attemptReason: String? = nil,
        parentRunID: String? = nil,
        supersededBy: String? = nil,
        completion: Components.Schemas.WorkUnitCompletionFacts? = nil,
        cost: Components.Schemas.CostSoFar? = nil,
        workUnit: String? = nil
    ) -> Components.Schemas.RunSnapshot {
        let stageID = "stage-\(id)"
        return .init(
            as_of_revision: 12,
            entity_version: 1,
            run: .init(
                id: id,
                project_id: projectID,
                display_names: workUnit.map {
                    .init(
                        value1: .init(
                            project: .init(text: projectID, source: .name),
                            work_unit: .init(text: $0, source: .name)))
                },
                created_at: nil,
                last_activity_at: nil,
                spec_digest: "sha256:\(String(repeating: "1", count: 64))",
                policy_digest: "sha256:\(String(repeating: "2", count: 64))",
                campaign_id: campaignID,
                attempt_number: campaignAttempt,
                attempt_reason: attemptReason,
                parent_run_id: parentRunID,
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
                hold_reason: hold.map { .init(value1: $0) },
                lifecycle: lifecycle,
                superseded_by: supersededBy,
                completion: completion.map { .init(value1: $0) },
                billable_cost_so_far: cost.map { .init(value1: $0) }))
    }

    static func projectingObservationTimes(
        _ snapshot: Components.Schemas.RunSnapshot,
        from timeline: Components.Schemas.RunTimeline?
    ) -> Components.Schemas.RunSnapshot {
        var projected = snapshot
        let milestones = timeline?.milestones ?? []
        projected.run.created_at = milestones.first { $0.kind == .run_submitted }?.recorded_at

        var activity = milestones.map(\.recorded_at)
        activity.append(contentsOf: timeline?.invocations.map(\.observed_at) ?? [])
        if let hold = timeline?.hold?.value1 {
            activity.append(hold.last_observed_at)
        }
        projected.run.last_activity_at = activity.max()
        return projected
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
