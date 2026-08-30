import SwiftUI

/// The §5.14 freshness banner: while the daemon is unreachable, its
/// sync reads are failing, or the credential is rejected, the cached
/// view stays readable and this says so; it never blocks the content it
/// qualifies. Fresh and unvalidated states show nothing.
struct FreshnessBanner: View {
    let freshness: InboxStore.Freshness
    let lastUpdatedAt: Date?

    init(freshness: InboxStore.Freshness, lastUpdatedAt: Date? = nil) {
        self.freshness = freshness
        self.lastUpdatedAt = lastUpdatedAt
    }

    var body: some View {
        TimelineView(.explicit(Self.staleAt(lastUpdatedAt: lastUpdatedAt).map { [$0] } ?? [])) {
            context in
            banner(at: context.date)
        }
    }

    static func staleAt(lastUpdatedAt: Date?) -> Date? {
        lastUpdatedAt.map { $0.addingTimeInterval(SyncCoordinator.stalenessThreshold) }
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
                    tint: .waxText,
                    wash: .waxWash)
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
        case .unauthenticated:
            banner(
                "This device's access was revoked. Cached items stay readable; actions are disabled.",
                keyword: "Revoked",
                tint: .waxText,
                wash: .waxWash
            )
        }
    }

    /// A full-width tinted wash with a leading small-caps mono keyword in
    /// the state color and the message in Plex Sans, text-dim unless the
    /// state passes an explicit high-contrast `foreground`.
    private func banner(
        _ message: String, keyword: String, tint: Color, wash: Color, foreground: Color = .inkDim
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            KeywordLabel(text: keyword, color: tint)
            Text(message)
                .font(FreesideFont.callout)
                .foregroundStyle(foreground)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(wash)
    }
}

struct LastUpdatedLabel: View {
    let lastUpdatedAt: Date?

    var body: some View {
        // Advance only on minute boundaries measured from the last update:
        // that is when the coarse label (and the stale color) actually
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
            .foregroundStyle(isStale(at: context.date) ? Color.waxText : Color.inkDim)
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

    private func isStale(at now: Date) -> Bool {
        guard let lastUpdatedAt else { return true }
        return now.timeIntervalSince(lastUpdatedAt) >= SyncCoordinator.stalenessThreshold
    }
}
