import FreesideAPI
import SwiftUI

/// The attention inbox, scoped to open work by default.
struct InboxView: View {
    let store: InboxStore
    @Binding var selection: String?
    let launchScope: InboxStore.Scope?
    let launchProjectID: String?

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
                VStack(spacing: 0) {
                    Picker("Scope", selection: Bindable(store).scope) {
                        ForEach(InboxStore.Scope.allCases) { scope in
                            Text(scope.label).tag(scope)
                        }
                    }
                    .pickerStyle(.segmented)
                    .padding(.horizontal)
                    .padding(.bottom, 8)

                    Picker("Project", selection: projectSelection) {
                        Text("All projects").tag(String?.none)
                        ForEach(store.projects, id: \.self) { project in
                            Text(project).tag(String?.some(project))
                        }
                    }
                    .pickerStyle(.menu)
                    .padding(.horizontal)
                    .padding(.bottom, 8)

                    if store.rows.isEmpty {
                        ContentUnavailableView(
                            "No \(store.scope.label.lowercased()) items",
                            systemImage: "checklist",
                            description: Text("Attention items in this scope will appear here.")
                        )
                    } else {
                        List(store.rows, id: \.item.id, selection: $selection) { snapshot in
                            InboxRowView(item: snapshot.item)
                        }
                    }
                }
            }
        }
        .navigationTitle("Inbox")
        .task {
            Self.applyLaunchFilters(
                to: store, scope: launchScope, projectID: launchProjectID)
        }
        .onChange(of: store.scope) { repairSelection() }
        .onChange(of: store.projectID) { repairSelection() }
        .onChange(of: store.projects) {
            if store.freshness == .fresh {
                store.finishLaunchProjectRepair()
            } else {
                store.repairProjectFilter()
            }
            repairSelection()
        }
        .onChange(of: store.freshness) {
            if store.freshness == .fresh {
                store.finishLaunchProjectRepair()
            }
        }
        .onChange(of: store.rows.map(\.item.id)) { repairSelection() }
    }

    private var projectSelection: Binding<String?> {
        Binding(
            get: { store.projectID },
            set: { store.selectProjectFilter($0) }
        )
    }

    @MainActor
    static func applyLaunchFilters(
        to store: InboxStore,
        scope: InboxStore.Scope?,
        projectID: String?
    ) {
        if let scope { store.scope = scope }
        if let projectID { store.applyLaunchProjectFilter(projectID) }
    }

    private func repairSelection() {
        guard store.loadState == .loaded else { return }
        if let selection, !store.rows.contains(where: { $0.item.id == selection }) {
            self.selection = nil
        }
    }
}

struct InboxRowView: View {
    let item: Components.Schemas.AttentionItem

    var body: some View {
        let subject = AttentionDisplay.subject(item)
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
                Text(subject.lead)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if let identifier = subject.identifier {
                    Text(identifier)
                        .font(.caption.monospaced())
                        .foregroundStyle(.tertiary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
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
