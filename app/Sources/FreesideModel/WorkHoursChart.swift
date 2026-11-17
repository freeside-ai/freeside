import Foundation
import FreesideAPI

/// A calendar date as `GET /work-hours` names one, `YYYY-MM-DD`. It is a
/// label, not an instant: adding days steps the label in a UTC Gregorian
/// calendar, so a clock change in the reader's zone never shifts a date.
/// Only `init(containing:in:)` reads a zone.
package struct CalendarDate: Hashable, Comparable, Sendable {
    package let year: Int
    package let month: Int
    package let day: Int

    /// Nil unless `text` is strictly `YYYY-MM-DD` and names a real date.
    package init?(_ text: String) {
        let fields = text.split(separator: "-", omittingEmptySubsequences: false)
        guard fields.map(\.count) == [4, 2, 2], fields.allSatisfy({ $0.allSatisfy(\.isASCII) }),
            let year = Int(fields[0]), let month = Int(fields[1]), let day = Int(fields[2]),
            DateComponents(year: year, month: month, day: day).isValidDate(in: Self.utc)
        else { return nil }
        self.year = year
        self.month = month
        self.day = day
    }

    /// The date `instant` falls on in `zone`.
    package init(containing instant: Date, in zone: TimeZone) {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = zone
        let parts = calendar.dateComponents([.year, .month, .day], from: instant)
        year = parts.year ?? 0
        month = parts.month ?? 0
        day = parts.day ?? 0
    }

    package func adding(days: Int) -> CalendarDate {
        let shifted = Self.utc.date(byAdding: .day, value: days, to: noonUTC) ?? noonUTC
        return CalendarDate(containing: shifted, in: .gmt)
    }

    package var text: String { String(format: "%04d-%02d-%02d", year, month, day) }

    /// Noon UTC on this date. Formatted in GMT, it names this date whatever
    /// the reader's zone.
    package var noonUTC: Date {
        Self.utc.date(from: DateComponents(year: year, month: month, day: day, hour: 12)) ?? .distantPast
    }

    package static func < (lhs: CalendarDate, rhs: CalendarDate) -> Bool {
        (lhs.year, lhs.month, lhs.day) < (rhs.year, rhs.month, rhs.day)
    }

    private static let utc: Calendar = {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .gmt
        return calendar
    }()
}

/// The Over Time tab's period: seven calendar dates ending on `last`.
package struct WorkHoursPeriod: Hashable, Sendable {
    package static let length = 7

    package let last: CalendarDate
    package var first: CalendarDate { last.adding(days: 1 - Self.length) }
    package var dates: [CalendarDate] { (0..<Self.length).map { first.adding(days: $0) } }

    /// The period `periodsBack` steps before the one that ends on `today`.
    package init(endingOn today: CalendarDate, periodsBack: Int = 0) {
        last = today.adding(days: -periodsBack * Self.length)
    }
}

/// What the Over Time tab draws from a `GET /work-hours` answer (#3242):
/// each day's time in the run time strips' four classes, its held time by
/// reason, and its tasks, with the period's totals. Seconds stay as the
/// daemon rounded them, so a day's classes can sum to a little less than
/// its elapsed time.
package struct WorkHoursChart: Equatable {
    package struct ClassTime: Equatable {
        package let kind: RunTimeStrip.Kind
        package let seconds: TimeInterval
    }

    package struct HeldTime: Equatable {
        package let reason: Components.Schemas.RunHoldReason
        package let seconds: TimeInterval
    }

    package struct TaskTime: Identifiable, Equatable {
        package let id: String
        /// The task's display name, or nil when the client doesn't hold
        /// the task, which then shows by id and opens nothing.
        package let name: String?
        package let seconds: TimeInterval
    }

    /// Whether the daemon was recording holds over a day. Before it began,
    /// held time reads as other, so a day it doesn't cover is marked
    /// rather than drawn as zero held time.
    package enum HoldHistory: Equatable {
        case recorded
        /// Recording began at this instant, inside the day.
        case began(Date)
        /// Recording began after the day ended.
        case none
    }

    package struct Day: Identifiable, Equatable {
        package let date: CalendarDate
        package let elapsed: TimeInterval
        /// One entry per class, in `RunTimeStrip.Kind` order.
        package let classes: [ClassTime]
        /// Largest first, as the daemon lists them.
        package let held: [HeldTime]
        /// Largest first, as the daemon lists them.
        package let tasks: [TaskTime]
        package let holdHistory: HoldHistory
        /// The day had begun but not ended at `as_of`: today, so far. A
        /// day that hadn't begun, which a client clock ahead of the
        /// daemon's can ask for, isn't partial.
        package let isPartial: Bool

        package var id: CalendarDate { date }

        /// The classes the day can report: held drops out when hold
        /// history covers none of the day, since its held time reads as
        /// other there rather than as zero.
        package var reportedClasses: [ClassTime] {
            holdHistory == .none ? classes.filter { $0.kind != .held } : classes
        }
    }

    package let days: [Day]
    package let elapsed: TimeInterval
    /// The period's totals, one entry per class, in `RunTimeStrip.Kind`
    /// order.
    package let classes: [ClassTime]
    package let taskCount: Int
    package let holdHistorySince: Date
    /// Runs the daemon left out; the figures are incomplete when it isn't
    /// zero.
    package let skippedRunCount: Int

    /// The largest day's elapsed time: every bar is drawn to this scale.
    package var scale: TimeInterval { days.map(\.elapsed).max() ?? 0 }
    package var isEmpty: Bool { days.allSatisfy { $0.elapsed == 0 } }
    /// Whether any day starts before hold history began.
    package var startsBeforeHoldHistory: Bool { days.contains { $0.holdHistory != .recorded } }

    /// Nil when a day's date isn't a calendar date, which breaks the
    /// contract. `tasks` are the tasks the client holds, which name a
    /// day's tasks.
    package init?(_ hours: Components.Schemas.WorkHours, tasks: [Components.Schemas.TaskSnapshot]) {
        let names = Dictionary(
            tasks.map { ($0.task.id, $0.task.display_names.task.text) }, uniquingKeysWith: { first, _ in first })
        var days: [Day] = []
        for day in hours.days {
            guard let date = CalendarDate(day.date) else { return nil }
            days.append(
                Day(
                    date: date, elapsed: TimeInterval(day.elapsed_seconds),
                    classes: Self.classes(
                        working: day.agent_working_seconds, waiting: day.waiting_on_you_seconds,
                        held: day.held_seconds, other: day.other_seconds),
                    held: day.held_by_reason.map { .init(reason: $0.reason, seconds: TimeInterval($0.seconds)) },
                    tasks: day.tasks.map {
                        .init(id: $0.task_id, name: names[$0.task_id], seconds: TimeInterval($0.elapsed_seconds))
                    },
                    holdHistory: Self.holdHistory(
                        since: hours.hold_history_since, starts: day.starts_at, ends: day.ends_at),
                    isPartial: day.starts_at <= hours.as_of && hours.as_of < day.ends_at))
        }
        self.days = days
        elapsed = TimeInterval(hours.totals.elapsed_seconds)
        classes = Self.classes(
            working: hours.totals.agent_working_seconds, waiting: hours.totals.waiting_on_you_seconds,
            held: hours.totals.held_seconds, other: hours.totals.other_seconds)
        taskCount = hours.totals.task_count
        holdHistorySince = hours.hold_history_since
        skippedRunCount = hours.skipped_run_count
    }

    private static func classes(working: Int64, waiting: Int64, held: Int64, other: Int64) -> [ClassTime] {
        RunTimeStrip.Kind.allCases.map { kind in
            let seconds =
                switch kind {
                case .agentWorking: working
                case .waitingOnYou: waiting
                case .held: held
                case .other: other
                }
            return ClassTime(kind: kind, seconds: TimeInterval(seconds))
        }
    }

    private static func holdHistory(since: Date, starts: Date, ends: Date) -> HoldHistory {
        if since <= starts { return .recorded }
        return since < ends ? .began(since) : .none
    }
}
