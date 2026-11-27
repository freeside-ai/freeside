import Foundation
import FreesideAPI

/// The System Health pane's findings: the synced `system_health` items, open
/// and closed, as a pure projection of what the client already holds. The
/// Unified List leaves these items out (`UnifiedList`), so this is the one
/// place a client lists them.
package struct SystemHealthFindings: Equatable {
    /// Open findings, blocking before advisory and newest first within
    /// each, so a finding that stops unattended work leads whatever its
    /// age.
    package let open: [Components.Schemas.AttentionItem]
    /// Closed findings, newest first by when they closed (`closedAt`).
    package let closed: [Components.Schemas.AttentionItem]

    /// `open` and `closed` are `InboxStore.openSnapshots` and
    /// `closedSnapshots`, unfiltered: a finding concerns the daemon, so no
    /// project filter applies.
    package init(
        open: [Components.Schemas.AttentionItemSnapshot],
        closed: [Components.Schemas.AttentionItemSnapshot]
    ) {
        let isFinding = { (item: Components.Schemas.AttentionItem) in item._type == .system_health }
        self.open = Self.newestFirst(open.map(\.item).filter(isFinding), by: \.created_at)
            .enumerated()
            .sorted { lhs, rhs in
                let (lhsBlocking, rhsBlocking) = (Self.isBlocking(lhs.element), Self.isBlocking(rhs.element))
                return lhsBlocking != rhsBlocking ? lhsBlocking : lhs.offset < rhs.offset
            }
            .map(\.element)
        self.closed = Self.newestFirst(closed.map(\.item).filter(isFinding), by: Self.closedAt)
    }

    package var openCount: Int { open.count }

    package var blockingCount: Int { open.filter(Self.isBlocking).count }

    /// The word a row's keyword line carries: the posture of an open
    /// finding, the outcome of a closed one, in the words the closed card
    /// uses.
    package static func keyword(_ item: Components.Schemas.AttentionItem) -> String {
        guard item.status == .open else { return AttentionDisplay.label(item.status) }
        return AttentionDisplay.label(item.posture?.value1 ?? .advisory)
    }

    /// What an open finding does to unattended admission, read beside its
    /// keyword: a blocking finding stops it, an advisory one names the
    /// capability it affects and says it does not stop it. Only the pane's
    /// header states whether admission is open (#3678). A closed finding
    /// has none: its keyword is its outcome, and it stops nothing.
    package static func effect(_ item: Components.Schemas.AttentionItem) -> String? {
        guard item.status == .open else { return nil }
        guard !isBlocking(item) else { return "stops unattended admission" }
        let unstopped = "does not stop admission"
        guard let capability = item.health_diagnostic?.value1.impairs else { return unstopped }
        let affected: String
        switch capability {
        case .unattended_admission: affected = "unattended admission"
        case .run_visibility: affected = "run visibility"
        case .agent_credential: affected = "agent credential"
        case .unknown: affected = "an unrecognized capability"
        case .none: return unstopped
        }
        return "affects \(affected) · \(unstopped)"
    }

    /// When a closed finding closed: its conclusion's instant, which a
    /// daemon resolution carries with no `decided_at`; else the decision's,
    /// else its creation, for an item recorded before conclusions were.
    package static func closedAt(_ item: Components.Schemas.AttentionItem) -> Date? {
        item.conclusion?.value1.concluded_at ?? item.decided_at ?? item.created_at
    }

    /// A finding with no posture is advisory: only a blocking one closes
    /// unattended admission (plan §4). A posture this build doesn't know
    /// counts as blocking, so the app never plays down a finding.
    package static func isBlocking(_ item: Components.Schemas.AttentionItem) -> Bool {
        switch item.posture?.value1 {
        case .blocking?, .unknown?: true
        case .advisory?, nil: false
        }
    }

    /// Newest first by `instant`, an undated item last, ties in the order
    /// given.
    private static func newestFirst(
        _ items: [Components.Schemas.AttentionItem], by instant: (Components.Schemas.AttentionItem) -> Date?
    ) -> [Components.Schemas.AttentionItem] {
        items.enumerated().sorted { lhs, rhs in
            let (lhsDate, rhsDate) = (instant(lhs.element), instant(rhs.element))
            guard lhsDate != rhsDate else { return lhs.offset < rhs.offset }
            return (lhsDate ?? .distantPast) > (rhsDate ?? .distantPast)
        }.map(\.element)
    }
}
