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

    /// With the year shown, the format is the abbreviated-date,
    /// shortened-time style the timelines printed before, so a prior-year
    /// instant reads exactly as it did.
    @Test func aDifferentYearMatchesTheAbbreviatedShortenedStyle() throws {
        let date = try instant("2025-12-31T18:05:00Z")
        let now = try instant("2026-01-02T00:00:00Z")
        for identifier in ["en_US", "en_GB", "de_DE", "ja_JP"] {
            let locale = Locale(identifier: identifier)
            let expected = date.formatted(
                Date.FormatStyle(date: .abbreviated, time: .shortened, locale: locale, timeZone: utc))
            #expect(
                FreesideFormat.shortTime(date, now: now, locale: locale, timeZone: utc) == expected,
                "locale \(identifier)")
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
