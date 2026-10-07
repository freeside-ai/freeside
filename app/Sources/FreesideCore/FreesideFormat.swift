import SwiftUI

#if os(iOS)
    import UIKit
#endif

/// Shared value formats for primary text. One place so every timeline,
/// review round, and event row prints an instant the same way.
enum FreesideFormat {
    /// The one time format the task surfaces share (R8): the abbreviated
    /// date, a comma, and the shortened time (`Aug 11, 10:15 PM`), never
    /// seconds, with the year dropped when it is `now`'s year in
    /// `timeZone`. The two halves are formatted apart and joined here
    /// because the locale's own date-and-time pattern puts a word between
    /// them ("at", "um"), and a row's context line has no room for one. The
    /// exact instant belongs in a tooltip or Technical details, not here.
    ///
    /// `now` is a parameter, not read from the clock, so a screenshot with
    /// fixed fixture dates does not change when the calendar year does.
    static func shortTime(
        _ date: Date,
        now: Date,
        locale: Locale = .current,
        timeZone: TimeZone = .current
    ) -> String {
        var calendar = locale.calendar
        calendar.timeZone = timeZone
        let clock = Date.FormatStyle(locale: locale, calendar: calendar, timeZone: timeZone)
            .hour().minute()
        let day = shortDate(date, now: now, locale: locale, timeZone: timeZone)
        return "\(day), \(date.formatted(clock))"
    }

    /// The date alone in the same grammar, for a summary that names a day
    /// ("from Sep 11"): abbreviated month and day, the year only when it
    /// differs from `now`'s.
    static func shortDate(
        _ date: Date,
        now: Date,
        locale: Locale = .current,
        timeZone: TimeZone = .current
    ) -> String {
        var calendar = locale.calendar
        calendar.timeZone = timeZone
        let style = Date.FormatStyle(locale: locale, calendar: calendar, timeZone: timeZone)
            .month(.abbreviated).day()
        let sameYear = calendar.component(.year, from: date) == calendar.component(.year, from: now)
        return date.formatted(sameYear ? style : style.year())
    }

    /// The instant `shortTime` abbreviates, to the second and with its
    /// year: the ISO 8601 stamp the task row's and pairing's hover carry.
    static func exactTime(_ date: Date) -> String {
        date.formatted(.iso8601)
    }
}

extension View {
    /// Keeps the exact instant one gesture away from a `shortTime` string:
    /// hover help on macOS, a long-press Copy on iOS, as the pairing expiry
    /// row does. A row that shortens a recorded time and shows it nowhere
    /// else uses this so the shortening loses nothing.
    func exactInstant(_ date: Date) -> some View {
        exactStamp(FreesideFormat.exactTime(date))
    }

    /// The same for a line that shortens a span ("Observed … to …"): both
    /// ends, exactly.
    func exactInstants(_ start: Date, to end: Date) -> some View {
        exactStamp("\(FreesideFormat.exactTime(start)) to \(FreesideFormat.exactTime(end))")
    }

    @ViewBuilder
    private func exactStamp(_ exact: String) -> some View {
        #if os(macOS)
            help(exact)
        #elseif os(iOS)
            contextMenu {
                Button("Copy \(exact)") {
                    UIPasteboard.general.string = exact
                }
            }
        #else
            self
        #endif
    }
}

extension EnvironmentValues {
    /// The instant `FreesideFormat.shortTime` compares years against, read
    /// beside `locale` and `timeZone` by the views that format times. Nil
    /// reads the clock; a screenshot pins it with the other two.
    @Entry var pinnedNow: Date? = nil
}
