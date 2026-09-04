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
    let highestPriorityTitle: String?
    let highestPriorityLabel: String?
    let waitingLongestTitle: String?
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
        let waitingLongest = openItems.min { lhs, rhs in
            let lhsCreated = lhs.created_at ?? .distantFuture
            let rhsCreated = rhs.created_at ?? .distantFuture
            if lhsCreated != rhsCreated { return lhsCreated < rhsCreated }
            return lhs.id < rhs.id
        }

        openCount = openItems.count
        highestPriorityTitle = highestPriority.map(AttentionDisplay.title)
        highestPriorityLabel = highestPriority.map { AttentionDisplay.label($0.priority) }
        waitingLongestTitle = waitingLongest.map(AttentionDisplay.title)
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

    private static func priorityRank(_ priority: Components.Schemas.Priority) -> Int {
        switch priority {
        case .urgent: 0
        case .high: 1
        case .normal: 2
        case .low: 3
        }
    }
}

struct OperationalSummaryView: View {
    let summary: OperationalSummary

    var body: some View {
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
                summaryRow(
                    "Highest priority",
                    value: summary.highestPriorityTitle.map {
                        "\($0) · \(summary.highestPriorityLabel ?? "")"
                    } ?? "None")
                summaryRow("Waiting longest", value: summary.waitingLongestTitle ?? "None")
                summaryRow("Active runs", value: "\(summary.activeRunCount)")
                summaryRow(
                    "Daemon", value: summary.daemonState.rawValue,
                    valueColor: daemonStateColor)
            }
            .padding(16)
            .freesideCard()

            Label("Select an item to decide.", systemImage: "arrow.left")
                .font(FreesideFont.callout)
                .foregroundStyle(Color.inkDim)
        }
        .padding(28)
        .frame(maxWidth: 560, alignment: .leading)
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
