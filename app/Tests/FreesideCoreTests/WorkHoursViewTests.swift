import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// What the Over Time tab says to assistive technology (#3242): each day's
/// button reads its date, elapsed time, every class's time, and its marks;
/// the totals read as one heading sentence.
@Suite @MainActor struct WorkHoursViewTests {
    private let locale = Locale(identifier: "en_US_POSIX")

    private func chart(holdHistorySince: Date? = nil) throws -> WorkHoursChart {
        let hours =
            holdHistorySince.map { WorkStatusFixtures.workHoursWeek(holdHistorySince: $0) }
            ?? WorkStatusFixtures.workHoursWeek()
        return try #require(WorkHoursChart(hours, tasks: WorkStatusFixtures.tasks()))
    }

    private func label(_ day: WorkHoursChart.Day) -> String {
        WorkHoursView.dayLabel(
            day, marks: WorkHoursView.marks(day, locale: locale, timeZone: .gmt), now: RunFixtures.screenshotInstant,
            locale: locale, timeZone: .gmt)
    }

    @Test func aDayReadsItsDateAndEveryClass() throws {
        let day = try chart().days[4]
        #expect(
            label(day)
                == "Mon, Aug 10, elapsed 3 hours: agent working 2 hours, waiting on you 30 minutes, held 20 minutes, other 10 minutes."
        )
    }

    @Test func aDayReadsItsHoldHistoryAndTodayMarks() throws {
        let since = try #require(ISO8601DateFormatter().date(from: "2026-08-09T14:00:00Z"))
        let chart = try chart(holdHistorySince: since)
        #expect(label(chart.days[0]).hasSuffix(" No hold history."))
        // Its held time reads as other, so it reads no held figure.
        #expect(!label(chart.days[0]).contains("held"))
        // Foundation puts a narrow no-break space before PM.
        #expect(label(chart.days[3]).hasSuffix(" Hold history from 2:00\u{202F}PM."))
        #expect(label(chart.days[6]).hasSuffix(" So far today."))
        #expect(!label(chart.days[5]).contains("hold history"))
    }

    /// At 8 PM on Dec 31 in New York it is already Jan 1 in GMT; the
    /// year is the reader's, so a date that year names none.
    @Test func theYearIsTheReaders() throws {
        let newYork = try #require(TimeZone(identifier: "America/New_York"))
        let now = try #require(ISO8601DateFormatter().date(from: "2027-01-01T01:00:00Z"))
        let date = try #require(CalendarDate("2026-12-31"))
        #expect(WorkHoursView.dayName(date, weekday: false, now: now, locale: locale, timeZone: newYork) == "Dec 31")
        #expect(
            WorkHoursView.dayName(date, weekday: false, now: now, locale: locale, timeZone: .gmt) == "Dec 31, 2026")
    }

    @Test func theTotalsReadAsOneSentence() throws {
        #expect(
            WorkHoursView.summary(try chart(), locale: locale)
                == "Elapsed 12 hours, 35 minutes across 6 tasks: agent working 6 hours, 5 minutes, waiting on you 3 hours, held 2 hours, 40 minutes, other 50 minutes."
        )
    }

    @Test func theIncompleteNoticeCountsSkippedRuns() {
        #expect(WorkHoursView.incompleteSentence(1).hasPrefix("1 run was left out because its record"))
        #expect(WorkHoursView.incompleteSentence(3).hasPrefix("3 runs were left out because their records"))
    }
}
