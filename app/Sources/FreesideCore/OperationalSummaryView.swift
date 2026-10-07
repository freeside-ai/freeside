import FreesideAPI
import SwiftUI

struct OperationalSummary: Equatable {
    enum DaemonState: String, Equatable {
        case checking = "Checking"
        case connected = "Connected"
        case unreachable = "Unreachable"
        case syncFailing = "Sync failing"
        case contractMismatch = "Contract mismatch"
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
    let activeTaskCount: Int
    let daemonState: DaemonState

    init(
        openSnapshots: some Sequence<Components.Schemas.AttentionItemSnapshot>,
        tasks: some Sequence<Components.Schemas.TaskSnapshot>,
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
        activeTaskCount = tasks.count { TaskDisplay.isActive($0.task) }
        daemonState =
            switch freshness {
            case .unvalidated: .checking
            case .fresh: .connected
            case .unreachable: .unreachable
            case .syncFailing: .syncFailing
            case .contractMismatch: .contractMismatch
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

/// The macOS detail column while nothing is selected (6.7): how much is
/// open, what to take first, and the daemon's state folded under one
/// hairline. Every value that names something is a link that opens it: an
/// item in the detail column, the task count on the Tasks screen. A value
/// with nothing to name stays a plain fact.
struct OperationalSummaryView: View {
    let summary: OperationalSummary
    let onSelectItem: (String) -> Void
    let onShowTasks: () -> Void
    /// Nil samples the clock each minute, as the inbox row does; a fixed
    /// value keeps the waiting-longest duration stable for screenshots.
    var now: Date? = nil
    /// The reader's own choice for the Freshness fold; nil follows the
    /// daemon state.
    @State private var freshnessExpanded: Bool?

    var body: some View {
        if let now {
            content(at: now)
        } else {
            TimelineView(.periodic(from: .now, by: 60)) { context in
                content(at: context.date)
            }
        }
    }

    static func openStatement(_ openCount: Int) -> String {
        openCount == 1 ? "1 open item" : "\(openCount) open items"
    }

    private func content(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            VStack(alignment: .leading, spacing: 12) {
                CardEyebrow(keyword: "Inbox")
                Text(Self.openStatement(summary.openCount))
                    .font(FreesideFont.ask)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
            }
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Needs you first")
                itemRow(
                    "Highest Priority",
                    value: summary.highestPriorityTitle.map {
                        "\($0) · \(summary.highestPriorityLabel ?? "")"
                    },
                    itemID: summary.highestPriorityID)
                itemRow(
                    "Waiting Longest",
                    value: summary.waitingLongestValue(now: now),
                    itemID: summary.waitingLongestID)
            }
            VStack(alignment: .leading, spacing: 11) {
                KeywordLabel(text: "Tasks")
                FactLinkRow(
                    label: "Active", value: "\(summary.activeTaskCount)", accessibilityName: "Active tasks",
                    action: onShowTasks)
            }
            SentenceDisclosure(
                label: "Freshness", summary: summary.daemonState.rawValue,
                isExpanded: freshnessBinding
            ) {
                FactRow(
                    label: "Daemon", value: summary.daemonState.rawValue,
                    valueColor: daemonStateColor)
            }
            .padding(.top, 18)
            .overlay(alignment: .top) {
                Color.rule.frame(height: 1)
            }
        }
        .padding(28)
        .frame(minWidth: 320, maxWidth: 560, alignment: .leading)
        // A new daemon state takes the fold back from the reader's last
        // choice, so a daemon that starts failing is never left folded away.
        .onChange(of: summary.daemonState) { freshnessExpanded = nil }
    }

    private var freshnessBinding: Binding<Bool> {
        Binding(
            get: { freshnessExpanded ?? daemonNeedsAttention },
            set: { freshnessExpanded = $0 })
    }

    @ViewBuilder
    private func itemRow(_ label: String, value: String?, itemID: String?) -> some View {
        if let value, let itemID {
            FactLinkRow(label: label, value: value) { onSelectItem(itemID) }
        } else {
            FactRow(label: label, value: "None")
        }
    }

    private var daemonNeedsAttention: Bool {
        switch summary.daemonState {
        case .checking, .connected: false
        case .unreachable, .syncFailing, .contractMismatch, .unauthenticated: true
        }
    }

    private var daemonStateColor: Color {
        daemonNeedsAttention ? .waxText : .ink
    }
}
