import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

/// The Over Time tab's projection (#3242): the classes in the strips'
/// order, the scale, the hold-history and today marks, task names, and the
/// period's dates, which a clock change never shifts.
@Suite struct WorkHoursChartTests {
    private func chart(
        _ hours: Components.Schemas.WorkHours = WorkStatusFixtures.workHoursWeek()
    ) throws -> WorkHoursChart {
        try #require(WorkHoursChart(hours, tasks: WorkStatusFixtures.tasks()))
    }

    private func date(_ text: String) throws -> CalendarDate {
        try #require(CalendarDate(text))
    }

    private func instant(_ text: String) throws -> Date {
        try #require(ISO8601DateFormatter().date(from: text))
    }

    @Test func classesFollowTheStripsOrder() throws {
        let chart = try chart()
        #expect(chart.days.map(\.date.text) == WorkHoursPeriod(endingOn: try date("2026-08-12")).dates.map(\.text))
        let day = try #require(chart.days.first { $0.date.text == "2026-08-10" })
        #expect(day.classes.map(\.kind) == RunTimeStrip.Kind.allCases)
        #expect(day.classes.map(\.seconds) == [7_200, 1_800, 1_200, 600])
        #expect(day.held.map(\.reason) == [.usage_pool_exhausted, .base_advanced])
        #expect(chart.classes.map(\.kind) == RunTimeStrip.Kind.allCases)
        #expect(chart.classes.map(\.seconds).reduce(0, +) == chart.elapsed)
    }

    /// Every bar is drawn to the largest day's elapsed time.
    @Test func scalesToTheLargestDay() throws {
        let chart = try chart()
        #expect(chart.scale == 10_800)
        #expect(chart.scale == chart.days.map(\.elapsed).max())
        #expect(!chart.isEmpty)
    }

    @Test func anEmptyPeriodHasNoScale() throws {
        let chart = try chart(WorkStatusFixtures.workHoursWeek(empty: true))
        #expect(chart.isEmpty && chart.scale == 0 && chart.days.count == WorkHoursPeriod.length)
    }

    /// A day that ended before hold history began has none; a day it began
    /// in names when; a later day is recorded.
    @Test func marksDaysBeforeHoldHistory() throws {
        let since = try instant("2026-08-09T14:00:00Z")
        let chart = try chart(WorkStatusFixtures.workHoursWeek(holdHistorySince: since))
        #expect(
            chart.days.map(\.holdHistory) == [.none, .none, .none, .began(since), .recorded, .recorded, .recorded])
        #expect(chart.startsBeforeHoldHistory)
        // The daemon reads held time before then as other.
        #expect(chart.days[1].classes.map(\.seconds) == [2_400, 0, 0, 7_800])
        // So a day hold history doesn't cover reports no held time.
        #expect(chart.days[1].reportedClasses.map(\.kind) == [.agentWorking, .waitingOnYou, .other])
        #expect(chart.days[3].reportedClasses.map(\.kind) == RunTimeStrip.Kind.allCases)

        let covered = try self.chart()
        #expect(covered.days.allSatisfy { $0.holdHistory == .recorded })
        #expect(!covered.startsBeforeHoldHistory)
    }

    /// A day that starts exactly when hold history began is recorded.
    @Test func aDayStartingAtHoldHistoryIsRecorded() throws {
        let since = try instant("2026-08-10T00:00:00Z")
        let chart = try chart(WorkStatusFixtures.workHoursWeek(holdHistorySince: since))
        #expect(chart.days[3].holdHistory == .none)
        #expect(chart.days[4].holdHistory == .recorded)
    }

    @Test func onlyTodayIsPartial() throws {
        #expect(try chart().days.map(\.isPartial) == [false, false, false, false, false, false, true])
    }

    /// A client clock ahead of the daemon's can ask for a day the daemon
    /// hasn't reached; that day isn't today.
    @Test func aDayThatHasNotBegunIsNotPartial() throws {
        var hours = WorkStatusFixtures.workHoursWeek()
        hours.as_of = try instant("2026-08-11T23:00:00Z")
        #expect(try chart(hours).days.map(\.isPartial) == [false, false, false, false, false, true, false])
    }

    /// A task the client holds takes its display name; any other keeps
    /// only its id.
    @Test func namesTheTasksTheClientHolds() throws {
        let day = try #require(try chart().days.first { $0.date.text == "2026-08-09" })
        #expect(day.tasks.map(\.id) == ["task-card", WorkStatusFixtures.unheldTaskID])
        #expect(day.tasks.map(\.name) == ["Decide the retention window", nil])
    }

    @Test func carriesTheSkippedRunCount() throws {
        #expect(try chart(WorkStatusFixtures.workHoursWeek(skippedRunCount: 3)).skippedRunCount == 3)
        #expect(try chart().skippedRunCount == 0)
    }

    @Test func refusesADayThatIsNotACalendarDate() throws {
        var hours = WorkStatusFixtures.workHoursWeek()
        hours.days[0].date = "2026-8-6"
        #expect(WorkHoursChart(hours, tasks: []) == nil)
    }

    @Test(arguments: ["2026-02-30", "2026-8-12", "2026-08-12T00:00", "", "２０２６-08-12"])
    func refusesWhatIsNotADate(text: String) {
        #expect(CalendarDate(text) == nil)
    }

    /// Berlin's autumn change, on 2026-10-25: the date an instant falls on
    /// follows the zone's offset on each side, and a period across the
    /// change keeps seven consecutive dates.
    @Test func periodDatesHoldAcrossAClockChange() throws {
        let berlin = try #require(TimeZone(identifier: "Europe/Berlin"))
        // 00:30 local, in summer time and then in winter time.
        #expect(CalendarDate(containing: try instant("2026-10-24T22:30:00Z"), in: berlin).text == "2026-10-25")
        #expect(CalendarDate(containing: try instant("2026-10-25T23:30:00Z"), in: berlin).text == "2026-10-26")
        #expect(CalendarDate(containing: try instant("2026-10-25T22:30:00Z"), in: berlin).text == "2026-10-25")

        let period = WorkHoursPeriod(endingOn: try date("2026-10-27"))
        #expect(
            period.dates.map(\.text) == [
                "2026-10-21", "2026-10-22", "2026-10-23", "2026-10-24", "2026-10-25", "2026-10-26", "2026-10-27",
            ])
        #expect(period.first.text == "2026-10-21")
        #expect(WorkHoursPeriod(endingOn: try date("2026-10-27"), periodsBack: 1).last.text == "2026-10-20")
        #expect(WorkHoursPeriod(endingOn: try date("2027-01-03")).first.text == "2026-12-28")
    }
}
