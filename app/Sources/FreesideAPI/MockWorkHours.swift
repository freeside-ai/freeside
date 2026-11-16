import Foundation

/// The mock's `GET /work-hours` (#3241). The daemon derives the figures from
/// its runs; the mock serves the fixture days a test sets per project, and
/// answers a date with no fixture with a zero day, as the daemon answers a
/// date no run touched. The request checks and their messages are the
/// daemon's (`parseWorkHoursRequest` in daemon/internal/signet).
enum MockWorkHours {
    /// A request that passed the daemon's checks.
    struct Request: Equatable {
        let from: String
        let days: Int
        /// The zone as the request named it, which the response echoes.
        let timeZoneName: String
        let timeZone: TimeZone
        let projectID: String?
    }

    /// A refused request: the daemon's 400 and its message.
    struct Refusal: Error, Equatable {
        let message: String
    }

    /// The daemon's bound on `days` (`domain.MaxWorkHoursDays`).
    static let maxDays = 92

    static func parse(_ items: [URLQueryItem]) throws(Refusal) -> Request {
        let from = try required("from", in: items)
        guard dateParts(from) != nil else { throw Refusal(message: "from must be a calendar date, YYYY-MM-DD") }
        guard let days = Int(try required("days", in: items)), (1...maxDays).contains(days) else {
            throw Refusal(message: "days must be an integer from 1 to \(maxDays)")
        }
        // The daemon's `domain.WorkHoursPeriodInRange`: the last date is at
        // latest 9999-12-30, so every day's instants stay inside year 9999.
        guard let firstDate = dateParts(from).flatMap({ utc.date(from: $0) }),
            let after = utc.date(byAdding: .day, value: days, to: firstDate),
            utc.component(.year, from: after) < 10_000
        else { throw Refusal(message: "the period must end by 9999-12-30") }
        let zone = try required("time_zone", in: items)
        // Foundation resolves the names Go's zone database holds, and also
        // offsets such as "GMT+0500" that Go refuses; it renames "UTC" to
        // "GMT" and returns nil for "Local", the daemon host's zone.
        guard let timeZone = TimeZone(identifier: zone), timeZone.identifier == zone || zone == "UTC",
            !zone.hasPrefix("GMT+"), !zone.hasPrefix("GMT-")
        else { throw Refusal(message: "time_zone \"\(zone)\" is not a known IANA time zone") }
        let projectID =
            items.contains { $0.name == "project_id" } ? try required("project_id", in: items) : nil
        return Request(from: from, days: days, timeZoneName: zone, timeZone: timeZone, projectID: projectID)
    }

    /// The one non-empty value of `name`, refused missing, empty, or
    /// repeated.
    private static func required(_ name: String, in items: [URLQueryItem]) throws(Refusal) -> String {
        let values = items.filter { $0.name == name }.map { $0.value ?? "" }
        guard let value = values.first else { throw Refusal(message: "\(name) is required") }
        guard values.count == 1 else { throw Refusal(message: "\(name) must be given once") }
        guard !value.isEmpty else { throw Refusal(message: "\(name) must not be empty") }
        return value
    }

    /// The calendar date `text` names, strictly YYYY-MM-DD, or nil.
    private static func dateParts(_ text: String) -> DateComponents? {
        let fields = text.split(separator: "-", omittingEmptySubsequences: false)
        guard fields.map(\.count) == [4, 2, 2], fields.allSatisfy({ $0.allSatisfy(\.isASCII) }),
            let year = Int(fields[0]), let month = Int(fields[1]), let day = Int(fields[2])
        else { return nil }
        let parts = DateComponents(year: year, month: month, day: day)
        return parts.isValidDate(in: utc) ? parts : nil
    }

    private static let utc: Calendar = {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = .gmt
        return calendar
    }()

    /// The `days` dates from `from` on, as YYYY-MM-DD; empty when `from`
    /// names no date.
    static func dates(from: String, days: Int) -> [String] {
        guard let parts = dateParts(from), let first = utc.date(from: parts) else { return [] }
        return (0..<days).compactMap { offset in
            utc.date(byAdding: .day, value: offset, to: first).map { date in
                let day = utc.dateComponents([.year, .month, .day], from: date)
                return String(format: "%04d-%02d-%02d", day.year ?? 0, day.month ?? 0, day.day ?? 0)
            }
        }
    }

    /// The first instant of `date` in `zone`. Foundation's start of day is
    /// the daemon's: local midnight, or the clock change where one skips it.
    private static func startOfDate(_ date: String, in zone: TimeZone) -> Date? {
        guard var parts = dateParts(date) else { return nil }
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = zone
        parts.hour = 12
        return calendar.date(from: parts).map { calendar.startOfDay(for: $0) }
    }

    /// The response to `request` from `fixtures`, each project's days by
    /// date. A day's figures sum the fixtures for its date across the
    /// projects the request covers, and its bounds come from the request's
    /// zone, not the fixture.
    static func response(
        _ request: Request, fixtures: [String: [Components.Schemas.WorkHoursDay]], holdHistorySince: Date,
        asOf: Date
    ) -> Components.Schemas.WorkHours {
        let covered = request.projectID.map { fixtures[$0] ?? [] } ?? fixtures.values.flatMap { $0 }
        let byDate = Dictionary(grouping: covered, by: \.date)
        let dates = dates(from: request.from, days: request.days + 1)
        let days = zip(dates, dates.dropFirst()).map { date, next in
            let sum = Figures(byDate[date] ?? [])
            return Components.Schemas.WorkHoursDay(
                date: date, starts_at: startOfDate(date, in: request.timeZone) ?? asOf,
                ends_at: startOfDate(next, in: request.timeZone) ?? asOf,
                elapsed_seconds: sum.elapsed, agent_working_seconds: sum.working,
                waiting_on_you_seconds: sum.waiting, held_seconds: sum.held, held_by_reason: sum.heldByReason,
                other_seconds: sum.other, tasks: sum.tasks)
        }
        let totals = Figures(days)
        return .init(
            as_of: asOf, time_zone: request.timeZoneName, project_id: request.projectID,
            hold_history_since: holdHistorySince, skipped_run_count: 0,
            totals: .init(
                elapsed_seconds: totals.elapsed, agent_working_seconds: totals.working,
                waiting_on_you_seconds: totals.waiting, held_seconds: totals.held,
                held_by_reason: totals.heldByReason, other_seconds: totals.other,
                task_count: Set(days.flatMap { $0.tasks.map(\.task_id) }).count),
            days: days)
    }

    /// The sum of several days' figures, listed in the daemon's order:
    /// held reasons and tasks largest first, then by name.
    private struct Figures {
        var elapsed: Int64 = 0
        var working: Int64 = 0
        var waiting: Int64 = 0
        var held: Int64 = 0
        var other: Int64 = 0
        var heldByReason: [Components.Schemas.WorkHoursHeld] = []
        var tasks: [Components.Schemas.WorkHoursTask] = []

        init(_ days: [Components.Schemas.WorkHoursDay]) {
            var heldBy: [Components.Schemas.RunHoldReason: Int64] = [:]
            var taskSeconds: [String: Int64] = [:]
            for day in days {
                elapsed += day.elapsed_seconds
                working += day.agent_working_seconds
                waiting += day.waiting_on_you_seconds
                held += day.held_seconds
                other += day.other_seconds
                for entry in day.held_by_reason { heldBy[entry.reason, default: 0] += entry.seconds }
                for task in day.tasks { taskSeconds[task.task_id, default: 0] += task.elapsed_seconds }
            }
            heldByReason = heldBy.map { .init(reason: $0.key, seconds: $0.value) }
                .sorted { ($1.seconds, $0.reason.rawValue) < ($0.seconds, $1.reason.rawValue) }
            tasks = taskSeconds.map { .init(task_id: $0.key, elapsed_seconds: $0.value) }
                .sorted { ($1.elapsed_seconds, $0.task_id) < ($0.elapsed_seconds, $1.task_id) }
        }
    }
}
