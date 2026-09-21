import SwiftUI

/// The §5.14 freshness banner: while the daemon is unreachable, its
/// sync reads are failing, or the credential is rejected, the cached
/// view stays readable and this says so; it never blocks the content it
/// qualifies. Fresh and unvalidated states show nothing.
struct FreshnessBanner: View {
    let freshness: InboxStore.Freshness
    let lastUpdatedAt: Date?
    /// The revoked banner's in-app recovery (#1458). Absent by default, so
    /// every other call site (and the read-only screenshot fixtures) render
    /// the plain informational banner; supplied only where the operator can
    /// act, on `FreesideRootView`'s synced surface.
    let onRePair: (() -> Void)?

    init(
        freshness: InboxStore.Freshness, lastUpdatedAt: Date? = nil,
        onRePair: (() -> Void)? = nil
    ) {
        self.freshness = freshness
        self.lastUpdatedAt = lastUpdatedAt
        self.onRePair = onRePair
    }

    /// The revoked state is the only one whose credential the operator can
    /// clear from inside the app, and the action shows only when a handler
    /// is wired: no other freshness state offers an action.
    static func showsRePairAction(for freshness: InboxStore.Freshness, hasHandler: Bool) -> Bool {
        guard hasHandler else { return false }
        if case .unauthenticated = freshness { return true }
        return false
    }

    var body: some View {
        // Re-evaluate on the minute boundaries measured from the last
        // update, the schedule `LastUpdatedLabel` below already uses. An
        // explicit schedule cannot drive this: SwiftUI dates the very first
        // render at the schedule's own entry, so a single entry at
        // `lastUpdatedAt + stalenessThreshold` left `context.date` already
        // past the threshold at mount and pinned the banner on whenever
        // `lastUpdatedAt` was set (#1130). A periodic schedule instead
        // renders with the latest entry at or before now, and its entries at
        // `lastUpdatedAt + k × stalenessThreshold` are exactly the instants
        // the staleness comparison can flip.
        TimelineView(
            .periodic(from: lastUpdatedAt ?? .now, by: SyncCoordinator.stalenessThreshold)
        ) { context in
            banner(at: context.date)
        }
    }

    @ViewBuilder
    private func banner(at now: Date) -> some View {
        switch freshness {
        case .fresh, .unvalidated:
            if let lastUpdatedAt,
                now.timeIntervalSince(lastUpdatedAt) >= SyncCoordinator.stalenessThreshold
            {
                banner(
                    "The last successful refresh is stale; actions revalidate before use.",
                    keyword: "Stale",
                    tint: .accentText,
                    wash: .accentWash)
            }
        case .unreachable:
            banner(
                "Daemon unreachable — showing cached items; actions are disabled.",
                keyword: "Unreachable",
                tint: .accentText,
                wash: .accentWash
            )
        case .syncFailing:
            // The wash is the softest of the three, so the message keeps
            // the full-contrast foreground; the keyword alone carries the
            // state color.
            banner(
                "Daemon is reachable but sync is failing — showing cached items; actions are disabled.",
                keyword: "Sync failing",
                tint: .accentText,
                wash: .accentWashSoft,
                foreground: .ink
            )
        case .contractMismatch(let daemonContract):
            banner(
                """
                Daemon contract \(ContractDigestDisplay.short(daemonContract)), \
                app built for \(ContractDigestDisplay.shortClient) — update the daemon \
                or the app. Showing cached items; actions are disabled.
                """,
                keyword: "Mismatch",
                tint: .accentText,
                wash: .accentWashSoft,
                foreground: .ink
            )
        case .unauthenticated:
            banner(
                "This device's access was revoked. Cached items stay readable; actions are disabled.",
                keyword: "Revoked",
                tint: .waxText,
                wash: .waxWash,
                action: Self.showsRePairAction(for: freshness, hasHandler: onRePair != nil)
                    ? BannerAction(title: "Pair Again", perform: onRePair ?? {}) : nil
            )
        }
    }

    /// A full-width tinted wash with a leading small-caps mono keyword in
    /// the state color and the message in Plex Sans, text-dim unless the
    /// state passes an explicit high-contrast `foreground`. An optional
    /// trailing `action` renders as a text button in the state color.
    private func banner(
        _ message: String, keyword: String, tint: Color, wash: Color,
        foreground: Color = .inkDim, action: BannerAction? = nil
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            KeywordLabel(text: keyword, color: tint)
            Text(message)
                .font(FreesideFont.callout)
                .foregroundStyle(foreground)
            if let action {
                Spacer(minLength: 12)
                Button(action: action.perform) {
                    Text(action.title)
                        .font(FreesideFont.sans(.callout, weight: .medium))
                        .foregroundStyle(tint)
                }
                .buttonStyle(.plain)
                .accessibilityLabel(action.title)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(wash)
    }

    private struct BannerAction {
        let title: String
        let perform: () -> Void
    }
}

struct LastUpdatedLabel: View {
    let lastUpdatedAt: Date?

    var body: some View {
        // Advance only on minute boundaries measured from the last update:
        // that is when the coarse label (and the stale accent) actually
        // change. During healthy operation `lastUpdatedAt` changes every
        // heartbeat and drives the re-render itself, so this timeline only
        // matters once refreshes stop. Anchoring the period at
        // `lastUpdatedAt` flips the "N min ago" count exactly on the minute.
        TimelineView(.periodic(from: lastUpdatedAt ?? .now, by: 60)) { context in
            HStack(spacing: 4) {
                Image(systemName: "clock")
                    .accessibilityHidden(true)
                Text(Self.text(for: lastUpdatedAt, at: context.date))
            }
            .font(FreesideFont.caption)
            .foregroundStyle(Self.tint(for: lastUpdatedAt, at: context.date))
            .accessibilityElement(children: .combine)
        }
    }

    /// Coarse "last updated" phrasing. While the data is still fresh the
    /// exact age tells the reader nothing, so it reads "Updated recently";
    /// once stale it switches to a minute-or-coarser relative time
    /// ("Updated 2 min ago"). It is deliberately never second-resolution:
    /// the per-second countdown this replaces only ever counted up to the
    /// next heartbeat before resetting, motion with nothing to report.
    static func text(for lastUpdatedAt: Date?, at now: Date) -> String {
        guard let lastUpdatedAt else { return "Not updated yet" }
        guard now.timeIntervalSince(lastUpdatedAt) >= SyncCoordinator.stalenessThreshold
        else { return "Updated recently" }
        // Only ever asked for intervals at/over the staleness threshold, so
        // the largest-unit result never falls back to a seconds unit.
        let formatter = RelativeDateTimeFormatter()
        formatter.unitsStyle = .abbreviated
        formatter.dateTimeStyle = .numeric
        return "Updated \(formatter.localizedString(for: lastUpdatedAt, relativeTo: now))"
    }

    static func tint(for lastUpdatedAt: Date?, at now: Date) -> Color {
        guard let lastUpdatedAt else { return .accentText }
        return now.timeIntervalSince(lastUpdatedAt) >= SyncCoordinator.stalenessThreshold
            ? .accentText : .inkDim
    }
}
