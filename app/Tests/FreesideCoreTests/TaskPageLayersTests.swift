import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

@MainActor
@Suite struct TaskPageLayersTests {
    private let utc = TimeZone.gmt
    private let enUS = Locale(identifier: "en_US")

    @Test func sectionSummariesCountRunsAndNameTheStart() throws {
        let history = TaskHistoryFixtures.history(.revised)
        #expect(history.sections.count == 2)
        let earlier = try #require(history.sections.last)
        let start = try #require(TaskTimelinePresentation.sectionStart(earlier))
        let now = start.addingTimeInterval(86_400)
        let on = FreesideFormat.shortDate(start, now: now, locale: enUS, timeZone: utc)
        let runs = earlier.runs.count == 1 ? "1 run" : "\(earlier.runs.count) runs"
        // An earlier campaign says how many runs and when, never why it
        // ended: no recorded fact carries that.
        let earlierSummary = TaskTimelinePresentation.sectionSummary(
            earlier, now: now, locale: enUS, timeZone: utc)
        #expect(earlierSummary == "\(runs) · \(on)")
        #expect(!earlierSummary.contains("superseded"))
    }

    @Test func aSectionStartsAtItsAllocationElseItsEarliestMilestone() throws {
        var section = try #require(TaskHistoryFixtures.history(.retry).sections.first)
        let milestones = section.runs.flatMap(\.milestones).map(\.recorded_at)
        section.events.removeAll { $0.kind == .campaign_allocated }
        for index in section.runs.indices {
            section.runs[index].events.removeAll { $0.kind == .campaign_allocated }
        }
        #expect(TaskTimelinePresentation.sectionStart(section) == milestones.min())
        var empty = section
        empty.runs = []
        empty.events = []
        #expect(TaskTimelinePresentation.sectionStart(empty) == nil)
        #expect(TaskTimelinePresentation.sectionSummary(empty, now: .now) == "0 runs")
    }

    @Test func runDetailsSummaryJoinsRoleReasonAndHold() throws {
        let runs = TaskHistoryFixtures.history(.retry).sections.flatMap(\.runs)
        let retried = try #require(runs.first { $0.attempt_reason != nil })
        let summary = try #require(TaskTimelinePresentation.runDetailsSummary(retried))
        let reason = try #require(retried.attempt_reason)
        #expect(summary.contains(reason))
        if let role = retried.role?.value1 {
            #expect(summary.hasPrefix(TaskTimelinePresentation.label(role)))
        }
        if let hold = retried.hold?.value1 {
            #expect(summary.hasSuffix("hold: \(RunDisplay.label(hold.reason))"))
        }
        var bare = retried
        bare.role = nil
        bare.attempt_reason = nil
        bare.hold = nil
        bare.parent_run_id = nil
        bare.superseded_by = nil
        #expect(TaskTimelinePresentation.runDetailsSummary(bare) == nil)
        #expect(!TaskTimelinePresentation.hasRunDetails(bare))
        bare.parent_run_id = "run-parent"
        #expect(TaskTimelinePresentation.hasRunDetails(bare))
    }

    /// The run the task stands on repeats the task's chip; every other run
    /// is history, so faint, and says only what is known.
    @Test func runChipRepeatsTheTaskForItsCurrentRunAndIsFaintOtherwise() throws {
        let fixture = TaskProgressFixtures.make("Verification")
        let currentID = try #require(fixture.task.current_position?.value1.run_id)
        var run = try #require(
            TaskHistoryFixtures.history(.retry).sections.flatMap(\.runs).first)
        run.run_id = currentID
        run.superseded_by = nil
        let chip = try #require(
            TaskTimelinePresentation.runChip(run, task: fixture.task, position: fixture.position, synced: nil))
        #expect(chip.label == fixture.position?.status)
        #expect(chip.cut == TaskDisplay.statusCut(fixture.task, position: fixture.position))

        run.run_id = "run-some-earlier-attempt"
        #expect(
            TaskTimelinePresentation.runChip(run, task: fixture.task, position: fixture.position, synced: nil)
                == nil)
        run.superseded_by = currentID
        let superseded = try #require(
            TaskTimelinePresentation.runChip(run, task: fixture.task, position: fixture.position, synced: nil))
        #expect(superseded.label == "Superseded")
        #expect(superseded.cut == .faint)
        var failed = try #require(fixture.runs.first).run
        failed.outcome = .failed
        let known = try #require(
            TaskTimelinePresentation.runChip(run, task: fixture.task, position: fixture.position, synced: failed))
        #expect(known.label == "\(RunDisplay.label(Components.Schemas.RunOutcome.failed)) · Superseded")
        #expect(known.cut == .faint)
    }

    /// A run something ended never reads as in progress: the daemon leaves
    /// a handed-off specification run and an abandoned task's runs
    /// unconcluded, so the chip drops their pending outcome (#1462).
    @Test func runChipNeverCallsAnEndedRunInProgress() throws {
        var fixture = TaskProgressFixtures.make("Verification")
        let currentID = try #require(fixture.task.current_position?.value1.run_id)
        var pending = try #require(fixture.runs.first).run
        pending.outcome = .pending
        func chip(_ run: Components.Schemas.TaskTimelineRun, synced: Components.Schemas.Run? = pending) -> String? {
            TaskTimelinePresentation.runChip(run, task: fixture.task, position: fixture.position, synced: synced)?
                .label
        }

        // A specification run its implementation run superseded handed off.
        var specification = Components.Schemas.TaskTimelineRun(
            run_id: "run-spec", role: .init(value1: .specification), attempt_number: 1,
            superseded_by: currentID, milestones: [], events: [])
        let handedOff = try #require(
            TaskTimelinePresentation.runChip(
                specification, task: fixture.task, position: fixture.position, synced: pending))
        #expect(handedOff.label == "Handed Off")
        #expect(handedOff.cut == .faint)

        // A superseded implementation run with no final outcome.
        var implementation = Components.Schemas.TaskTimelineRun(
            run_id: "run-impl-1", role: .init(value1: .implementation), attempt_number: 1,
            superseded_by: currentID, milestones: [], events: [])
        #expect(chip(implementation) == "Superseded")
        var unobserved = pending
        unobserved.outcome = .unobserved
        #expect(chip(implementation, synced: unobserved) == "Superseded")

        // With no successor, a live task's earlier run keeps its outcome; an
        // ended task's shows none.
        implementation.superseded_by = nil
        specification.superseded_by = nil
        #expect(chip(implementation) == RunDisplay.label(Components.Schemas.RunOutcome.pending))
        for lifecycle in [Components.Schemas.Task.lifecyclePayload.abandoned, .stopped, .finished] {
            fixture.task.lifecycle = lifecycle
            #expect(chip(implementation) == nil)
            #expect(chip(specification) == nil)
        }

        // A final outcome stays.
        var failed = pending
        failed.outcome = .failed
        implementation.superseded_by = currentID
        #expect(
            chip(implementation, synced: failed)
                == "\(RunDisplay.label(Components.Schemas.RunOutcome.failed)) · Superseded")
    }

    @Test func headerMetaLineIsProjectIssueLifecycleSource() throws {
        let retry = try #require(
            TaskFixtures.defaultTasks().first { $0.task.id == TaskFixtures.retryTaskID }
        ).task
        #expect(
            TaskTimelinePresentation.headerMetaLine(retry)
                == "freeside · #724 · \(TaskDisplay.lifecycleLabel(retry).text) · Source: freeside-ai/freeside#724")
        let legacy = try #require(
            TaskFixtures.defaultTasks().first { $0.task.id == TaskFixtures.legacyTaskID }
        ).task
        #expect(!TaskTimelinePresentation.headerMetaLine(legacy).contains("#"))
        #expect(TaskTimelinePresentation.headerMetaLine(legacy).hasSuffix("Source: none recorded"))
    }

    @Test func foldsPersistPerTaskAndStartCollapsed() throws {
        let suite = "TaskPageLayersTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }

        let first = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(!first.isExpanded(.campaign("campaign-1"), taskID: "task-a"))
        first.setExpanded(true, .campaign("campaign-1"), taskID: "task-a")
        first.binding(.run("run-1"), taskID: "task-a").wrappedValue = true
        first.setExpanded(true, .roundFacts("review-1"), taskID: "task-b")

        let reloaded = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(reloaded.isExpanded(.campaign("campaign-1"), taskID: "task-a"))
        #expect(reloaded.isExpanded(.run("run-1"), taskID: "task-a"))
        // One task's folds never open another's, and a run's fold is not
        // its details fold.
        #expect(!reloaded.isExpanded(.campaign("campaign-1"), taskID: "task-b"))
        #expect(!reloaded.isExpanded(.runDetails("run-1"), taskID: "task-a"))
        #expect(reloaded.isExpanded(.roundFacts("review-1"), taskID: "task-b"))

        // Closing the last open fold drops the task's entry entirely.
        reloaded.setExpanded(false, .roundFacts("review-1"), taskID: "task-b")
        let stored = defaults.dictionary(forKey: "FreesideTaskTimelineExpandedSections")
        #expect(stored?["task-b"] == nil)
        #expect(stored?["task-a"] != nil)
    }

    /// Acceptance 6 (#2611): the Task fold is gone and the current
    /// campaign never folds: a device that stored either open keeps the
    /// value, and the page reads neither, while an earlier campaign's
    /// stored fold still opens it.
    @Test func aStoredCurrentCampaignFoldIsIgnoredAndAnEarlierOneKept() throws {
        let page = TaskStageFixtures.page(.secondCampaign)
        let history = try #require(page.history)
        let currentCampaign = try #require(history.sections.first?.campaign_id)
        let earlierCampaign = try #require(history.sections.last?.campaign_id)
        let suite = "TaskPageLayersTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set(
            [page.task.id: ["task", "campaign:\(currentCampaign)", "campaign:\(earlierCampaign)"]],
            forKey: "FreesideTaskTimelineExpandedSections")

        // The page asks only after the earlier campaigns' folds: the
        // current campaign's stored state is never read, and the earlier
        // campaign's opens as it was left.
        let store = TaskTimelineDisclosurePreferences(defaults: defaults)
        let folded = TaskTimelinePresentation.earlierSections(page.task, in: history)
        #expect(folded.map(\.campaign_id) == [earlierCampaign])
        #expect(folded.allSatisfy { store.isExpanded(.campaign($0.campaign_id), taskID: page.task.id) })
    }

    /// The Task Events fold is gone: a device that stored it open keeps the
    /// value, and it opens nothing.
    @Test func aStoredFoldForTheOldEventsDisclosureOpensNothing() throws {
        let suite = "TaskPageLayersTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set(["task-a": ["events"]], forKey: "FreesideTaskTimelineExpandedSections")

        let store = TaskTimelineDisclosurePreferences(defaults: defaults)
        for section in [
            TaskTimelineDisclosurePreferences.Section.campaign(nil), .campaign("events"), .run("events"),
            .runDetails("events"), .roundFacts("events"), .priorRounds("events"),
        ] {
            #expect(!store.isExpanded(section, taskID: "task-a"))
        }
    }

    /// The Identifiers footer persists under its own stable key, starts
    /// closed, and is no other fold.
    @Test func theIdentifiersFoldHasItsOwnKey() throws {
        #expect(TaskTimelineDisclosurePreferences.Section.identifiers.id == "identifiers")
        let others: [TaskTimelineDisclosurePreferences.Section] = [
            .campaign(nil), .campaign("identifiers"), .run("identifiers"), .runDetails("identifiers"),
            .roundFacts("identifiers"), .priorRounds("identifiers"),
        ]
        #expect(!others.map(\.id).contains("identifiers"))

        let suite = "TaskPageLayersTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(!store.isExpanded(.identifiers, taskID: "task-a"))
        store.setExpanded(true, .identifiers, taskID: "task-a")
        let reloaded = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(reloaded.isExpanded(.identifiers, taskID: "task-a"))
        #expect(!reloaded.isExpanded(.identifiers, taskID: "task-b"))
        #expect(others.allSatisfy { !reloaded.isExpanded($0, taskID: "task-a") })
    }

    /// The head's Attachments fold persists under its own key, starts
    /// closed, opens and closes per task, and is no other fold.
    @Test func theAttachmentsFoldHasItsOwnKey() throws {
        #expect(TaskTimelineDisclosurePreferences.Section.attachments.id == "attachments")
        let others: [TaskTimelineDisclosurePreferences.Section] = [
            .campaign(nil), .campaign("attachments"), .run("attachments"), .runDetails("attachments"),
            .roundFacts("attachments"), .priorRounds("attachments"), .identifiers,
        ]
        #expect(!others.map(\.id).contains("attachments"))

        let suite = "TaskPageLayersTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(!store.isExpanded(.attachments, taskID: "task-a"))
        store.setExpanded(true, .attachments, taskID: "task-a")
        let reloaded = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(reloaded.isExpanded(.attachments, taskID: "task-a"))
        #expect(!reloaded.isExpanded(.attachments, taskID: "task-b"))
        #expect(others.allSatisfy { !reloaded.isExpanded($0, taskID: "task-a") })
        reloaded.setExpanded(false, .attachments, taskID: "task-a")
        #expect(!TaskTimelineDisclosurePreferences(defaults: defaults).isExpanded(.attachments, taskID: "task-a"))
    }

    /// Each earlier thread's fold persists under its conversation id,
    /// starts closed, opens and closes per task, and shares no state with
    /// another thread's fold.
    @Test func eachEarlierThreadKeepsItsOwnFold() throws {
        let suite = "TaskPageLayersTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let store = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(!store.isExpanded(.conversation("conv-a"), taskID: "task-a"))
        store.setExpanded(true, .conversation("conv-a"), taskID: "task-a")
        let reloaded = TaskTimelineDisclosurePreferences(defaults: defaults)
        #expect(reloaded.isExpanded(.conversation("conv-a"), taskID: "task-a"))
        #expect(!reloaded.isExpanded(.conversation("conv-b"), taskID: "task-a"))
        #expect(!reloaded.isExpanded(.conversation("conv-a"), taskID: "task-b"))
        #expect(!reloaded.isExpanded(.attachments, taskID: "task-a"))
        reloaded.setExpanded(false, .conversation("conv-a"), taskID: "task-a")
        #expect(
            !TaskTimelineDisclosurePreferences(defaults: defaults).isExpanded(.conversation("conv-a"), taskID: "task-a")
        )
    }

    @Test func aMemoryOnlyStoreWritesNothing() {
        let store = TaskTimelineDisclosurePreferences(defaults: nil)
        store.setExpanded(true, .campaign("campaign-1"), taskID: "task-a")
        #expect(store.isExpanded(.campaign("campaign-1"), taskID: "task-a"))
        #expect(
            !TaskTimelineDisclosurePreferences(defaults: nil).isExpanded(.campaign("campaign-1"), taskID: "task-a"))
    }

    /// A new campaign is prepended, so a campaign's position changes while
    /// its id does not: the fold follows the id, and the legacy section with
    /// no campaign has its own stable key.
    @Test func aCampaignFoldFollowsItsIdNotItsPosition() throws {
        var history = TaskHistoryFixtures.history(.revised)
        let earlier = try #require(history.sections.last)
        let store = TaskTimelineDisclosurePreferences(defaults: nil)
        store.setExpanded(true, .campaign(earlier.campaign_id), taskID: history.task_id)

        var newest = try #require(history.sections.first)
        newest.campaign_id = "campaign-newest"
        history.sections.insert(newest, at: 0)
        let open = history.sections.filter { store.isExpanded(.campaign($0.campaign_id), taskID: history.task_id) }
        #expect(open.map(\.campaign_id) == [earlier.campaign_id])
        #expect(
            TaskTimelineDisclosurePreferences.Section.campaign(nil).id
                != TaskTimelineDisclosurePreferences.Section.campaign("outside-campaign").id)
    }

    /// Acceptance 1 (#2611): an active task's Current Run holds its current
    /// run at every stage, the run's line, its Review only once a round
    /// exists, and the current campaign's other runs as earlier attempts.
    @Test(arguments: [
        TaskStageFixtures.Scenario.specification, .implementation, .review, .verification, .retry, .returned,
        .remediation,
    ])
    func currentRunHoldsTheTasksRunAtEachStage(scenario: TaskStageFixtures.Scenario) throws {
        var page = TaskStageFixtures.page(scenario)
        page.task.task_lines = [
            TaskStageFixtures.line(.specifier, agent: "codex", pool: "S"),
            TaskStageFixtures.line(.implementer, agent: "claude", pool: "A"),
        ]
        let history = try #require(page.history)
        let runTimelines = Dictionary(
            uniqueKeysWithValues: TaskStageFixtures.script(scenario).runTimelines.map { ($0.run_id, $0) })
        let current = try #require(
            TaskTimelinePresentation.currentRunSection(
                page.task, timeline: history, runTimelines: runTimelines, schedules: []))

        #expect(current.run.run_id == page.task.current_position?.value1.run_id)
        let specifies = scenario == .specification
        #expect(current.run.role?.value1 == (specifies ? .specification : .implementation))
        #expect(current.line == (specifies ? "codex · pool S" : "claude · pool A"))
        #expect(current.earlierAttempts.map(\.run_id) == history.sections[0].runs.dropFirst().map(\.run_id))
        #expect(current.schedules.isEmpty)

        // Review draws only once the run's timeline holds a round, and
        // never for a specification run. The scripts record rounds only
        // for a decided review, so the review stage gains its Review once
        // its round is in the run's timeline, below.
        let recorded = runTimelines[current.run.run_id]?.review?.value1.rounds.isEmpty == false
        #expect(current.showsReview == (recorded && !specifies))
        #expect(recorded == (scenario == .remediation))
        let reviewed = Components.Schemas.RunTimeline(
            as_of_revision: 1, as_of: RunFixtures.screenshotInstant, run_id: current.run.run_id, milestones: [],
            invocations: [], review: .init(value1: .init(rounds: [RunFixtures.reviewRound(.running)])),
            admissions: [], outcomes: RunFixtures.noOutcomes)
        let withRound = try #require(
            TaskTimelinePresentation.currentRunSection(
                page.task, timeline: history, runTimelines: [current.run.run_id: reviewed], schedules: []))
        #expect(withRound.showsReview == !specifies)
    }

    /// A finished task, or a history not yet loaded, has no Current Run.
    @Test func aFinishedTaskHasNoCurrentRun() throws {
        var page = TaskStageFixtures.page(.verification)
        let history = try #require(page.history)
        #expect(
            TaskTimelinePresentation.currentRunSection(page.task, timeline: nil, runTimelines: [:], schedules: [])
                == nil)
        for lifecycle in [Components.Schemas.Task.lifecyclePayload.finished, .stopped, .abandoned] {
            page.task.lifecycle = lifecycle
            #expect(
                TaskTimelinePresentation.currentRunSection(
                    page.task, timeline: history, runTimelines: [:], schedules: []) == nil)
        }
        // Its page draws the rail, then the earlier campaigns, with no
        // run of the newest drawn as a card.
        let newest = try #require(history.sections.first).runs.map { TaskTimelinePresentation.Entry.run($0.run_id) }
        #expect(
            !TaskTimelinePresentation.entries(history, task: page.task).contains {
                newest.contains($0)
            })
    }

    /// Acceptance 3 (#2611): the Timeline has no fold around the current
    /// campaign, so it draws open with nothing stored; each earlier
    /// campaign folds below the rail, closed until opened, by its id.
    @Test func onlyEarlierCampaignsFoldAndEachIsRememberedByItsId() throws {
        let page = TaskStageFixtures.page(.secondCampaign)
        let history = try #require(page.history)
        #expect(history.sections.count == 2)
        let earlier = TaskTimelinePresentation.earlierSections(page.task, in: history)
        #expect(earlier.map(\.campaign_id) == [history.sections[1].campaign_id])

        let store = TaskTimelineDisclosurePreferences(defaults: nil)
        #expect(!store.isExpanded(.campaign(earlier[0].campaign_id), taskID: page.task.id))
        store.setExpanded(true, .campaign(earlier[0].campaign_id), taskID: page.task.id)
        #expect(store.isExpanded(.campaign(earlier[0].campaign_id), taskID: page.task.id))

        // The current campaign is the section holding the current run,
        // wherever it sits, else the history's first.
        var moved = page.task
        moved.current_position?.value1.run_id = try #require(history.sections[1].runs.first?.run_id)
        #expect(
            TaskTimelinePresentation.earlierSections(moved, in: history).map(\.campaign_id)
                == [history.sections[0].campaign_id])
        moved.current_position = nil
        #expect(
            TaskTimelinePresentation.earlierSections(moved, in: history).map(\.campaign_id)
                == [history.sections[1].campaign_id])
    }

    /// Acceptance 5 (#2611): the task's armed schedules sit in Current
    /// Run's Run Details, whose summary names the soonest to fire.
    @Test func runDetailsCarryTheTasksArmedSchedules() throws {
        let page = TaskStageFixtures.page(.implementation)
        let runID = try #require(page.task.current_position?.value1.run_id)
        var checks = try #require(RunFixtures.defaultSchedules().first { $0.schedule.kind == .pr_checks_deadline })
        checks.schedule.run_id = runID
        var later = checks
        later.schedule.id = "schedule-later"
        later.schedule.kind = .review_wait_threshold
        later.schedule.fire_at = checks.schedule.fire_at?.addingTimeInterval(3_600)
        var fired = checks
        fired.schedule.id = "schedule-fired"
        fired.schedule.status = .fired
        var elsewhere = checks
        elsewhere.schedule.id = "schedule-elsewhere"
        elsewhere.schedule.run_id = "run-of-another-task"

        let current = try #require(
            TaskTimelinePresentation.currentRunSection(
                page.task, timeline: page.history, runTimelines: [:], schedules: [later, fired, checks, elsewhere]))
        #expect(current.schedules.map(\.schedule.id) == ["schedule-later", checks.schedule.id])
        let fireAt = try #require(checks.schedule.fire_at)
        let time = FreesideFormat.shortTime(fireAt, now: fireAt, locale: enUS, timeZone: utc)
        let summary = try #require(
            TaskTimelinePresentation.runDetailsSummary(
                current.run, schedules: current.schedules, now: fireAt, locale: enUS, timeZone: utc))
        #expect(summary.hasSuffix("next checks \(time)"))

        var bare = current.run
        bare.role = nil
        bare.attempt_reason = nil
        bare.hold = nil
        bare.parent_run_id = nil
        bare.superseded_by = nil
        #expect(!TaskTimelinePresentation.hasRunDetails(bare))
        #expect(TaskTimelinePresentation.hasRunDetails(bare, schedules: current.schedules))
    }

    /// Acceptance 7 (#2611): the Runs chip goes to the run's card where the
    /// page draws one, opening the folds around it, else to its run page.
    @Test func theRunsChipGoesWhereTheRunIsDrawn() throws {
        let page = TaskStageFixtures.page(.secondCampaign)
        let history = try #require(page.history)
        let currentID = try #require(page.task.current_position?.value1.run_id)
        let target = { TaskTimelinePresentation.runTarget($0, task: page.task, timeline: history) }
        #expect(target(currentID) == .currentRun)
        let earlierCampaign = try #require(history.sections[1].runs.first)
        #expect(target(earlierCampaign.run_id) == .earlierCampaign(history.sections[1].campaign_id))
        #expect(target("run-not-in-the-history") == .runPage)

        let implementation = TaskStageFixtures.page(.retry)
        let attempts = try #require(implementation.history?.sections.first?.runs)
        #expect(attempts.count > 1)
        #expect(
            TaskTimelinePresentation.runTarget(
                attempts[1].run_id, task: implementation.task, timeline: try #require(implementation.history))
                == .earlierAttempt)

        // A finished task draws no card for its newest campaign's runs.
        var finished = page.task
        finished.lifecycle = .finished
        #expect(TaskTimelinePresentation.runTarget(currentID, task: finished, timeline: history) == .runPage)
        #expect(
            TaskTimelinePresentation.runTarget(earlierCampaign.run_id, task: finished, timeline: history)
                == .earlierCampaign(history.sections[1].campaign_id))
    }

    /// The guidance link leads to the very item whose lookup produced it.
    @Test func inboxGuidanceCarriesTheItemItNames() throws {
        let approval = TaskProgressFixtures.make("Approval required")
        let position = try #require(approval.position)
        #expect(position.attention)
        #expect(position.attentionItemID != nil)
        let held = try #require(TaskProgressFixtures.make("Verification").position)
        #expect(!held.attention && held.attentionItemID == nil)
    }
}
