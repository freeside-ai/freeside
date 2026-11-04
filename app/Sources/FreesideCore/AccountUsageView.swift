import FreesideAPI
import SwiftUI

/// Account usage (plan §5.4 usage observation): per usage pool, what the
/// daemon recorded about it, read from sync, with a manual refresh and
/// collector reassignment sent as saved commands. `SettingsSheet` hosts it on
/// both platforms, as it hosts Routing. It draws only what the record holds:
/// a window the collector didn't report is absent, never drawn as
/// unsupported, and nothing is computed or added across pools.
struct AccountUsageView: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    let model: AccountUsageModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Usage",
                ask: "How much of each account is used",
                consequence:
                    "What the daemon last read from each provider. A stale reading is shown as stale, never as replenished.",
                askLineLimit: nil)
            // The countdowns move with the clock; a pinned clock (screenshots)
            // holds them still.
            TimelineView(.periodic(from: .now, by: 60)) { context in
                let now = pinnedNow ?? context.date
                VStack(alignment: .leading, spacing: 16) {
                    notices
                    if let reason = model.unavailableReason {
                        dimLine(reason)
                    }
                    if model.pools.isEmpty {
                        dimLine("The daemon declares no usage pools.")
                    }
                    ForEach(model.pools, id: \.pool.id) { snapshot in
                        poolCard(snapshot, now: now)
                    }
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
    }

    // MARK: Notices

    @ViewBuilder private var notices: some View {
        if let failure = model.failure {
            Notice(tone: .wax, keyword: "Failed", sentence: failure)
                .accessibilityElement(children: .combine)
        }
        if let pending = model.pending {
            if pending.receipt == nil {
                Notice(
                    tone: .accent, keyword: "Unconfirmed",
                    sentence: model.sending
                        ? "Sending \(Self.describe(pending)) to the daemon…"
                        : "The daemon hasn't answered \(Self.describe(pending)). Retry sends the same command to recover its result.",
                    action: .init(
                        label: "Retry", accessibilityLabel: "Retry sending \(Self.describe(pending))",
                        isEnabled: !model.sending
                    ) { Task { await model.retry() } })
            } else {
                Notice(
                    tone: .neutral, keyword: "Accepted",
                    sentence: "The daemon accepted \(Self.describe(pending)). Waiting for sync to show it."
                )
                .accessibilityElement(children: .combine)
            }
        } else if let settled = model.settled {
            switch settled.record {
            case .refresh_usage(let record):
                Notice(
                    tone: .neutral, keyword: "Requested",
                    sentence: "Refresh requested for \(record.pool). New readings arrive through sync."
                )
                .accessibilityElement(children: .combine)
            case .reassign_collector(let record):
                Notice(
                    tone: .neutral, keyword: "Recorded",
                    sentence: "\(record.collector) collects for \(record.pool) now."
                )
                .accessibilityElement(children: .combine)
            default:
                EmptyView()
            }
        }
    }

    // MARK: Pool

    private func poolCard(_ snapshot: Components.Schemas.UsagePoolSnapshot, now: Date) -> some View {
        let pool = snapshot.pool
        let windows = UsageDisplay.windows(pool)
        return VStack(alignment: .leading, spacing: 10) {
            VStack(alignment: .leading, spacing: 3) {
                KeywordLabel(text: pool.id)
                    .accessibilityAddTraits(.isHeader)
                Text([pool.provider, pool.plan].compactMap(\.self).joined(separator: " · "))
                    .font(FreesideFont.monoValue)
                    .foregroundStyle(Color.ink)
            }
            .accessibilityElement(children: .combine)
            refreshStatus(snapshot, now: now)
            ForEach(windows, id: \.self) { window in
                windowRow(window, pool: pool, now: now)
            }
            if pool.collector == nil {
                // Nothing would collect a refresh it asked for.
                dimLine("No enrollment meters this pool, so nothing collects its usage.")
            } else {
                Button("Refresh") { Task { await model.refresh(pool: pool.id) } }
                    .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                    .disabled(!model.canSend)
                    .accessibilityLabel("Refresh usage for \(pool.id)")
            }
            if !pool.enrollments.isEmpty {
                collector(pool, now: now)
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    /// The collector's last attempt, or that the pool has no reading.
    @ViewBuilder private func refreshStatus(
        _ snapshot: Components.Schemas.UsagePoolSnapshot, now: Date
    ) -> some View {
        if let attempt = snapshot.pool.last_attempt?.value1 {
            let (keyword, color) = Self.attemptKeyword(attempt.status)
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    KeywordLabel(text: keyword, color: color)
                    Text(time(attempt.at, now: now))
                        .font(FreesideFont.monoValue)
                        .foregroundStyle(Color.inkDim)
                        .exactInstant(attempt.at)
                }
                if !attempt.detail.isEmpty {
                    dimLine(attempt.detail)
                }
            }
            .accessibilityElement(children: .combine)
        } else {
            dimLine("No reading yet.")
        }
    }

    // MARK: Window

    /// One window as one VoiceOver element: label, share or status, stale,
    /// used up, reset, and observation, in that order.
    @ViewBuilder private func windowRow(
        _ window: String, pool: Components.Schemas.UsagePoolUsage, now: Date
    ) -> some View {
        let reading = pool.windows.first { $0.window == window }
        let stale = reading.map { UsageDisplay.isStale($0, now: now) } ?? false
        let exhausted = UsageDisplay.isExhausted(window: window, marks: pool.recorded_exhausted_marks, now: now)
        let models = UsageDisplay.models(limitedBy: window, in: pool)
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                windowName(window)
                Spacer(minLength: 0)
                if stale { KeywordLabel(text: "Stale", color: .waxText) }
                if exhausted { KeywordLabel(text: "Used up", color: .waxText) }
                Text(reading.map(UsageDisplay.value) ?? "No reading")
                    .font(FreesideFont.monoValue)
                    .foregroundStyle(reading == nil ? Color.inkDim : Color.ink)
            }
            if !models.isEmpty {
                monoLine("Limits \(models.joined(separator: ", "))")
            }
            if let resets = reading?.resets_at {
                monoLine("Resets \(time(resets, now: now)) · \(UsageDisplay.countdown(to: resets, now: now))")
                    .exactInstant(resets)
            }
            ForEach(
                Array(UsageDisplay.uncoveredMarks(window: window, in: pool, now: now).enumerated()), id: \.offset
            ) { _, mark in
                markLine(mark, now: now)
            }
            if let reading {
                monoLine("Observed \(time(reading.observed_at, now: now))")
                    .exactInstant(reading.observed_at)
            }
        }
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder private func markLine(_ mark: Components.Schemas.UsageExhaustedMark, now: Date) -> some View {
        let source = mark.source == .provider_refusal ? "Refused" : "Read exhausted"
        if let resets = mark.resets_at {
            monoLine("\(source) until \(time(resets, now: now)) · \(UsageDisplay.countdown(to: resets, now: now))")
                .exactInstant(resets)
        } else {
            monoLine("\(source), no reset recorded")
        }
    }

    @ViewBuilder private func windowName(_ window: String) -> some View {
        if let label = UsageDisplay.label(window) {
            Text(label)
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.ink)
        } else {
            Text(window)
                .font(FreesideFont.monoValue)
                .foregroundStyle(Color.ink)
        }
    }

    // MARK: Collector

    private func collector(_ pool: Components.Schemas.UsagePoolUsage, now: Date) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            KeywordLabel(text: "Collector")
                .accessibilityAddTraits(.isHeader)
            ForEach(pool.enrollments, id: \.enrollment_id) { enrollment in
                enrollmentRow(enrollment, pool: pool, now: now)
            }
        }
    }

    private func enrollmentRow(
        _ enrollment: Components.Schemas.UsagePoolEnrollment, pool: Components.Schemas.UsagePoolUsage, now: Date
    ) -> some View {
        let isCollector = enrollment.enrollment_id == pool.collector
        return VStack(alignment: .leading, spacing: 4) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(enrollment.enrollment_id)
                        .font(FreesideFont.monoValue)
                        .foregroundStyle(Color.ink)
                    if isCollector { KeywordLabel(text: "Collects", color: .accentText) }
                }
                monoLine("\(enrollment.auth_identity_id) · \(enrollment.harness_client.rawValue)")
                if let availability = enrollment.availability?.value1 {
                    monoLine(
                        "\(Self.availabilityText(availability.answer)) · observed \(time(availability.observed_at, now: now))"
                    )
                    .exactInstant(availability.observed_at)
                } else {
                    monoLine("No answer recorded")
                }
            }
            .accessibilityElement(children: .combine)
            if !isCollector {
                Button("Make Collector") {
                    Task { await model.reassign(pool: pool.id, to: enrollment.enrollment_id) }
                }
                .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
                .disabled(!model.canSend)
                .accessibilityLabel("Make \(enrollment.enrollment_id) the collector for \(pool.id)")
            }
        }
    }

    // MARK: Shared pieces

    private func time(_ date: Date, now: Date) -> String {
        FreesideFormat.shortTime(date, now: now, locale: locale, timeZone: timeZone)
    }

    private func monoLine(_ text: String) -> some View {
        Text(text)
            .font(FreesideFont.monoValue)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }

    private func dimLine(_ text: String) -> some View {
        Text(text)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }

    /// The saved command in words, so a notice says what Retry resends.
    static func describe(_ pending: PendingUsageCommand) -> String {
        switch pending.command.payload {
        case .refresh_usage(let payload): "the refresh of \(payload.pool)"
        case .reassign_collector(let payload): "making \(payload.enrollment_id) the collector for \(payload.pool)"
        case .decision, .stop_task, .submit_task, .change_task_line, .set_routing_policy: "the usage command"
        }
    }

    static func attemptKeyword(_ status: Components.Schemas.CollectionAttemptStatus) -> (String, Color) {
        switch status {
        case .succeeded: ("Refreshed", .inkDim)
        case .failed: ("Refresh failed", .waxText)
        case .unsupported: ("Unsupported", .waxText)
        }
    }

    static func availabilityText(_ answer: Components.Schemas.AvailabilityAnswer) -> String {
        switch answer {
        case .supported: "Supported"
        case .unsupported: "Unsupported"
        case .failed: "Check failed"
        }
    }
}
