import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The task page's strip across a task's runs (#3234), on the fixture
/// task: a specification, a first attempt that stopped on a question, an
/// hour and a quarter with no run, and a retry held for machine capacity
/// and now waiting on the final review.
@MainActor
@Suite struct TaskTimeStripViewTests {
    private let utc = TimeZone.gmt
    private let enUS = Locale(identifier: "en_US")

    private func strip() throws -> TaskTimeStrip.Strip {
        let fixture = TaskFixtures.taskStrip()
        return try #require(
            TaskTimeStrip.strip(
                runs: TaskTimelinePresentation.timeStripRuns(
                    fixture.history, task: fixture.task.task, synced: fixture.runs),
                attentionItems: AttentionFixtures.taskStripItems(), now: RunFixtures.taskStripNow))
    }

    @Test func theSummaryNamesTheElapsedTimeTheRunsAndEachTotal() throws {
        #expect(
            TaskTimeStripView.summary(try strip(), locale: enUS)
                == "Elapsed 4 hours, 50 minutes across 3 runs: agent working 2 hours, 3 minutes, "
                + "waiting on you 57 minutes, held 20 minutes, other 15 minutes, between runs 1 hour, 15 minutes.")
    }

    @Test func aSingleRunsSummaryNamesNoTimeBetweenRuns() throws {
        let fixture = TaskFixtures.taskStrip()
        let retry = try #require(
            TaskTimelinePresentation.timeStripRuns(fixture.history, task: fixture.task.task, synced: fixture.runs)
                .first { $0.runID == RunFixtures.taskStripRetryRunID })
        let strip = try #require(
            TaskTimeStrip.strip(
                runs: [retry], attentionItems: AttentionFixtures.taskStripItems(), now: RunFixtures.taskStripNow))
        #expect(
            TaskTimeStripView.summary(strip, locale: enUS)
                == "Elapsed 1 hour, 15 minutes across 1 run: agent working 24 minutes, "
                + "waiting on you 26 minutes, held 20 minutes, other 5 minutes.")
    }

    /// Each run's part opens that run and says its title, length, and span;
    /// the time between runs opens nothing.
    @Test func eachRunsPartOpensItsRunAndBetweenRunsOpensNothing() throws {
        let fixture = TaskFixtures.taskStrip()
        let titles = Dictionary(
            uniqueKeysWithValues: fixture.history.sections.flatMap(\.runs).map {
                ($0.run_id, TaskTimelinePresentation.runTitle($0))
            })
        let items = TaskTimeStripView.items(
            try strip(), titles: titles, now: RunFixtures.taskStripNow, locale: enUS, timeZone: utc)
        #expect(
            items.map(\.opensRunID) == [
                RunFixtures.taskStripSpecificationRunID, RunFixtures.taskStripFirstRunID, nil,
                RunFixtures.taskStripRetryRunID,
            ])
        #expect(
            items.map(\.label) == [
                "Specification, 31 minutes, Aug 12, 2:40\u{202F}AM to Aug 12, 3:11\u{202F}AM",
                "Implementation · Attempt 1, 1 hour, 49 minutes, Aug 12, 3:11\u{202F}AM to Aug 12, 5:00\u{202F}AM",
                "Between runs, 1 hour, 15 minutes, Aug 12, 5:00\u{202F}AM to Aug 12, 6:15\u{202F}AM",
                "Implementation · Attempt 2, 1 hour, 15 minutes, Aug 12, 6:15\u{202F}AM to Aug 12, 7:30\u{202F}AM",
            ])
    }

    /// Served clocks decide by their end, over the synced lifecycle and the
    /// run chip's rule (#3237).
    @Test func aRunWithServedClocksIsFinishedByTheirEnd() throws {
        let fixture = TaskFixtures.taskStrip()
        let task = fixture.task.task
        let runs = fixture.history.sections.flatMap(\.runs)
        var retry = try #require(runs.first { $0.run_id == RunFixtures.taskStripRetryRunID })
        var first = try #require(runs.first { $0.run_id == RunFixtures.taskStripFirstRunID })
        var synced = try #require(fixture.runs.first { $0.run.id == retry.run_id }).run
        synced.lifecycle = .finished
        #expect(!TaskTimelinePresentation.runIsFinished(retry, task: task, synced: synced))
        #expect(TaskTimelinePresentation.runIsFinished(first, task: task, synced: nil))

        retry.clocks?.value1.ended_at = RunFixtures.taskStripInstant(minute: 262)
        #expect(TaskTimelinePresentation.runIsFinished(retry, task: task, synced: nil))
        first.clocks?.value1.ended_at = nil
        #expect(!TaskTimelinePresentation.runIsFinished(first, task: task, synced: nil))
    }

    /// From a daemon that serves no clocks, the synced run's lifecycle
    /// decides when the client holds it; without it, the run chip's rule
    /// does: a successor, or the task no longer active, ended the run.
    @Test func aRunIsFinishedByItsSyncedLifecycleElseTheRunChipsRule() throws {
        let fixture = TaskFixtures.taskStrip()
        let task = fixture.task.task
        let runs = fixture.history.sections.flatMap(\.runs)
        var retry = try #require(runs.first { $0.run_id == RunFixtures.taskStripRetryRunID })
        var first = try #require(runs.first { $0.run_id == RunFixtures.taskStripFirstRunID })
        retry.clocks = nil
        first.clocks = nil
        var synced = try #require(fixture.runs.first { $0.run.id == retry.run_id }).run

        // The synced lifecycle wins over the fallback in both directions.
        #expect(!TaskTimelinePresentation.runIsFinished(retry, task: task, synced: synced))
        synced.lifecycle = .finished
        #expect(TaskTimelinePresentation.runIsFinished(retry, task: task, synced: synced))
        var supersededButActive = synced
        supersededButActive.lifecycle = .active
        #expect(!TaskTimelinePresentation.runIsFinished(first, task: task, synced: supersededButActive))

        // An unknown lifecycle, or no synced run, falls back.
        synced.lifecycle = .unknown
        #expect(!TaskTimelinePresentation.runIsFinished(retry, task: task, synced: synced))
        #expect(!TaskTimelinePresentation.runIsFinished(retry, task: task, synced: nil))
        #expect(TaskTimelinePresentation.runIsFinished(first, task: task, synced: nil))
        let stopped = TaskFixtures.confirmedStopped(fixture.task).task
        #expect(TaskTimelinePresentation.runIsFinished(retry, task: stopped, synced: nil))

        #expect(
            TaskTimelinePresentation.timeStripRuns(fixture.history, task: task, synced: []).map(\.finished)
                == TaskTimelinePresentation.timeStripRuns(fixture.history, task: task, synced: fixture.runs)
                .map(\.finished))
    }
}
