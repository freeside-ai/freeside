import FreesideAPI
import SwiftUI

/// What the standing stopped indicator says, derived from the synced
/// unattended-admission state alone (plan §4 stop_unattended). The state is
/// never inferred from attention items: acknowledging or dismissing the item
/// that raised a stop leaves the indicator standing until the daemon reports
/// admission open again.
struct UnattendedStoppedPresentation: Equatable {
    /// Why admission is closed, by the first stop the daemon lists. An
    /// operator stop is listed first, so it names the indicator whenever one
    /// is in force.
    enum Cause: Equatable {
        case operatorStop
        case blockingFinding
        /// The daemon reports stopped without naming a stop this client can
        /// read; the indicator still stands.
        case unspecified
    }

    let cause: Cause
    /// The item whose decision or resolution reopens admission, when the
    /// daemon names one and it is open in the synced inbox. Nil otherwise,
    /// so the indicator never offers a link with no card to land on.
    let itemID: String?
    /// Stops in force beyond the one the indicator names.
    let additionalStops: Int
    /// False when the snapshot cannot be claimed current (§5.14): restored
    /// from cache, read failing, or older than the staleness threshold. The
    /// indicator then reports the last known state, never the present one.
    let isCurrent: Bool

    /// Nil when there is nothing to show: no bootstrap has supplied the
    /// state, or admission is open.
    @MainActor
    static func make(
        operation: Components.Schemas.UnattendedOperationSnapshot?,
        freshness: InboxStore.Freshness,
        lastUpdatedAt: Date?,
        now: Date,
        reason: (String) -> String?
    ) -> Self? {
        guard let operation, operation.admission == .stopped || !operation.stops.isEmpty
        else { return nil }
        let first = operation.stops.first
        let cause: Cause
        switch first?.kind {
        case .operator_stop: cause = .operatorStop
        case .blocking_system_health: cause = .blockingFinding
        // A drain reads as the generic stop until the app has its own
        // wording for one (#3103).
        case .update_drain, nil: cause = .unspecified
        }
        // The reason only says whether the item is open; its text stays on
        // the item, so a long one never grows the indicator (#2313).
        let itemIsOpen = first?.item_id.flatMap(reason) != nil
        return .init(
            cause: cause,
            itemID: itemIsOpen ? first?.item_id : nil,
            additionalStops: max(0, operation.stops.count - 1),
            isCurrent: isCurrent(freshness: freshness, lastUpdatedAt: lastUpdatedAt, now: now))
    }

    /// Whether a snapshot can be claimed current (§5.14): fresh, and read
    /// within the staleness threshold.
    @MainActor
    static func isCurrent(freshness: InboxStore.Freshness, lastUpdatedAt: Date?, now: Date) -> Bool {
        let withinThreshold =
            lastUpdatedAt.map {
                now.timeIntervalSince($0) < SyncCoordinator.stalenessThreshold
            } ?? false
        return freshness == .fresh && withinThreshold
    }

    var keyword: String { "Stopped" }

    /// Wax only while the stop is current. A cached stop is a record of the
    /// last refresh, so it reads under the freshness banner, not over it.
    var tone: Notice.Tone { isCurrent ? .wax : .neutral }

    /// One sentence naming the cause, plus a count of any other stops. The
    /// item's reason is never in it: the action opens the item, and the
    /// notice stays the same size whatever the daemon wrote.
    var message: String {
        var text: String
        switch (cause, isCurrent) {
        case (.operatorStop, true):
            text = "Unattended operation is stopped by operator decision."
        case (.operatorStop, false):
            text =
                "At the last successful refresh, "
                + "unattended operation was stopped by operator decision."
        case (.blockingFinding, true):
            text = "Unattended operation is stopped by a system health finding."
        case (.blockingFinding, false):
            text =
                "At the last successful refresh, "
                + "unattended operation was stopped by a system health finding."
        case (.unspecified, true):
            text = "Unattended operation is stopped."
        case (.unspecified, false):
            text = "At the last successful refresh, unattended operation was stopped."
        }
        if additionalStops == 1 {
            text += " 1 other stop is in force."
        } else if additionalStops > 1 {
            text += " \(additionalStops) other stops are in force."
        }
        return text
    }

    /// The label on the action: the reopening item's card when one is
    /// open, else System Health itself.
    var actionTitle: String {
        guard itemID != nil else { return "Open System Health" }
        return cause == .operatorStop ? "Review to Resume" : "Open Finding"
    }
}

/// The standing indicator that unattended operation is stopped, shown under
/// the freshness banner wherever that draws on either platform. It reads
/// the same freshness the banner does, so a stale or cached snapshot is
/// reported as the last known state and never as current.
struct UnattendedStoppedIndicator: View {
    let operation: Components.Schemas.UnattendedOperationSnapshot?
    let freshness: InboxStore.Freshness
    let lastUpdatedAt: Date?
    /// The daemon's reason for an open item, by id; nil when the item is not
    /// open in the synced inbox, so the action opens System Health alone.
    let reason: (String) -> String?
    /// Opens System Health, with the reopening item's card when one is open.
    /// Absent where the surface is read-only.
    let onOpenSystemHealth: ((String?) -> Void)?
    var placement = StandingNoticePlacement.window

    var body: some View {
        // The same schedule as `FreshnessBanner`: its entries are the
        // instants the staleness comparison can flip.
        TimelineView(
            .periodic(from: lastUpdatedAt ?? .now, by: SyncCoordinator.stalenessThreshold)
        ) { context in
            if let presentation = UnattendedStoppedPresentation.make(
                operation: operation, freshness: freshness,
                lastUpdatedAt: lastUpdatedAt, now: context.date, reason: reason)
            {
                row(presentation)
            }
        }
    }

    private func row(_ presentation: UnattendedStoppedPresentation) -> some View {
        Notice(
            tone: presentation.tone, keyword: presentation.keyword, sentence: presentation.message,
            action: action(for: presentation)
        )
        .standingNoticeInset(placement)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("unattended-stopped-indicator")
    }

    func action(for presentation: UnattendedStoppedPresentation) -> Notice.Action? {
        guard let onOpenSystemHealth else { return nil }
        let itemID = presentation.itemID
        return .init(label: presentation.actionTitle) { onOpenSystemHealth(itemID) }
    }
}

extension InboxStore {
    /// The daemon's reason on an item that is open in the synced inbox.
    func openItemReason(_ itemID: String) -> String? {
        guard let item = snapshotsByID[itemID]?.item, item.status == .open else { return nil }
        return item.reason
    }
}

extension SyncCoordinator {
    /// How many System Health findings are open, the count the pane, the
    /// iPhone gear badge, and the Mac menu bar line all show; nil until the
    /// inbox has loaded, when a zero would read as nothing open. It counts
    /// open findings, not unseen ones: no acknowledgement reaches the wire
    /// (devlog 2026-11-09-0836-system-health-signals).
    public var openFindingCount: Int? {
        guard store.loadState == .loaded else { return nil }
        return SystemHealthFindings(open: store.openSnapshots, closed: []).openCount
    }

    /// The standing indicator's message for surfaces outside the synced
    /// window (the Mac menu bar); nil when admission is open or unknown.
    public func unattendedStoppedMessage(at now: Date = .now) -> String? {
        UnattendedStoppedPresentation.make(
            operation: unattendedOperation, freshness: store.freshness,
            lastUpdatedAt: lastUpdatedAt, now: now,
            reason: { [store] in store.openItemReason($0) }
        )?.message
    }
}
