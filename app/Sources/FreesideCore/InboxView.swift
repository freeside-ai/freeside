import FreesideAPI
import SwiftUI

/// The inbox list: every attention item as a row, open items first.
struct InboxView: View {
    let store: InboxStore
    @Binding var selection: String?

    var body: some View {
        Group {
            switch store.loadState {
            case .idle, .loading:
                ProgressView()
            case .failed(let message):
                ContentUnavailableView {
                    Label("Couldn't load the inbox", systemImage: "exclamationmark.triangle")
                } description: {
                    Text(message)
                }
            case .loaded:
                if store.rows.isEmpty {
                    ContentUnavailableView(
                        "Freeside",
                        systemImage: "checklist",
                        description: Text("Attention items will appear here.")
                    )
                } else {
                    List(store.rows, id: \.item.id, selection: $selection) { snapshot in
                        InboxRowView(item: snapshot.item)
                    }
                }
            }
        }
        .navigationTitle("Inbox")
    }
}

struct InboxRowView: View {
    let item: Components.Schemas.AttentionItem

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Text(AttentionDisplay.title(item._type))
                    .font(.headline)
                Spacer()
                PriorityBadge(priority: item.priority)
            }
            Text(item.reason)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .lineLimit(2)
            HStack(spacing: 8) {
                Text(AttentionDisplay.subject(item.subject))
                    .font(.caption)
                    .foregroundStyle(.tertiary)
                if item.status != .open {
                    StatusBadge(status: item.status)
                }
            }
        }
        .padding(.vertical, 2)
    }
}

struct PriorityBadge: View {
    let priority: Components.Schemas.Priority

    var body: some View {
        Text(AttentionDisplay.label(priority))
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(color.opacity(0.15), in: Capsule())
            .foregroundStyle(color)
    }

    private var color: Color {
        switch priority {
        case .urgent: return .red
        case .high: return .orange
        case .normal: return .blue
        case .low: return .gray
        }
    }
}

struct StatusBadge: View {
    let status: Components.Schemas.ItemStatus

    var body: some View {
        Text(AttentionDisplay.label(status))
            .font(.caption2.weight(.medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(.quaternary, in: Capsule())
            .foregroundStyle(.secondary)
    }
}
