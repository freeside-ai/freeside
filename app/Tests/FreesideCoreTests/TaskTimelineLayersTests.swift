import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

@MainActor
@Suite struct TaskTimelineLayersTests {
    private let utc = TimeZone.gmt
    private let enUS = Locale(identifier: "en_US")

    @Test func sectionSummariesCountRunsAndNameTheStart() throws {
        let history = TaskHistoryFixtures.history(.revised)
        #expect(history.sections.count == 2)
        let current = try #require(history.sections.first)
        let earlier = try #require(history.sections.last)
        let start = try #require(TaskTimelinePresentation.sectionStart(current))
        let now = start.addingTimeInterval(86_400)
        let from = FreesideFormat.shortDate(start, now: now, locale: enUS, timeZone: utc)
        let attempts = current.runs.count == 1 ? "1 attempt" : "\(current.runs.count) attempts"
        #expect(
            TaskTimelinePresentation.sectionSummary(
                current, isCurrent: true, now: now, locale: enUS, timeZone: utc)
                == "current · \(attempts) · from \(from)")
        // An earlier campaign says how many runs and when, never why it
        // ended: no recorded fact carries that.
        let earlierSummary = TaskTimelinePresentation.sectionSummary(
            earlier, isCurrent: false, now: now, locale: enUS, timeZone: utc)
        #expect(earlierSummary.hasPrefix(earlier.runs.count == 1 ? "1 run" : "\(earlier.runs.count) runs"))
        #expect(!earlierSummary.contains("current"))
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
        #expect(
            TaskTimelinePresentation.sectionSummary(empty, isCurrent: true, now: .now) == "current · 0 attempts")
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
        let suite = "TaskTimelineLayersTests-\(UUID().uuidString)"
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

    /// The Task Events fold is gone: a device that stored it open keeps the
    /// value, and it opens nothing.
    @Test func aStoredFoldForTheOldEventsDisclosureOpensNothing() throws {
        let suite = "TaskTimelineLayersTests-\(UUID().uuidString)"
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

        let suite = "TaskTimelineLayersTests-\(UUID().uuidString)"
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

    /// A rail row keeps its milestone's instant, which the rail shortens
    /// and offers exactly, in the page's own clock and time zone.
    @Test func milestoneRowsCarryTheirExactInstant() throws {
        let run = try #require(TaskHistoryFixtures.history(.retry).sections.first?.runs.first)
        let rows = TaskTimelinePresentation.milestoneRows(run)
        #expect(rows.map(\.recordedAt) == run.milestones.map { Optional($0.recorded_at) })
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
