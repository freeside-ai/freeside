import Foundation
import FreesideAPI
import Observation

/// A usage command (`refresh_usage` or `reassign_collector`) saved before it
/// was sent: the exact command an explicit Retry resends, and its receipt
/// once the daemon accepted it. Local delivery evidence, never an authority
/// for the pool's record.
public struct PendingUsageCommand: Codable, Equatable, Sendable {
    public let command: Components.Schemas.ClientCommand
    /// The epoch the command was made in. A receipt from another epoch can't
    /// be compared with the record sync shows.
    public let syncEpoch: String
    public var receipt: Components.Schemas.CommandResult?

    func isValid(deviceID: String) -> Bool {
        switch command.payload {
        case .refresh_usage, .reassign_collector: break
        case .decision, .stop_task, .submit_task, .change_task_line, .set_routing_policy, .move_parked_task,
            .set_workflow_policy:
            return false
        }
        if let receipt, !CommandResultTrust.accepts(receipt, for: command) { return false }
        return !command.command_id.isEmpty && !deviceID.isEmpty && command.device_id == deviceID
            && !syncEpoch.isEmpty && command.expected_entity_version == nil && command.expected_bindings == nil
    }

    /// The pool the command names.
    public var pool: String? {
        switch command.payload {
        case .refresh_usage(let payload): payload.pool
        case .reassign_collector(let payload): payload.pool
        case .decision, .stop_task, .submit_task, .change_task_line, .set_routing_policy, .move_parked_task,
            .set_workflow_policy:
            nil
        }
    }
}

/// Reads the usage pools sync adopted and sends the operator's refresh or
/// collector reassignment as a saved command (plan §5.4 usage observation),
/// modeled on `RoutingPolicyModel`: the command persists before it is sent,
/// an unanswered send is retried only by an explicit Retry that resends it
/// unchanged, and nothing here changes a pool's record; new readings and a
/// new collector arrive through sync. The saved command, whether it is being
/// sent, and its refusal live in the coordinator, where a reopened sheet
/// finds them.
@MainActor @Observable
package final class AccountUsageModel {
    private let coordinator: SyncCoordinator

    package init(coordinator: SyncCoordinator) { self.coordinator = coordinator }

    package var pools: [Components.Schemas.UsagePoolSnapshot] { coordinator.usagePools }

    /// The synced runs parked on `pool` (`UsageDisplay.parkedRuns`).
    package func parkedRuns(on pool: String) -> Int {
        UsageDisplay.parkedRuns(on: pool, in: coordinator.runs)
    }

    package var pending: PendingUsageCommand? { coordinator.pendingUsageCommand }
    package var sending: Bool { coordinator.usageCommandSending }
    package var failure: String? {
        get { coordinator.usageCommandFailure }
        set { coordinator.usageCommandFailure = newValue }
    }

    /// The command this session settled: a refresh once the daemon accepted
    /// it, and a reassignment while sync still shows its collector, so
    /// another device's later reassignment replaces the notice.
    package var settled: Components.Schemas.CommandResult? {
        guard let receipt = coordinator.settledUsageCommand else { return nil }
        guard case .reassign_collector(let record) = receipt.record else { return receipt }
        return pools.first { $0.pool.id == record.pool }?.pool.collector == record.collector ? receipt : nil
    }

    /// Why no usage command can be sent now, or nil when one can.
    package var unavailableReason: String? {
        switch coordinator.store.freshness {
        case .fresh, .unvalidated:
            return coordinator.cursors == nil ? "Refresh before changing usage collection." : nil
        case .unauthenticated: return "Pair this device again before changing usage collection."
        case .unreachable: return "Offline. Connect to the daemon before changing usage collection."
        case .syncFailing: return "Usage isn't current. Refresh before changing collection."
        case .incompatible: return "Update the app or the daemon before changing usage collection."
        }
    }

    /// One usage command is pending at a time, so a second waits for the
    /// first to settle.
    package var canSend: Bool { unavailableReason == nil && pending == nil && !sending }

    package func refresh(pool: String) async {
        await submit(.refresh_usage(.init(kind: .refresh_usage, pool: pool)))
    }

    package func reassign(pool: String, to enrollmentID: String) async {
        await submit(.reassign_collector(.init(kind: .reassign_collector, pool: pool, enrollment_id: enrollmentID)))
    }

    /// Explicit retry only: resends the saved command unchanged, and only in
    /// the epoch it was made in (sync settles it in any other).
    package func retry() async {
        guard let entry = pending, entry.receipt == nil, entry.syncEpoch == coordinator.cursors?.syncEpoch,
            coordinator.store.freshness != .unauthenticated
        else { return }
        await send(entry)
    }

    private func submit(_ payload: Components.Schemas.ClientCommand.payloadPayload) async {
        guard canSend, let epoch = coordinator.cursors?.syncEpoch else { return }
        let command = Components.Schemas.ClientCommand(
            command_id: UUID().uuidString, device_id: coordinator.store.device.deviceID, payload: payload)
        let entry = PendingUsageCommand(command: command, syncEpoch: epoch)
        coordinator.clearSettledUsageCommand()
        guard coordinator.retainUsageCommand(entry) else {
            failure = "The command couldn't be saved on this device. Nothing was sent. Try again."
            return
        }
        await send(entry)
    }

    private func send(_ entry: PendingUsageCommand) async {
        guard !sending else { return }
        coordinator.usageCommandSending = true
        failure = nil
        defer { coordinator.usageCommandSending = false }
        do {
            switch try await coordinator.store.client.submitCommand(body: .json(entry.command)) {
            case .ok(let ok):
                let receipt = try ok.body.json
                guard CommandResultTrust.accepts(receipt, for: entry.command) else { return }
                var accepted = entry
                accepted.receipt = receipt
                // If this save fails, the unanswered command stays retryable.
                guard coordinator.retainUsageCommand(accepted) else { return }
                // A refresh settles on its receipt; a reassignment once sync
                // shows the revision it was recorded at.
                coordinator.settleUsageCommand()
                await coordinator.refreshAfterCommit()
                coordinator.settleUsageCommand()
            case .conflict:
                // No usage command refuses with a conflict; an unexpected one
                // leaves the saved command unresolved.
                break
            case .badRequest(let refusal):
                // The daemon refused the command before accepting it.
                coordinator.finishUsageCommand()
                let refused = if case .refresh_usage = entry.command.payload { "refresh" } else { "reassignment" }
                failure = CommandRefusal.message(
                    refusal, otherwise: "The daemon rejected the \(refused) (status 400). Nothing was changed.")
                await coordinator.refreshAfterCommit()
            case .undocumented(let status, _):
                switch status {
                case 404:
                    // The daemon refused the command before accepting it.
                    coordinator.finishUsageCommand()
                    let refused = if case .refresh_usage = entry.command.payload { "refresh" } else { "reassignment" }
                    failure = "The daemon rejected the \(refused) (status \(status)). Nothing was changed."
                    await coordinator.refreshAfterCommit()
                case 401, 403: coordinator.store.publish(.unauthenticated)
                default: break
                }
            }
        } catch {
            // A missing or malformed answer leaves the saved command unresolved.
        }
    }
}

/// What the usage view draws for a pool's record, as pure functions of the
/// record and the client's clock. None computes a value the daemon didn't
/// record: a share is the reading's own number, staleness and exhaustion
/// compare the record's own instants with `now`, and nothing adds across
/// pools.
package enum UsageDisplay {
    /// The reading's used share in the form the provider reported it, or nil
    /// for a reading with a status alone.
    package static func share(_ reading: Components.Schemas.UsageWindowReading) -> String? {
        if let percent = reading.used_percent { return "\(percent)%" }
        if let quantity = reading.quantity, let unit = reading.unit { return "\(quantity) \(unit)" }
        return nil
    }

    /// Whether the client's clock has carried the reading past the daemon's
    /// `stale_at`. A stale reading is shown as stale, never as replenished.
    package static func isStale(_ reading: Components.Schemas.UsageWindowReading, now: Date) -> Bool {
        reading.stale_at.map { now >= $0 } ?? false
    }

    /// Whether any of the window's recorded marks hasn't lapsed: a mark
    /// lapses once `now` reaches its `resets_at`, and one without a reset
    /// holds until the record changes.
    package static func isExhausted(
        window: String, marks: [Components.Schemas.UsageExhaustedMark], now: Date
    ) -> Bool {
        marks.contains { $0.window == window && ($0.resets_at.map { now < $0 } ?? true) }
    }

    /// Every window the record names, by a reading or a mark, in window
    /// order: a refusal can mark a window the collector hasn't read.
    package static func windows(_ pool: Components.Schemas.UsagePoolUsage) -> [String] {
        Array(Set(pool.windows.map(\.window) + pool.recorded_exhausted_marks.map(\.window))).sorted()
    }

    /// Words for the window ids the collectors report; nil for any other id,
    /// which the view shows as itself.
    package static func label(_ window: String) -> String? {
        switch window {
        case "five_hour": "5-hour"
        case "seven_day": "Weekly"
        case "seven_day_opus": "Weekly Opus"
        case "seven_day_sonnet": "Weekly Sonnet"
        // The Claude CLI's name for the Fable weekly bucket.
        case "seven_day_overage_included": "Weekly Fable"
        default: nil
        }
    }

    /// The time left until `instant`, to the minute: `in 45m`, `in 2h 30m`,
    /// `in 3d 4h`, or `passed` once it is reached.
    package static func countdown(to instant: Date, now: Date) -> String {
        let seconds = Int(instant.timeIntervalSince(now))
        guard seconds > 0 else { return "passed" }
        let minutes = max(1, seconds / 60)
        if minutes < 60 { return "in \(minutes)m" }
        let hours = minutes / 60
        if hours < 24 { return minutes % 60 == 0 ? "in \(hours)h" : "in \(hours)h \(minutes % 60)m" }
        return hours % 24 == 0 ? "in \(hours / 24)d" : "in \(hours / 24)d \(hours % 24)h"
    }

    // MARK: Capacity (R32)

    /// One window as the capacity block draws it: what is left of it, in
    /// the reading's own terms.
    package struct WindowCapacity: Equatable, Sendable {
        package enum Level: Equatable, Sendable {
            /// More than the low line is left.
            case fine
            /// At or under `lowLine` percent is left.
            case low
            /// Nothing is left: an exhausted reading or a live mark.
            case usedUp
            /// No reading, and every mark has lapsed.
            case noReading
        }

        /// The window id the record names.
        package let window: String
        /// The window's words, or its id when it has none.
        package let name: String
        package let value: String
        /// The percent left, which the meter fills; nil draws no meter.
        package let left: Int?
        package let level: Level
        /// When the window next has capacity, if the record says.
        package let resetsAt: Date?

        /// Whether `name` is the bare window id, which reads as an id.
        package var nameIsWindowID: Bool { name == window }
    }

    /// The percent left at or under which a window reads low.
    package static let lowLine = 15

    /// What the reading leaves of the window, in percent; nil for a
    /// reading with no percent.
    package static func left(_ reading: Components.Schemas.UsageWindowReading) -> Int? {
        reading.used_percent.map { min(100, max(0, 100 - $0)) }
    }

    /// The window's capacity at `now`. It is used up while its reading is
    /// exhausted or any of its marks is live, and stays used up until the
    /// last of those causes lapses: its reset is the latest among them, and
    /// none when any cause names none. A used-up window draws an empty
    /// meter whatever unit its reading has, since nothing is left in any.
    package static func capacity(
        window: String, in pool: Components.Schemas.UsagePoolUsage, now: Date
    ) -> WindowCapacity {
        let name = label(window) ?? window
        let reading = pool.windows.first { $0.window == window }
        let liveMarks = pool.recorded_exhausted_marks.filter {
            $0.window == window && isExhausted(window: window, marks: [$0], now: now)
        }
        let exhaustedReading = reading?.status == .exhausted ? reading : nil
        if exhaustedReading != nil || !liveMarks.isEmpty {
            let causes = (exhaustedReading.map { [$0.resets_at] } ?? []) + liveMarks.map(\.resets_at)
            let resetsAt = causes.contains(nil) ? nil : causes.compactMap(\.self).max()
            return .init(
                window: window, name: name, value: "Used up", left: 0, level: .usedUp, resetsAt: resetsAt)
        }
        guard let reading else {
            return .init(window: window, name: name, value: "No reading", left: nil, level: .noReading, resetsAt: nil)
        }
        let left = left(reading)
        return .init(
            window: window, name: name,
            value: left.map { "\($0)% left" } ?? share(reading) ?? "Allowed",
            left: left, level: left.map { $0 <= lowLine } == true ? .low : .fine,
            resetsAt: reading.resets_at)
    }

    /// The runs parked on `pool`: those whose hold names it. The daemon sets
    /// `hold_usage_pool` only beside `usage_pool_exhausted` and only while
    /// the hold is live, so the field alone says the run is parked now; no
    /// reason, lifecycle, or timeline check is added. A hold names a pool,
    /// not a window, so every used-up window of the pool shares the count.
    package static func parkedRuns(on pool: String, in runs: [Components.Schemas.RunSnapshot]) -> Int {
        runs.count { $0.run.hold_usage_pool == pool }
    }

    /// The parked clause on a window's reset line (`2 runs parked`, `1 run
    /// parked`), or nil when the window isn't used up or nothing is parked.
    package static func parkedClause(count: Int, level: WindowCapacity.Level) -> String? {
        guard level == .usedUp, count > 0 else { return nil }
        return count == 1 ? "1 run parked" : "\(count) runs parked"
    }

    /// The reset in words: a countdown under a day (`Resets in 41m`), the
    /// weekday and clock within the week (`Resets Thu 9:00 PM`), and
    /// `shortTime` from a week on, or when the reset falls on today's
    /// weekday, where a weekday would name the wrong week: a weekly window
    /// that has just rolled over resets on the same weekday next week, and
    /// `Resets Wed 2:39 AM` read on a Wednesday says a minute ago. A reset
    /// the clock has passed reads `Resets passed` until the daemon's next
    /// reading replaces it.
    package static func resetLine(
        to instant: Date, now: Date, locale: Locale = .current, timeZone: TimeZone = .current
    ) -> String {
        let day: TimeInterval = 86_400
        let interval = instant.timeIntervalSince(now)
        if interval < day { return "Resets \(countdown(to: instant, now: now))" }
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = timeZone
        let sameWeekday = calendar.component(.weekday, from: instant) == calendar.component(.weekday, from: now)
        if interval < 7 * day, !sameWeekday {
            return "Resets \(FreesideFormat.weekdayTime(instant, locale: locale, timeZone: timeZone))"
        }
        return "Resets \(FreesideFormat.shortTime(instant, now: now, locale: locale, timeZone: timeZone))"
    }

    /// The earliest `stale_at` among the pool's readings that the client's
    /// clock has reached (`isStale`); nil while none has.
    package static func staleSince(_ pool: Components.Schemas.UsagePoolUsage, now: Date) -> Date? {
        pool.windows.filter { isStale($0, now: now) }.compactMap(\.stale_at).min()
    }

    /// The pool band's trailing line: the collector's last attempt, or
    /// that the readings went stale, which outranks it. Wax marks a line
    /// that says the readings can't be trusted as current.
    package struct AttemptLine: Equatable, Sendable {
        package let text: String
        package let isWax: Bool
        /// The instant the line names, for the exact-instant hover (R8).
        package let instant: Date?
    }

    package static func attemptLine(
        _ pool: Components.Schemas.UsagePoolUsage, now: Date, locale: Locale = .current,
        timeZone: TimeZone = .current
    ) -> AttemptLine {
        func time(_ date: Date) -> String {
            FreesideFormat.shortTime(date, now: now, locale: locale, timeZone: timeZone)
        }
        if let stale = staleSince(pool, now: now) {
            return .init(text: "stale since \(time(stale))", isWax: true, instant: stale)
        }
        guard let attempt = pool.last_attempt?.value1 else {
            return .init(text: "no reading", isWax: false, instant: nil)
        }
        switch attempt.status {
        case .succeeded: return .init(text: "read \(time(attempt.at))", isWax: false, instant: attempt.at)
        case .failed: return .init(text: "refresh failed \(time(attempt.at))", isWax: true, instant: attempt.at)
        case .unsupported: return .init(text: "unsupported \(time(attempt.at))", isWax: true, instant: attempt.at)
        case .unknown: return .init(text: "unrecognized reading \(time(attempt.at))", isWax: true, instant: attempt.at)
        }
    }
}
