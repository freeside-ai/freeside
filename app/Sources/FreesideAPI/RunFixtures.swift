import Foundation

/// Deterministic daemon-observation fixtures for the runs list and timeline screens.
/// They contain no agent-authored text, matching the production wire shape.
public enum RunFixtures {
    public static let activeRunID = "run-freeside-657"
    public static let readyRunID = "run-freeside-654"
    public static let legacyRunID = "run-freeside-540"
    public static let completedRunID = "run-freeside-640"
    public static let refreshedRunID = "run-freeside-refreshed"

    /// A fixed capture clock: five minutes past the newest fixture
    /// observation, so run cards render their relative last-active text
    /// deterministically instead of against the wall clock.
    public static let screenshotInstant = date(2_700)

    public static func defaultRuns() -> [Components.Schemas.RunSnapshot] {
        let timelines = Dictionary(
            uniqueKeysWithValues: defaultTimelines().map { ($0.run_id, $0) })
        return [
            snapshot(
                id: activeRunID, projectID: "freeside", stage: "implementation",
                attempt: 2, milestone: .invocation_started, outcome: .pending,
                lifecycle: .active, hold: .verification_findings,
                campaignID: "campaign-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
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
                campaignID: "campaign-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
                campaignAttempt: 1,
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

    /// An approved specification handed to implementation in the same attempt.
    /// Kept separate from the default mock scenarios for focused presentation tests.
    public static func handedOffSpecificationRun() -> Components.Schemas.RunSnapshot {
        snapshot(
            id: "run-freeside-specification-bound", projectID: "freeside", stage: "specification",
            attempt: 1, milestone: .run_submitted, outcome: .pending,
            lifecycle: .finished, campaignID: "campaign-freeside-ready", campaignAttempt: 1,
            supersededBy: readyRunID, workUnit: "#654")
    }

    /// A run that exercises attempt-ordered invocation history (#1263): two
    /// early implementation attempts re-observed long after a later remediation
    /// attempt completed, plus one review round. Kept out of `defaultRuns()`
    /// and `defaultTimelines()` so the runs-list screenshot digests do not
    /// churn.
    public static func refreshedHistoryRun() -> Components.Schemas.RunSnapshot {
        var snapshot = snapshot(
            id: refreshedRunID, projectID: "freeside", stage: "implement",
            attempt: 2, milestone: .publication_ready, outcome: .pending,
            lifecycle: .active, workUnit: "#1263")
        // The completed attempt is the daemon's remediation `implement` stage,
        // appended after the first two attempts (creation order), so it holds
        // the newest attempt position under one Implementation heading.
        let remediationStageID = "stage-\(refreshedRunID)-remediation"
        snapshot.run.stages.append(
            .init(
                id: remediationStageID, run_id: refreshedRunID, name: "implement",
                attempts: [
                    .init(
                        id: "attempt-\(refreshedRunID)-remediation-1", stage_id: remediationStageID,
                        number: 1, invocation_id: "inv-\(refreshedRunID)-remediation-1")
                ]))
        return projectingObservationTimes(snapshot, from: refreshedHistoryTimeline())
    }

    public static func refreshedHistoryTimeline() -> Components.Schemas.RunTimeline {
        let remediationInvocation = "inv-\(refreshedRunID)-remediation-1"
        var round = reviewRound(.completed)
        round.invocation_id = "review-\(refreshedRunID)-1"
        round.requested_at = date(3_600)
        round.completed_at = date(3_720)
        return .init(
            as_of_revision: 12, as_of: date(5_100), run_id: refreshedRunID,
            milestones: [
                milestone(.run_submitted, runID: refreshedRunID, minute: 0),
                milestone(.invocation_admitted, runID: refreshedRunID, attempt: 1, minute: 1),
                milestone(.invocation_started, runID: refreshedRunID, attempt: 1, minute: 2),
                milestone(.invocation_admitted, runID: refreshedRunID, attempt: 2, minute: 20),
                milestone(.invocation_started, runID: refreshedRunID, attempt: 2, minute: 21),
                .init(
                    run_id: refreshedRunID, kind: .invocation_admitted,
                    invocation_id: remediationInvocation, recorded_at: date(2_400)),
                .init(
                    run_id: refreshedRunID, kind: .invocation_started,
                    invocation_id: remediationInvocation, recorded_at: date(2_460)),
                .init(
                    run_id: refreshedRunID, kind: .terminal_recorded,
                    invocation_id: remediationInvocation,
                    terminal: .init(value1: .completed), recorded_at: date(3_000)),
                milestone(.publication_ready, runID: refreshedRunID, minute: 55),
            ],
            invocations: [
                // The two early attempts, re-observed long after the remediation
                // attempt completed: their late observed_at must not lift them
                // above the completed attempt.
                .init(
                    invocation_id: "inv-\(refreshedRunID)-1", run_id: refreshedRunID,
                    status: .failed, live: false, observed_at: date(5_000)),
                .init(
                    invocation_id: "inv-\(refreshedRunID)-2", run_id: refreshedRunID,
                    status: .gone, live: false, observed_at: date(4_000)),
                .init(
                    invocation_id: remediationInvocation, run_id: refreshedRunID,
                    status: .completed, live: false, observed_at: date(3_050)),
                .init(
                    invocation_id: round.invocation_id, run_id: refreshedRunID,
                    status: .completed, live: false, observed_at: date(3_700)),
            ],
            review: .init(value1: .init(rounds: [round])),
            billable_cost_so_far: nil)
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

    public static func reviewRound(
        _ state: Components.Schemas.ReviewProgressState,
        round: Int = 1, findings: Bool = false,
        availability: Components.Schemas.ReviewEvidenceAvailability = .unavailable
    ) -> Components.Schemas.RunReviewRound {
        .init(
            round: round, invocation_id: "review-\(activeRunID)-\(round)",
            state: state, source: .init(kind: "freeside_invoked"),
            base_sha: String(repeating: "a", count: 40),
            head_sha: String(repeating: round == 1 ? "b" : "c", count: 40),
            requested_at: date(1_800),
            completed_at: state == .completed || state == .failed ? date(1_920) : nil,
            provider: state == .completed ? "openai" : nil,
            model_configuration: state == .completed ? "codex/high" : nil,
            outcome: state == .completed ? .init(value1: findings ? .findings : .clean) : nil,
            findings_count: state == .completed ? (findings ? 3 : 0) : nil,
            dispositions: state == .completed
                ? .init(
                    value1: .init(
                        fixed: findings ? 1 : 0, declined: 0, deferred: findings ? 1 : 0, open: findings ? 1 : 0))
                : nil,
            failure: state == .failed
                ? .init(
                    value1: .init(_class: "configuration", reason: "The reviewer could not inspect the bound diff."))
                : nil,
            retry_pending: false,
            evidence: .init(
                completion_evidence: state == .completed
                    ? .init(value1: "sha256:" + String(repeating: "e", count: 64)) : nil,
                availability: availability))
    }

    public static func reviewEvidence(
        runID: String, round: Components.Schemas.RunReviewRound
    ) -> Components.Schemas.ReviewEvidence {
        let findings = round.findings_count == 3
        let result =
            findings
            ? #"{"findings":[{"severity":"P1","location":{"path":"Sources/Cache.swift","start_line":42,"end_line":45},"explanation":"A failed write clears the last saved value. Keep the previous value until the replacement succeeds."},{"severity":"P2","location":{"path":"Tests/CacheTests.swift","start_line":18,"end_line":22},"explanation":"Cover the failed-write path so a later change cannot silently lose the saved value."},{"severity":"P3","location":{"path":"README.md","whole_file":true},"explanation":"Document whether a failed refresh keeps the last saved value."}]}"#
            : #"{"findings":[]}"#
        let message =
            findings
            ? "I inspected the candidate diff and the cache tests. I found a data-loss path and two related gaps. I did not run the tests."
            : "I inspected the candidate diff and traced the cache read and write paths. I found no actionable defects. I did not run the tests; this review does not replace verification."
        let activity = """
            freeside-review-access-v1 base=\(round.base_sha) head=\(round.head_sha) cwd=/workspace
            reviewer startup: using the configured model
            {"type":"thread.started","thread_id":"fixture-review"}
            {"type":"turn.started"}
            {"type":"item.started","item":{"id":"command-1","type":"command_execution","command":"git diff --stat BASE HEAD","aggregated_output":"","exit_code":null,"status":"in_progress"}}
            {"type":"item.completed","item":{"id":"command-1","type":"command_execution","command":"git diff --stat BASE HEAD","aggregated_output":"Sources/Cache.swift | 12 ++++++------\\nTests/CacheTests.swift | 4 ++--","exit_code":0,"status":"completed"}}
            {"type":"item.completed","item":{"id":"message-1","type":"agent_message","text":"\(message)"}}
            {"type":"turn.completed"}
            """
        let diagnostics = """
            reviewer startup: connection interrupted
            {"type":"provider.notice","message":"Additional provider output is not supported by this reader."}
            {"type":"turn.started"}
            {"type":"turn.failed","error":{"message":"The reviewer could not inspect the bound diff."}}
            """
        return .init(
            source: round.source, run_id: runID, round: round.round, invocation_id: round.invocation_id,
            content_kind: .reviewer_output, head_binding: .head_bound, source_head_sha: round.head_sha,
            sensitivity_class: .sensitive, publish_eligible: false, availability: round.evidence.availability,
            events: round.evidence.availability == .available
                ? .init((round.state == .failed ? diagnostics : activity).utf8) : nil,
            result: round.evidence.availability == .available
                ? .init((round.state == .failed ? "" : result).utf8) : nil,
            exit_status: round.evidence.availability == .available ? 0 : nil)
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
                review: .init(
                    value1: .init(rounds: [
                        reviewRound(.completed, round: 1, findings: true),
                        reviewRound(.running, round: 2),
                    ])),
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
                task_id: "task-\(campaignID ?? id)",
                display_names: workUnit.map {
                    .init(
                        value1: .init(
                            project: .init(text: projectID, source: .name),
                            task: .init(text: $0, source: .name)))
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
