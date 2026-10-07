import Foundation
import Testing

@testable import FreesideCore

@Suite struct FreesideFormatTests {
    private let utc = TimeZone.gmt
    private let enUS = Locale(identifier: "en_US")

    private func instant(_ iso: String) throws -> Date {
        try Date(iso, strategy: .iso8601)
    }

    @Test func omitsTheYearWhenItIsTheCurrentYear() throws {
        let text = FreesideFormat.shortTime(
            try instant("2026-09-12T09:41:27Z"), now: try instant("2026-09-18T12:00:00Z"),
            locale: enUS, timeZone: utc)
        #expect(!text.contains("2026"))
        #expect(text.contains("Sep 12"))
        #expect(text.contains("9:41"))
    }

    @Test func neverPrintsSeconds() throws {
        let text = FreesideFormat.shortTime(
            try instant("2026-09-12T09:41:27Z"), now: try instant("2026-09-18T12:00:00Z"),
            locale: enUS, timeZone: utc)
        #expect(!text.contains("27"))
    }

    /// R8: the date, a comma, the time, with none of the words a locale's
    /// own date-and-time pattern puts between them ("at", "um"). Unicode
    /// sets a narrow no-break space ahead of the day period; the test reads
    /// it as a plain space so the pinned string is the one a reader sees.
    @Test func joinsTheDateAndTheTimeWithACommaAndNoWord() throws {
        let date = try instant("2026-08-11T22:15:00Z")
        let now = try instant("2026-09-18T12:00:00Z")
        let expected = [
            "en_US": "Aug 11, 10:15 PM", "en_GB": "11 Aug, 22:15", "de_DE": "11. Aug., 22:15",
        ]
        for (identifier, text) in expected {
            let short = FreesideFormat.shortTime(
                date, now: now, locale: Locale(identifier: identifier), timeZone: utc)
            #expect(short.replacing("\u{202F}", with: " ") == text, "locale \(identifier)")
        }
    }

    /// A prior-year instant keeps its year in the date half, and the same
    /// comma joins the time.
    @Test func aDifferentYearKeepsTheYearAheadOfTheComma() throws {
        let date = try instant("2025-12-31T18:05:00Z")
        let now = try instant("2026-01-02T00:00:00Z")
        let expected = [
            "en_US": "Dec 31, 2025, 6:05 PM", "en_GB": "31 Dec 2025, 18:05",
            "de_DE": "31. Dez. 2025, 18:05", "ja_JP": "2025年12月31日, 18:05",
        ]
        for (identifier, text) in expected {
            let short = FreesideFormat.shortTime(
                date, now: now, locale: Locale(identifier: identifier), timeZone: utc)
            #expect(short.replacing("\u{202F}", with: " ") == text, "locale \(identifier)")
        }
    }

    /// The year is compared in the display time zone: 23:30 UTC on
    /// New Year's Eve is already next year in Tokyo.
    @Test func comparesTheYearInTheDisplayTimeZone() throws {
        let date = try instant("2025-12-31T23:30:00Z")
        let now = try instant("2026-01-01T00:30:00Z")
        let tokyo = try #require(TimeZone(identifier: "Asia/Tokyo"))
        #expect(
            !FreesideFormat.shortTime(date, now: now, locale: enUS, timeZone: tokyo).contains("2026"))
        #expect(FreesideFormat.shortTime(date, now: now, locale: enUS, timeZone: utc).contains("2025"))
    }

    @Test func followsTheLocaleOrderAndClock() throws {
        let text = FreesideFormat.shortTime(
            try instant("2026-09-12T21:41:00Z"), now: try instant("2026-09-18T12:00:00Z"),
            locale: Locale(identifier: "de_DE"), timeZone: utc)
        #expect(text.contains("12. Sept"))
        #expect(text.contains("21:41"))
        #expect(!text.contains("2026"))
    }

    /// What `shortTime` drops stays recoverable: the exact stamp keeps the
    /// seconds and the year, in UTC whatever the display zone.
    @Test func theExactTimeKeepsTheSecondsAndTheYear() throws {
        #expect(FreesideFormat.exactTime(try instant("2026-09-12T09:41:27Z")) == "2026-09-12T09:41:27Z")
    }
}
