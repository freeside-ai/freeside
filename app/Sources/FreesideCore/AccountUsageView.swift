import FreesideAPI
import SwiftUI

/// Account usage (plan §5.4 usage observation): one card per usage pool,
/// grouped under its provider, saying what is left of each window the
/// daemon recorded, read from sync, with a manual refresh and collector
/// reassignment sent as saved commands. It is Settings' Usage pane on both
/// platforms (`SettingsPaneView`). It draws only what the record holds: a
/// window the collector didn't report is absent, never drawn as
/// unsupported, and nothing is computed or added across pools. The 4 Nov
/// 2026 handoff draws it (`docs/design/handoff-2026-11-04-settings-usage-new-task/`,
/// canvas band 2).
struct AccountUsageView: View {
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    let model: AccountUsageModel
    /// The pools whose Collectors fold is open. Nothing persists it: the
    /// fold opens closed.
    @State private var openCollectors: Set<String> = []

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
                    ForEach(Self.providerGroups(model.pools), id: \.provider) { group in
                        providerGroup(group.provider, pools: group.pools, now: now)
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

    // MARK: Providers

    /// The pools under one keyword per provider, in the order each provider
    /// first appears in the daemon's pool order. The keyword is the record's
    /// provider id; no name is mapped onto it.
    static func providerGroups(
        _ pools: [Components.Schemas.UsagePoolSnapshot]
    ) -> [(provider: String, pools: [Components.Schemas.UsagePoolUsage])] {
        var groups: [(provider: String, pools: [Components.Schemas.UsagePoolUsage])] = []
        for pool in pools.map(\.pool) {
            if let index = groups.firstIndex(where: { $0.provider == pool.provider }) {
                groups[index].pools.append(pool)
            } else {
                groups.append((pool.provider, [pool]))
            }
        }
        return groups
    }

    private func providerGroup(
        _ provider: String, pools: [Components.Schemas.UsagePoolUsage], now: Date
    ) -> some View {
        VStack(alignment: .leading, spacing: FreesideLadder.current.moduleGap) {
            KeywordLabel(text: provider)
                .accessibilityAddTraits(.isHeader)
            VStack(alignment: .leading, spacing: 10) {
                ForEach(pools, id: \.id) { pool in
                    poolCard(pool, now: now)
                }
            }
        }
    }

    // MARK: Pool

    /// One pool: the band, a capacity block (R32) per window the record
    /// names, and the foot with its collectors and Refresh.
    private func poolCard(_ pool: Components.Schemas.UsagePoolUsage, now: Date) -> some View {
        let windows = UsageDisplay.windows(pool)
        let detail = pool.last_attempt?.value1.detail ?? ""
        let parkedRuns = model.parkedRuns(on: pool.id)
        return VStack(alignment: .leading, spacing: 0) {
            band(pool, now: now)
            VStack(alignment: .leading, spacing: 14) {
                if !detail.isEmpty {
                    dimLine(detail)
                } else if windows.isEmpty {
                    // So no card is empty.
                    dimLine("No reading yet.")
                }
                ForEach(windows, id: \.self) { window in
                    let capacity = UsageDisplay.capacity(window: window, in: pool, now: now)
                    CapacityBlock(
                        capacity: capacity,
                        resetLine: capacity.resetsAt.map {
                            UsageDisplay.resetLine(to: $0, now: now, locale: locale, timeZone: timeZone)
                        },
                        parked: UsageDisplay.parkedClause(count: parkedRuns, level: capacity.level))
                }
                foot(pool, now: now)
            }
            .padding(.top, 14)
            .padding(.horizontal, 18)
            .padding(.bottom, 16)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .clipShape(RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    /// The pool's name row on ground-3: its id, its plan, and the
    /// collector's last attempt, or `stale since` in wax once a reading
    /// went stale. Stale is said here once, never on a window. One VoiceOver
    /// element.
    private func band(_ pool: Components.Schemas.UsagePoolUsage, now: Date) -> some View {
        let line = UsageDisplay.attemptLine(pool, now: now, locale: locale, timeZone: timeZone)
        return ViewThatFits(in: .horizontal) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                poolIdentity(pool)
                Spacer(minLength: 0)
                attemptText(line)
            }
            VStack(alignment: .leading, spacing: 3) {
                poolIdentity(pool)
                attemptText(line)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.top, 9)
        .padding(.horizontal, 18)
        .padding(.bottom, 10)
        .background(Color.ground3)
        .overlay(alignment: .bottom) { Color.rule.frame(height: 1) }
        .accessibilityElement(children: .combine)
    }

    private func poolIdentity(_ pool: Components.Schemas.UsagePoolUsage) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(pool.id)
                .font(FreesideFont.statement)
                .foregroundStyle(Color.ink)
            if let plan = pool.plan {
                Text(plan)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
            }
        }
    }

    @ViewBuilder private func attemptText(_ line: UsageDisplay.AttemptLine) -> some View {
        let text = Text(line.text)
            .font(FreesideFont.attachmentFact)
            .foregroundStyle(line.isWax ? Color.waxText : Color.inkFaint)
        if let instant = line.instant {
            text.exactInstant(instant)
        } else {
            text
        }
    }

    // MARK: Collectors

    /// The card's last row: the Collectors fold leading and Refresh
    /// trailing, or Refresh under the fold where the two don't fit one line.
    /// Refresh stays per pool because `refresh_usage` names one pool. The
    /// fold's rows draw under the row at the card's full width, so an open
    /// fold doesn't squeeze them beside the button. The collector is one of
    /// the enrollments, so a pool with none has neither control.
    @ViewBuilder private func foot(_ pool: Components.Schemas.UsagePoolUsage, now: Date) -> some View {
        VStack(alignment: .leading, spacing: FreesideLadder.current.moduleGap) {
            if !pool.enrollments.isEmpty {
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .center, spacing: 12) {
                        collectorsToggle(pool)
                        refreshButton(pool)
                    }
                    VStack(alignment: .leading, spacing: FreesideLadder.current.moduleGap) {
                        collectorsToggle(pool)
                        refreshButton(pool)
                    }
                }
            }
            if pool.collector == nil {
                // Nothing would collect a refresh it asked for.
                dimLine("No enrollment meters this pool, so nothing collects its usage.")
            }
            if openCollectors.contains(pool.id) {
                VStack(alignment: .leading, spacing: 10) {
                    ForEach(pool.enrollments, id: \.enrollment_id) { enrollment in
                        enrollmentRow(enrollment, pool: pool, now: now)
                    }
                }
            }
        }
    }

    /// The fold's toggle alone: its rows draw under the whole row (`foot`).
    /// Every card has one, so its spoken label names the pool, and keeps
    /// the count it replaces.
    private func collectorsToggle(_ pool: Components.Schemas.UsagePoolUsage) -> some View {
        let count = Self.enrollmentCount(pool.enrollments.count)
        return SentenceDisclosure(
            label: "Collectors", summary: count, spokenLabel: "Collectors for \(pool.id), \(count)",
            isExpanded: collectorsBinding(pool.id)
        ) {
            EmptyView()
        }
    }

    @ViewBuilder private func refreshButton(_ pool: Components.Schemas.UsagePoolUsage) -> some View {
        if pool.collector != nil {
            Button("Refresh") { Task { await model.refresh(pool: pool.id) } }
                .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
                .disabled(!model.canSend)
                .accessibilityLabel("Refresh usage for \(pool.id)")
        }
    }

    static func enrollmentCount(_ count: Int) -> String {
        count == 1 ? "1 enrollment" : "\(count) enrollments"
    }

    private func collectorsBinding(_ pool: String) -> Binding<Bool> {
        Binding(
            get: { openCollectors.contains(pool) },
            set: { isOpen in
                if isOpen { openCollectors.insert(pool) } else { openCollectors.remove(pool) }
            })
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
        case .decision, .stop_task, .submit_task, .change_task_line, .set_routing_policy, .move_parked_task:
            "the usage command"
        }
    }

    static func availabilityText(_ answer: Components.Schemas.AvailabilityAnswer) -> String {
        switch answer {
        case .supported: "Supported"
        case .unsupported: "Unsupported"
        case .failed: "Check failed"
        case .unknown: "Unknown"
        }
    }
}
