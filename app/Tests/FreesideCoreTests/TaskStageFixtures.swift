import Foundation
import FreesideAPI

@testable import FreesideCore
@testable import FreesideModel

/// Task histories in the daemon's order, for the event rail. Each one is
/// written oldest first, as it happened, and handed over newest first, as
/// the daemon sends it. Where two events share an instant the daemon puts
/// the one whose kind sorts first on top, and the script is written to
/// match: a specification's approval sits under the submission of the run
/// it starts.
enum TaskStageFixtures {
    enum Scenario: CaseIterable {
        /// A specification run that has started and nothing more.
        case specification
        /// An approved specification and a first implementation attempt.
        case implementation
        /// The first attempt's work under review.
        case review
        /// One campaign through a clean review to a blocked publication.
        case verification
        /// A failed attempt and a second one, with no review between.
        case retry
        /// A second attempt after a review found something.
        case returned
        /// A second campaign whose specification run has started.
        case secondCampaign
        /// A run the daemon recorded without a role or an attempt number.
        case noRole
        /// The first review's findings sent to a remediator that has
        /// started, with one finding declined.
        case remediation
    }

    static let taskID = "stage-task"

    struct Script {
        private(set) var sections: [Components.Schemas.TaskTimelineSection] = []
        /// Each run's review rounds by run id, as its run timeline holds
        /// them: the review facts the task's events do not carry.
        private(set) var reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
        private var oldestFirst: [Components.Schemas.TaskEvent] = []
        private var minute = -240

        private var now: Date { RunFixtures.screenshotInstant.addingTimeInterval(TimeInterval(minute * 60)) }

        /// Moves the clock on. Events written without a wait between them
        /// share an instant.
        mutating func wait(_ minutes: Int = 7) { minute += minutes }

        mutating func event(_ kind: Components.Schemas.TaskEventKind, run: String? = nil, pr: Int? = nil) {
            oldestFirst.append(.init(kind: kind, recorded_at: now, run_id: run, pr_number: pr))
        }

        mutating func campaign(_ id: String, specification run: String) {
            sections.insert(.init(campaign_id: id, events: [], runs: []), at: 0)
            section(
                id,
                .init(kind: .campaign_allocated, recorded_at: now, campaign_id: id, specification_run_id: run))
        }

        mutating func approve(_ campaign: String, specification: String, implementation: String) {
            section(
                campaign,
                .init(
                    kind: .specification_approved, recorded_at: now, campaign_id: campaign,
                    run_id: implementation,
                    approved_spec_digest: .init(value1: "sha256:" + String(repeating: "c", count: 64)),
                    specification_run_id: specification))
        }

        /// A run's submission and, a minute on, the start of its first
        /// invocation.
        mutating func submit(
            _ run: String, role: Components.Schemas.TaskRunRole?, attempt: Int?, campaign: String?
        ) {
            if !sections.contains(where: { $0.campaign_id == campaign }) {
                sections.insert(.init(campaign_id: campaign, events: [], runs: []), at: 0)
            }
            let member = Components.Schemas.TaskTimelineRun(
                run_id: run, role: role.map { .init(value1: $0) }, attempt_number: attempt, milestones: [],
                events: [])
            for index in sections.indices where sections[index].campaign_id == campaign {
                sections[index].runs.insert(member, at: 0)
            }
            milestone(run, .run_submitted)
            wait(1)
            milestone(run, .invocation_started, invocation: "\(run)-invocation-1")
        }

        mutating func milestone(
            _ run: String, _ kind: Components.Schemas.RunMilestoneKind, invocation: String? = nil,
            terminal: Components.Schemas.ObservedInvocationStatus? = nil,
            hold: Components.Schemas.RunHoldReason? = nil
        ) {
            let milestone = Components.Schemas.RunMilestone(
                run_id: run, kind: kind, invocation_id: invocation,
                terminal: terminal.map { .init(value1: $0) }, reason: hold.map { .init(value1: $0) },
                recorded_at: now)
            for index in sections.indices {
                guard let member = sections[index].runs.firstIndex(where: { $0.run_id == run }) else { continue }
                sections[index].runs[member].milestones.insert(milestone, at: 0)
            }
            oldestFirst.append(
                .init(milestone: .init(value1: milestone), kind: .run_milestone, recorded_at: now, run_id: run))
        }

        /// A work result: the run's `pass`th invocation ending.
        mutating func result(
            _ run: String, _ terminal: Components.Schemas.ObservedInvocationStatus, pass: Int = 1
        ) {
            milestone(run, .terminal_recorded, invocation: "\(run)-invocation-\(pass)", terminal: terminal)
        }

        mutating func review(
            _ run: String, round: Int, _ kind: Components.Schemas.TaskEventKind,
            outcome: Components.Schemas.ReviewOutcome? = nil
        ) {
            oldestFirst.append(
                .init(
                    review: .init(
                        value1: .init(
                            invocation_id: "\(run)-review-\(round)", round: round,
                            head_sha: String(repeating: "b", count: 40), base_sha: String(repeating: "a", count: 40),
                            outcome: outcome.map { .init(value1: $0) },
                            failure: kind == .review_failed ? "reviewer_unavailable" : nil)),
                    kind: kind, recorded_at: now, run_id: run))
        }

        /// The decision on a completed round's findings, now: `remediate` of
        /// them go to the invocation `remediator` and `declined` are
        /// declined. A round whose every finding is declined has no
        /// remediator and no remediation record. The round's times are its
        /// review events'.
        mutating func decide(
            _ run: String, round: Int, remediate: Int, declined: Int, remediator: String? = nil
        ) {
            func recorded(_ kind: Components.Schemas.TaskEventKind) -> Date? {
                oldestFirst.last { $0.kind == kind && $0.run_id == run && $0.review?.value1.round == round }?
                    .recorded_at
            }
            var record = RunFixtures.reviewRound(.completed, round: round, findings: true)
            record.invocation_id = "\(run)-review-\(round)"
            record.requested_at = recorded(.review_requested)
            record.completed_at = recorded(.review_completed)
            record.findings_count = remediate + declined
            record.dispositions = .init(value1: .init(fixed: 0, declined: declined, deferred: 0, open: remediate))
            record.remediation = remediator.map {
                .init(
                    value1: .init(
                        invocation_id: $0, finding_ids: (0..<remediate).map { "finding-\($0 + 1)" },
                        decided_at: now))
            }
            reviewRounds[run, default: []].append(record)
        }

        mutating func verify(_ run: String, clean: Bool) {
            oldestFirst.append(
                .init(
                    verification: .init(
                        value1: .init(
                            item_id: "\(run)-ready", _class: clean ? .ready_clean : .ready_degraded,
                            head_sha: String(repeating: "b", count: 40),
                            base_sha: String(repeating: "a", count: 40))),
                    kind: .verification_recorded, recorded_at: now, run_id: run))
        }

        private mutating func section(_ campaign: String, _ event: Components.Schemas.TaskEvent) {
            if let index = sections.firstIndex(where: { $0.campaign_id == campaign }) {
                sections[index].events.insert(event, at: 0)
            }
            oldestFirst.append(event)
        }

        var timeline: Components.Schemas.TaskTimeline {
            .init(
                as_of_revision: 12, as_of: RunFixtures.screenshotInstant, task_id: TaskStageFixtures.taskID,
                project_id: "freeside",
                name: .init(text: "Update session handling across desktop and mobile clients", source: ._operator),
                events: oldestFirst.reversed(), sections: sections)
        }

        /// The run timelines a client caches for the runs with review
        /// rounds, milestones oldest first.
        var runTimelines: [Components.Schemas.RunTimeline] {
            sections.flatMap(\.runs).compactMap { run in
                reviewRounds[run.run_id].map {
                    .init(
                        as_of_revision: 12, as_of: RunFixtures.screenshotInstant, run_id: run.run_id,
                        milestones: run.milestones.reversed(), invocations: [],
                        review: .init(value1: .init(rounds: $0)))
                }
            }
        }
    }

    static let specificationRun = "stage-spec-1"
    static let implementationRun = "stage-implementation-1"
    static let secondImplementationRun = "stage-implementation-2"
    static let campaign = "stage-campaign-1"
    static let remediator = "\(implementationRun)-remediation-1"

    /// The task and its current run as a client holds them beside
    /// `history(scenario)`: the run is working, with no hold, except that a
    /// task at Verification is held on its findings.
    static func page(_ scenario: Scenario) -> TaskProgressFixtures.Case {
        page(history(scenario), heldOnFindings: scenario == .verification)
    }

    /// `page(_:)` for any history whose newest section's newest run is the
    /// current one.
    static func page(
        _ history: Components.Schemas.TaskTimeline, heldOnFindings: Bool = false
    ) -> TaskProgressFixtures.Case {
        guard let current = history.sections.first?.runs.first else {
            preconditionFailure("A stage fixture's history names its current run")
        }
        var page = TaskProgressFixtures.make("Implementation")
        var run = page.runs[current.role?.value1 == .specification ? 1 : 0]
        run.run.id = current.run_id
        run.run.task_id = history.task_id
        run.run.campaign_id = history.sections[0].campaign_id
        run.run.lifecycle = .active
        run.run.superseded_by = nil
        run.run.outcome = .pending
        if heldOnFindings { run.run.hold_reason = .init(value1: .verification_findings) }
        page.runs = [run]
        page.history = history
        page.task.id = history.task_id
        page.task.display_names.task = history.name
        page.task.campaign_ids = history.sections.compactMap(\.campaign_id).reversed()
        page.task.run_ids = history.sections.flatMap(\.runs).map(\.run_id).reversed()
        page.task.current_position?.value1.run_id = current.run_id
        page.task.current_position?.value1.stage = run.run.stages.last?.name
        return page
    }

    /// A coordinator holding `page` and `runTimelines` as a confirmed-fresh
    /// sync would, and the task's snapshot, for drawing the whole task page.
    @MainActor
    static func coordinator(
        _ page: TaskProgressFixtures.Case, runTimelines: [Components.Schemas.RunTimeline] = []
    ) throws -> (coordinator: SyncCoordinator, task: Components.Schemas.TaskSnapshot) {
        var task = TaskFixtures.approvedCampaign().task
        task.task = page.task
        let cache = InMemoryCacheStore()
        try cache.save(
            .init(
                cursors: .init(
                    syncEpoch: "stage-epoch", lastFullSnapshotRevision: task.as_of_revision,
                    highestObservedServerRevision: task.as_of_revision),
                attentionItems: page.items, runs: page.runs, runTimelines: runTimelines, tasks: [task],
                taskTimelines: [page.history].compactMap { $0 }))
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: MockServer()), cache: cache)
        coordinator.store.freshness = .fresh
        return (coordinator, task)
    }

    static func history(_ scenario: Scenario) -> Components.Schemas.TaskTimeline {
        script(scenario).timeline
    }

    static func script(_ scenario: Scenario) -> Script {
        var script = Script()
        script.event(.task_created)
        script.wait(2)
        if scenario == .noRole {
            script.event(.task_started, run: "stage-legacy-1")
            script.submit("stage-legacy-1", role: nil, attempt: nil, campaign: nil)
            script.wait()
            script.result("stage-legacy-1", .completed)
            script.wait()
            script.review("stage-legacy-1", round: 1, .review_requested)
            return script
        }
        script.event(.task_started, run: specificationRun)
        script.campaign(campaign, specification: specificationRun)
        script.wait(1)
        script.submit(specificationRun, role: .specification, attempt: 1, campaign: campaign)
        if scenario == .specification { return script }
        script.wait(18)
        script.result(specificationRun, .completed)
        script.wait(25)
        script.approve(campaign, specification: specificationRun, implementation: implementationRun)
        script.submit(implementationRun, role: .implementation, attempt: 1, campaign: campaign)
        if scenario == .implementation { return script }
        script.wait(31)
        switch scenario {
        case .retry:
            script.result(implementationRun, .failed)
            script.wait()
            script.submit(secondImplementationRun, role: .implementation, attempt: 2, campaign: campaign)
            return script
        case .secondCampaign:
            script.campaign("stage-campaign-2", specification: "stage-spec-2")
            script.wait(1)
            script.submit("stage-spec-2", role: .specification, attempt: 1, campaign: "stage-campaign-2")
            return script
        default:
            break
        }
        script.review(implementationRun, round: 1, .review_requested)
        if scenario == .review { return script }
        script.wait(9)
        if scenario == .remediation {
            script.review(implementationRun, round: 1, .review_completed, outcome: .findings)
            script.wait(4)
            script.decide(implementationRun, round: 1, remediate: 2, declined: 1, remediator: remediator)
            script.wait(2)
            script.milestone(implementationRun, .invocation_started, invocation: remediator)
            return script
        }
        if scenario == .returned {
            script.review(implementationRun, round: 1, .review_failed)
            script.wait(2)
            script.review(implementationRun, round: 2, .review_requested)
            script.wait(9)
            // The round's own invocation ends, which the round's entry
            // already says.
            script.milestone(
                implementationRun, .terminal_recorded, invocation: "\(implementationRun)-review-2",
                terminal: .completed)
            script.review(implementationRun, round: 2, .review_completed, outcome: .findings)
            script.wait(4)
            // The daemon records the first pass's result after the review of
            // its work was requested, and a fix-up pass in the same attempt
            // records a second one.
            script.result(implementationRun, .completed)
            script.milestone(implementationRun, .invocation_started, invocation: "\(implementationRun)-invocation-2")
            script.wait(12)
            script.result(implementationRun, .completed, pass: 2)
            script.wait(3)
            script.submit(secondImplementationRun, role: .implementation, attempt: 2, campaign: campaign)
            return script
        }
        script.review(implementationRun, round: 1, .review_completed, outcome: .clean)
        script.wait(6)
        script.verify(implementationRun, clean: false)
        script.wait(2)
        // The daemon records the attempt's result when the publication
        // cycle closes, after the review of its work.
        script.milestone(implementationRun, .publication_blocked, hold: .verification_findings)
        script.result(implementationRun, .completed)
        return script
    }
}
