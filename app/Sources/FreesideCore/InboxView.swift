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
                        ContentUnavailableView {
                            Label {
                                Text("No \(store.scope.label.lowercased()) items")
                                    .font(FreesideFont.title)
                            } icon: {
                                Image(systemName: "checklist")
                            }
                        } description: {
                            Text("Attention items in this scope will appear here.")
                                .font(FreesideFont.callout)
                        }
                        .foregroundStyle(Color.inkDim)
                    } else {
                        List(store.rows, id: \.item.id, selection: $selection) { snapshot in
                            InboxRowView(item: snapshot.item, isSelected: selection == snapshot.item.id)
                                .listRowInsets(EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12))
                                .listRowSeparator(.hidden)
                                .listRowBackground(Color.clear)
                        }
                        .listStyle(.plain)
                        .scrollContentBackground(.hidden)
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

/// One inbox row as a ground-2 card; the selected row's border turns
/// accent-dim in place of the platform selection highlight.
struct InboxRowView: View {
    let item: Components.Schemas.AttentionItem
    var isSelected = false

    var body: some View {
        let subject = AttentionDisplay.subject(item)
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Text(AttentionDisplay.title(item))
                    .font(FreesideFont.itemTitle)
                    .foregroundStyle(Color.ink)
                Spacer()
                PriorityBadge(priority: item.priority)
            }
            Text(item.reason)
                .font(FreesideFont.subheadline)
                .foregroundStyle(Color.inkDim)
                .lineLimit(2)
            HStack(spacing: 8) {
                Text(subject.lead)
                    .font(FreesideFont.caption)
                    .foregroundStyle(Color.inkDim)
                if let identifier = subject.identifier {
                    Text(identifier)
                        .font(FreesideFont.monoCaption)
                        .foregroundStyle(Color.inkDim)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                if item.status != .open {
                    StatusBadge(status: item.status)
                }
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .freesideCard(border: isSelected ? .accentBorder : .rule)
    }
}

/// Urgent is wax, high is the accent, normal is water, low is faint.
struct PriorityBadge: View {
    let priority: Components.Schemas.Priority

    var body: some View {
        StateChip(label: AttentionDisplay.label(priority), color: color)
    }

    private var color: Color {
        switch priority {
        case .urgent: return .waxText
        case .high: return .accentText
        case .normal: return .waterText
        case .low: return .inkDim
        }
    }
}

struct StatusBadge: View {
    let status: Components.Schemas.ItemStatus

    var body: some View {
        StateChip(label: AttentionDisplay.label(status), color: .inkDim)
    }
}
