import FreesideAPI
import SwiftUI

struct OperationalSummary: Equatable {
    enum DaemonState: String, Equatable {
        case checking = "Checking"
        case connected = "Connected"
        case unreachable = "Unreachable"
        case syncFailing = "Sync failing"
        case unauthenticated = "Pairing required"
    }

    let openCount: Int
    let highestPriorityID: String?
    let highestPriorityTitle: String?
    let highestPriorityLabel: String?
    let waitingLongestID: String?
    let waitingLongestTitle: String?
    /// The item itself, so the row can show the same time text its inbox
    /// row does: a deadline first, else the wait from the row's origin.
    let waitingLongestItem: Components.Schemas.AttentionItem?
    let activeRunCount: Int
    let daemonState: DaemonState

    init(
        openSnapshots: some Sequence<Components.Schemas.AttentionItemSnapshot>,
        runs: some Sequence<Components.Schemas.RunSnapshot>,
        freshness: InboxStore.Freshness
    ) {
        let openItems = openSnapshots.map(\.item)
        let highestPriority = openItems.min { lhs, rhs in
            let lhsRank = Self.priorityRank(lhs.priority)
            let rhsRank = Self.priorityRank(rhs.priority)
            if lhsRank != rhsRank { return lhsRank < rhsRank }
            let lhsCreated = lhs.created_at ?? .distantFuture
            let rhsCreated = rhs.created_at ?? .distantFuture
            if lhsCreated != rhsCreated { return lhsCreated < rhsCreated }
            return lhs.id < rhs.id
        }
        // Ordered by the displayed wait, not creation: a blocked item's
        // wait predates its card, and the pick must be the row whose
        // duration reads longest.
        let waitingLongest = openItems.min { lhs, rhs in
            let lhsSince = AttentionDisplay.rowTimeOrigin(lhs) ?? .distantFuture
            let rhsSince = AttentionDisplay.rowTimeOrigin(rhs) ?? .distantFuture
            if lhsSince != rhsSince { return lhsSince < rhsSince }
            return lhs.id < rhs.id
        }

        openCount = openItems.count
        highestPriorityID = highestPriority?.id
        highestPriorityTitle = highestPriority.map(AttentionDisplay.title)
        highestPriorityLabel = highestPriority.map { AttentionDisplay.label($0.priority) }
        waitingLongestID = waitingLongest?.id
        waitingLongestTitle = waitingLongest.map(AttentionDisplay.title)
        waitingLongestItem = waitingLongest
        activeRunCount = runs.count { $0.run.lifecycle == .active }
        daemonState =
            switch freshness {
            case .unvalidated: .checking
            case .fresh: .connected
            case .unreachable: .unreachable
            case .syncFailing: .syncFailing
            case .unauthenticated: .unauthenticated
            }
    }

    /// The waiting-longest row's value: the item's title and the time text
    /// its inbox row shows, so the two never disagree; the title alone when
    /// the row shows no time.
    func waitingLongestValue(now: Date) -> String? {
        guard let item = waitingLongestItem else { return nil }
        let title = AttentionDisplay.title(item)
        guard let rowTime = AttentionDisplay.relativeRowTime(item, now: now) else { return title }
        return "\(title) · \(rowTime)"
    }

    private static func priorityRank(_ priority: Components.Schemas.Priority) -> Int {
        switch priority {
        case .urgent: 0
        case .high: 1
        case .normal: 2
        case .low: 3
        }
    }
}

/// The macOS detail column while nothing is selected. Every row that
/// names something opens it: an item in the detail column, the run count
/// on the Runs screen. A row with nothing to name stays a plain fact.
struct OperationalSummaryView: View {
    let summary: OperationalSummary
    let onSelectItem: (String) -> Void
    let onShowRuns: () -> Void
    /// Nil samples the clock each minute, as the inbox row does; a fixed
    /// value keeps the waiting-longest duration stable for screenshots.
    var now: Date? = nil

    var body: some View {
        if let now {
            content(at: now)
        } else {
            TimelineView(.periodic(from: .now, by: 60)) { context in
                content(at: context.date)
            }
        }
    }

    private func content(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 18) {
            VStack(alignment: .leading, spacing: 5) {
                Text("Freeside")
                    .font(FreesideFont.title)
                    .foregroundStyle(Color.ink)
                Text("Operational summary")
                    .font(FreesideFont.monoCaption)
                    .foregroundStyle(Color.inkDim)
            }

            VStack(alignment: .leading, spacing: 12) {
                summaryRow("Open decisions", value: "\(summary.openCount)")
                itemRow(
                    "Highest priority",
                    value: summary.highestPriorityTitle.map {
                        "\($0) · \(summary.highestPriorityLabel ?? "")"
                    },
                    itemID: summary.highestPriorityID)
                itemRow(
                    "Waiting longest",
                    value: summary.waitingLongestValue(now: now),
                    itemID: summary.waitingLongestID)
                navigableRow("Active runs", value: "\(summary.activeRunCount)", action: onShowRuns)
                summaryRow(
                    "Daemon", value: summary.daemonState.rawValue,
                    valueColor: daemonStateColor)
            }
            .padding(16)
            .freesideCard()
        }
        .padding(28)
        .frame(maxWidth: 560, alignment: .leading)
    }

    @ViewBuilder
    private func itemRow(_ label: String, value: String?, itemID: String?) -> some View {
        if let value, let itemID {
            navigableRow(label, value: value) { onSelectItem(itemID) }
        } else {
            summaryRow(label, value: "None")
        }
    }

    /// The fact row as a plain button with the chevron the decision card's
    /// navigable rows carry, so the card keeps its fact-row look.
    private func navigableRow(
        _ label: String, value: String, action: @escaping () -> Void
    ) -> some View {
        Button(action: action) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                summaryRow(label, value: value)
                Image(systemName: "chevron.right")
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.accentText)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Open \(label): \(value)")
    }

    private func summaryRow(
        _ label: String,
        value: String,
        valueColor: Color = .ink
    ) -> some View {
        FactRow(label: label, value: value, valueColor: valueColor)
            .font(FreesideFont.callout)
            .foregroundStyle(Color.inkDim)
    }

    private var daemonStateColor: Color {
        switch summary.daemonState {
        case .checking, .connected: .ink
        case .unreachable, .syncFailing, .unauthenticated: .waxText
        }
    }
}
