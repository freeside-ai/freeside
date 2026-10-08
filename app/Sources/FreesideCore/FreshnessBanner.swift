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
                Notice(
                    tone: .accent, keyword: "Stale",
                    sentence: "The last successful refresh is stale; actions revalidate before use."
                )
                .standingNoticeInset()
            }
        case .unreachable:
            Notice(
                tone: .accent, keyword: "Unreachable",
                sentence: "Daemon unreachable. Showing cached items; actions are disabled."
            )
            .standingNoticeInset()
        case .syncFailing:
            Notice(
                tone: .accent, keyword: "Sync failing",
                sentence:
                    "Daemon is reachable but sync is failing. Showing cached items; actions are disabled."
            )
            .standingNoticeInset()
        case .contractMismatch(let daemonContract):
            let mismatch = ContractMismatchSentence(
                daemonContract: daemonContract,
                tail: " Showing cached items; actions are disabled.")
            Notice(
                tone: .accent, keyword: "Mismatch",
                sentence: mismatch.plain, drawn: mismatch.text
            )
            .standingNoticeInset()
        case .unauthenticated:
            Notice(
                tone: .wax, keyword: "Revoked",
                sentence:
                    "This device's access was revoked. Cached items stay readable; actions are disabled.",
                action: Self.showsRePairAction(for: freshness, hasHandler: onRePair != nil)
                    ? .init(label: "Pair Again", handler: onRePair ?? {}) : nil
            )
            .standingNoticeInset()
        }
    }
}

/// The contract-mismatch sentence the freshness banner and the menu-bar
/// panel both draw: the two short digests in mono inside a sans sentence,
/// and the fix. `tail` is what the surface adds about its own state.
struct ContractMismatchSentence {
    let daemonContract: String
    var tail = ""

    private var daemon: String { ContractDigestDisplay.short(daemonContract) }
    private var app: String { ContractDigestDisplay.shortClient }

    var plain: String {
        "Daemon contract \(daemon), app \(app). Update the daemon or the app.\(tail)"
    }

    var text: Text {
        Text(
            "Daemon contract \(Text(daemon).font(FreesideFont.trailingSummary)), app \(Text(app).font(FreesideFont.trailingSummary)). Update the daemon or the app.\(tail)"
        )
    }
}

extension View {
    /// The inset a standing notice above the synced surface takes: 16pt
    /// from each side and 4pt above and below, so two notices sit 8pt
    /// apart. Each notice carries its own because they come and go
    /// independently, and a container's padding would outlive them.
    func standingNoticeInset() -> some View {
        padding(.horizontal, 16).padding(.vertical, 4)
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
