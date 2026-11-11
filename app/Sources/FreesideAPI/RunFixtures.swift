import Foundation

/// Deterministic daemon-observation fixtures for the runs list and timeline screens.
/// They contain no agent-authored text, matching the production wire shape.
public enum RunFixtures {
    public static let activeRunID = "run-freeside-657"
    public static let readyRunID = "run-freeside-654"
    public static let legacyRunID = "run-freeside-540"
    public static let completedRunID = "run-freeside-640"
    public static let refreshedRunID = "run-freeside-refreshed"
    public static let specificationHistoryRunID = "run-freeside-specification-history"
    public static let retryCampaignID =
        "campaign-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

    /// The demo tasks' names, carried on their runs' `display_names` the way
    /// the daemon copies a task's current name onto its runs. A task's name
    /// source is never `name` (daemon/internal/domain/task.go): an operator
    /// named the retry and completed tasks, the agent proposed the ready
    /// task's name (its claim stays labeled in the clients), and the
    /// approved specification supplied the specification task's. The legacy
    /// and oriole runs carry no names, so their tasks fall back to the
    /// identifier.
    public static let retryTaskName = Components.Schemas.DisplayName(
        text: "Repair the acceptance rig", source: ._operator)
    public static let readyTaskName = Components.Schemas.DisplayName(
        text: "Summarize review rounds on the card", source: .agent)
    public static let specificationTaskName = Components.Schemas.DisplayName(
        text: "Time grammar for inbox rows", source: .specification)
    public static let completedTaskName = Components.Schemas.DisplayName(
        text: "Drain failed startups on launch", source: ._operator)
    public static let refreshedTaskName = Components.Schemas.DisplayName(
        text: "Order invocation history by attempt", source: ._operator)

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
                campaignID: retryCampaignID,
                campaignAttempt: 2, attemptReason: "Retry after repairing the acceptance rig",
                parentRunID: "run-freeside-656", cost: activeCost, taskName: retryTaskName),
            snapshot(
                id: readyRunID, projectID: "freeside", stage: "implementation",
                attempt: 1, milestone: .publication_ready, outcome: .published,
                lifecycle: .active, campaignID: "campaign-freeside-ready", campaignAttempt: 1,
                taskName: readyTaskName),
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
                campaignID: retryCampaignID,
                campaignAttempt: 1,
                supersededBy: activeRunID, taskName: retryTaskName),
            snapshot(
                id: "run-freeside-specification", projectID: "freeside", stage: "specification",
                attempt: 1, milestone: .execution_export_recorded, outcome: .pending,
                lifecycle: .active, taskName: specificationTaskName),
            completedRun(),
        ].map { projectingObservationTimes($0, from: timelines[$0.run.id]) }
    }

    /// A completed run (#1134): the work unit's PR merged and closed its
    /// bound issue, so the outcome is completed, the lifecycle finished, and
    /// the summary carries the completion facts and the spend figure.
    public static func completedRun() -> Components.Schemas.RunSnapshot {
        projectingObservationTimes(
            snapshot(
                id: completedRunID, projectID: "freeside", stage: "implementation",
                attempt: 1, milestone: .work_unit_completed, outcome: .completed,
                lifecycle: .finished, campaignID: "campaign-freeside-completed", campaignAttempt: 1,
                completion: completedFacts, cost: completedCost, taskName: completedTaskName),
            from: completedTimeline())
    }

    /// An approved specification handed to implementation in the same attempt.
    /// The daemon concludes it `handed_off` (#3043).
    /// Kept separate from the default mock scenarios for focused presentation tests.
    public static func handedOffSpecificationRun() -> Components.Schemas.RunSnapshot {
        snapshot(
            id: "run-freeside-specification-bound", projectID: "freeside", stage: "specification",
            attempt: 1, milestone: .run_submitted, outcome: .handed_off,
            lifecycle: .finished, campaignID: "campaign-freeside-ready", campaignAttempt: 1,
            supersededBy: readyRunID, taskName: readyTaskName)
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
            lifecycle: .active, taskName: refreshedTaskName)
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
            billable_cost_so_far: nil,
            admissions: [
                writerAdmission("inv-\(refreshedRunID)-1", at: date(60)),
                writerAdmission("inv-\(refreshedRunID)-2", at: date(1_200)),
                writerAdmission(remediationInvocation, role: .remediator, at: date(2_400)),
                reviewerAdmission(
                    round.invocation_id, at: date(3_600),
                    implementer: .differed, remediator: .differed),
            ])
    }

    /// The purposes the specification-history run's attempts record, in
    /// attempt order (#3044): research continued twice, so that purpose
    /// repeats.
    public static let specificationHistoryPurposes: [Components.Schemas.AttemptPurpose] = [
        .first_draft, .research_continuation, .research_continuation, .discussion_reply, .revision,
    ]

    /// A specification run whose invocations each record why they ran
    /// (#3044). Kept out of `defaultRuns()` and `defaultTimelines()`, as
    /// `refreshedHistoryRun()` is, so existing screenshot digests do not
    /// churn.
    public static func specificationHistoryRun() -> Components.Schemas.RunSnapshot {
        let id = specificationHistoryRunID
        var snapshot = snapshot(
            id: id, projectID: "freeside", stage: "specification",
            attempt: specificationHistoryPurposes.count, milestone: .terminal_recorded, outcome: .pending,
            lifecycle: .active, taskName: specificationTaskName)
        for index in snapshot.run.stages[0].attempts.indices {
            snapshot.run.stages[0].attempts[index].purpose = .init(value1: specificationHistoryPurposes[index])
        }
        return projectingObservationTimes(snapshot, from: specificationHistoryTimeline())
    }

    public static func specificationHistoryTimeline() -> Components.Schemas.RunTimeline {
        let id = specificationHistoryRunID
        let attempts = 1...specificationHistoryPurposes.count
        return .init(
            as_of_revision: 12, as_of: date(TimeInterval(specificationHistoryPurposes.count * 600 + 120)), run_id: id,
            // The submission names no invocation, so it carries no attempt
            // label.
            milestones: [.init(run_id: id, kind: .run_submitted, invocation_id: nil, recorded_at: date(0))]
                + attempts.flatMap { attempt in
                    [
                        milestone(.invocation_admitted, runID: id, attempt: attempt, minute: attempt * 10 - 9),
                        milestone(.invocation_started, runID: id, attempt: attempt, minute: attempt * 10 - 8),
                    ]
                },
            invocations: attempts.map { attempt in
                .init(
                    invocation_id: "inv-\(id)-\(attempt)", run_id: id,
                    status: .completed, live: false, observed_at: date(TimeInterval(attempt * 600)))
            },
            billable_cost_so_far: nil,
            admissions: attempts.map { attempt in
                writerAdmission(
                    "inv-\(id)-\(attempt)", role: .specifier, at: date(TimeInterval(attempt * 600 - 540)))
            })
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
            billable_cost_so_far: .init(value1: completedCost),
            admissions: [writerAdmission("inv-\(completedRunID)-1", at: date(60))])
    }

    /// What ran a writing invocation, as its execution admission recorded
    /// it: Claude Code under the operator's subscription.
    public static func writerAdmission(
        _ invocationID: String, role: Components.Schemas.RoleName = .implementer, at admittedAt: Date
    ) -> Components.Schemas.InvocationAdmissionFacts {
        .init(
            invocation_id: invocationID, kind: .execution, role: .init(value1: role),
            admitted_at: admittedAt,
            agent_digest: .init(value1: "sha256:" + String(repeating: "c", count: 64)),
            selection_source: .init(value1: .lineup), harness: .init(value1: .claude_code),
            model: "claude-opus-5-5", requested_effort: .init(value1: .high), native_effort: "high",
            enrollment_id: "anthropic-a/claude_code", selection_model_id: "claude-opus-5-5",
            selection_effort: "high",
            selection_digest: .init(value1: "sha256:" + String(repeating: "e", count: 64)),
            cost_owner: "platform-team", lineage_group: "anthropic",
            credential_mode: .init(value1: .subscription_contained),
            egress_profile: .init(value1: .provider_only), operating_mode: .init(value1: .unattended),
            isolation_class: "fresh_vm_read_only_volume_handoff")
    }

    /// What ran a review, as its review admission recorded it: Codex, whose
    /// lineage is compared against each writing role's. A review admission
    /// records no credential mode, egress, operating mode, or isolation.
    public static func reviewerAdmission(
        _ invocationID: String, at admittedAt: Date,
        implementer: Components.Schemas.LineageRelation, remediator: Components.Schemas.LineageRelation
    ) -> Components.Schemas.InvocationAdmissionFacts {
        .init(
            invocation_id: invocationID, kind: .review, role: .init(value1: .reviewer),
            admitted_at: admittedAt,
            agent_digest: .init(value1: "sha256:" + String(repeating: "d", count: 64)),
            selection_source: .init(value1: .lineup), harness: .init(value1: .codex_cli),
            model: "gpt-5.2-codex", requested_effort: .init(value1: .high), native_effort: "high",
            enrollment_id: "openai-a/codex_cli", selection_model_id: "gpt-5.2-codex",
            selection_effort: "high",
            selection_digest: .init(value1: "sha256:" + String(repeating: "f", count: 64)),
            cost_owner: "platform-team", lineage_group: "openai",
            independence: [
                .init(writing_role: .implementer, lineage: implementer),
                .init(writing_role: .remediator, lineage: remediator),
            ])
    }

    /// An execution admission recorded before admissions carried an agent
    /// binding: every agent fact is null and renders as unavailable.
    public static func legacyAdmission(
        _ invocationID: String, at admittedAt: Date
    ) -> Components.Schemas.InvocationAdmissionFacts {
        .init(
            invocation_id: invocationID, kind: .execution, admitted_at: admittedAt,
            credential_mode: .init(value1: .subscription_contained),
            egress_profile: .init(value1: .provider_only), operating_mode: .init(value1: .attended_dev),
            isolation_class: "fresh_vm_read_only_volume_handoff")
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
        ]
    }

    /// A round as the daemon reports it in `state`. The request time and the
    /// reviewer identity default to what the daemon sends today: a fixed
    /// instant, and an identity only once the round has completed. A caller
    /// passes them to place a round relative to its own clock, or to model a
    /// daemon that names the reviewer while the round still runs.
    public static func reviewRound(
        _ state: Components.Schemas.ReviewProgressState,
        round: Int = 1, findings: Bool = false,
        availability: Components.Schemas.ReviewEvidenceAvailability = .unavailable,
        requestedAt: Date? = nil, provider: String? = nil, modelConfiguration: String? = nil
    ) -> Components.Schemas.RunReviewRound {
        let requested = requestedAt ?? date(1_800)
        return .init(
            round: round, invocation_id: "review-\(activeRunID)-\(round)",
            state: state, source: .init(kind: "freeside_invoked"),
            base_sha: String(repeating: "a", count: 40),
            head_sha: String(repeating: round == 1 ? "b" : "c", count: 40),
            requested_at: requested,
            completed_at: state == .completed || state == .failed ? requested.addingTimeInterval(120) : nil,
            provider: provider ?? (state == .completed ? "openai" : nil),
            model_configuration: modelConfiguration ?? (state == .completed ? "codex/high" : nil),
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
            ? #"{"findings":[{"severity":"P1","location":{"path":"Sources/Cache.swift","start_line":42,"end_line":45},"explanation":"A failed write clears the last saved value. Keep the previous value until `save(_:)` returns."},{"severity":"P2","location":{"path":"Tests/CacheTests.swift","start_line":18,"end_line":22},"explanation":"Cover the failed-write path so a later change cannot silently lose the saved value."},{"severity":"P3","location":{"path":"README.md","whole_file":true},"explanation":"Document whether a failed refresh keeps the last saved value."}]}"#
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
            detail: nil,
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
                billable_cost_so_far: .init(value1: activeCost),
                admissions: [
                    writerAdmission("inv-\(activeRunID)-1", at: date(60)),
                    writerAdmission("inv-\(activeRunID)-2", at: date(1_200)),
                    reviewerAdmission(
                        "review-\(activeRunID)-1", at: date(1_800),
                        implementer: .differed, remediator: .unknown),
                    reviewerAdmission(
                        "review-\(activeRunID)-2", at: date(1_800),
                        implementer: .differed, remediator: .unknown),
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
                ],
                completion: nil,
                billable_cost_so_far: nil,
                admissions: [writerAdmission("inv-\(readyRunID)-1", at: date(60))]),
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
                billable_cost_so_far: nil,
                admissions: [legacyAdmission("inv-run-oriole-121-1", at: date(60))]),
            // The legacy run's timeline is empty: no milestones synthesized,
            // matching the daemon's no-backfill projection of an unobserved run.
            .init(
                as_of_revision: 12, as_of: date(0), run_id: legacyRunID,
                milestones: [],
                invocations: [],
                completion: nil,
                billable_cost_so_far: nil,
                admissions: []),
            .init(
                as_of_revision: 12, as_of: date(900), run_id: "run-freeside-656",
                milestones: [
                    milestone(.run_submitted, runID: "run-freeside-656", minute: 0),
                    milestone(.terminal_recorded, runID: "run-freeside-656", minute: 15),
                ],
                invocations: [], completion: nil, billable_cost_so_far: nil, admissions: []),
            .init(
                as_of_revision: 12, as_of: date(300), run_id: "run-freeside-specification",
                milestones: [
                    milestone(.run_submitted, runID: "run-freeside-specification", minute: 0),
                    milestone(.execution_export_recorded, runID: "run-freeside-specification", minute: 5),
                ],
                invocations: [], completion: nil, billable_cost_so_far: nil, admissions: []),
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
        holdUsagePool: String? = nil,
        campaignID: String? = nil,
        campaignAttempt: Int? = nil,
        attemptReason: String? = nil,
        parentRunID: String? = nil,
        supersededBy: String? = nil,
        completion: Components.Schemas.WorkUnitCompletionFacts? = nil,
        cost: Components.Schemas.CostSoFar? = nil,
        taskName: Components.Schemas.DisplayName? = nil
    ) -> Components.Schemas.RunSnapshot {
        let stageID = "stage-\(id)"
        return .init(
            as_of_revision: 12,
            entity_version: 1,
            run: .init(
                id: id,
                project_id: projectID,
                task_id: "task-\(campaignID ?? id)",
                display_names: taskName.map {
                    .init(
                        value1: .init(
                            project: .init(text: projectID, source: .name),
                            task: $0))
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
                hold_usage_pool: holdUsagePool,
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
        createdAt: Date
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
