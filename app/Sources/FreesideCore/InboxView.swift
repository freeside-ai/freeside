import Foundation
import FreesideAPI
import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

/// The attention inbox, scoped to open work by default.
struct InboxView: View {
    static func orderingCaption(for scope: InboxStore.Scope) -> String {
        let open =
            "Newest first; overdue leads. Priority breaks ties within the past hour, past day, and older."
        let resolved =
            "Newest decision first, using creation time when no decision time is available."
        switch scope {
        case .open: return open
        case .resolved: return resolved
        case .all: return "Open items first. \(open) Resolved items follow. \(resolved)"
        }
    }

    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    #if os(macOS)
        @FocusedValue(\.decisionCommandActions) private var decisionCommandActions
    #endif
    let store: InboxStore
    @Binding var selection: String?
    let launchScope: InboxStore.Scope?
    let launchProjectID: String?
    private let interactiveSelection: Binding<String?>?
    private let navigationPath: Binding<[String]>?
    private let onFilterChange: () -> Void
    private let onMoveSelection: (Int) -> Void
    private let onRefresh: @MainActor () async -> Void
    private let lastUpdatedAt: Date?
    var onRevealTechnicalDetails: (String) -> Void

    init(
        store: InboxStore,
        selection: Binding<String?>,
        launchScope: InboxStore.Scope?,
        launchProjectID: String?,
        interactiveSelection: Binding<String?>? = nil,
        navigationPath: Binding<[String]>? = nil,
        onFilterChange: @escaping () -> Void = {},
        onMoveSelection: @escaping (Int) -> Void = { _ in },
        lastUpdatedAt: Date? = nil,
        onRefresh: @escaping @MainActor () async -> Void = {},
        onRevealTechnicalDetails: @escaping (String) -> Void = { _ in }
    ) {
        self.store = store
        _selection = selection
        self.launchScope = launchScope
        self.launchProjectID = launchProjectID
        self.interactiveSelection = interactiveSelection
        self.navigationPath = navigationPath
        self.onFilterChange = onFilterChange
        self.onMoveSelection = onMoveSelection
        self.onRefresh = onRefresh
        self.lastUpdatedAt = lastUpdatedAt
        self.onRevealTechnicalDetails = onRevealTechnicalDetails
    }

    var body: some View {
        Group {
            switch store.loadState {
            case .idle, .loading:
                ProgressView()
            case .failed(let message):
                UnavailableStateView(
                    title: "Couldn't load the inbox",
                    systemImage: "exclamationmark.triangle",
                    description: message)
            case .loaded:
                VStack(spacing: 0) {
                    scopeBar
                        .padding(.horizontal)
                        .padding(.bottom, 8)

                    orderingCaption
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
                        UnavailableStateView(
                            title: "No \(store.scope.label.lowercased()) items",
                            systemImage: "checklist",
                            description: "Attention items in this scope will appear here.")
                    } else {
                        #if os(iOS)
                            List(store.rows, id: \.item.id) { snapshot in
                                NavigationLink(value: snapshot.item.id) {
                                    InboxRowView(
                                        item: snapshot.item,
                                        onRevealTechnicalDetails: {
                                            selection = snapshot.item.id
                                            onRevealTechnicalDetails(snapshot.item.id)
                                        })
                                }
                                .listRowInsets(
                                    EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12)
                                )
                                .listRowSeparator(.hidden)
                                .listRowBackground(Color.clear)
                            }
                            .listStyle(.plain)
                            .scrollContentBackground(.hidden)
                        #else
                            List(
                                store.rows, id: \.item.id,
                                selection: interactiveSelection ?? $selection
                            ) { snapshot in
                                InboxRowView(
                                    item: snapshot.item,
                                    isSelected: selection == snapshot.item.id,
                                    onRevealTechnicalDetails: {
                                        selection = snapshot.item.id
                                        onRevealTechnicalDetails(snapshot.item.id)
                                    }
                                )
                                .hidesSystemListSelection()
                                .listRowInsets(
                                    EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12)
                                )
                                .listRowSeparator(.hidden)
                                .listRowBackground(Color.clear)
                            }
                            .listStyle(.plain)
                            .scrollContentBackground(.hidden)
                            .onKeyPress(
                                characters: CharacterSet(charactersIn: "jk"), phases: .down
                            ) { press in
                                guard press.modifiers.isEmpty else { return .ignored }
                                onMoveSelection(press.characters == "j" ? 1 : -1)
                                return .handled
                            }
                            .onKeyPress(.return, phases: .down) { press in
                                guard press.modifiers.isEmpty,
                                    decisionCommandActions?.canTakeRecommendation == true
                                else { return .ignored }
                                decisionCommandActions?.takeRecommendation()
                                return .handled
                            }
                        #endif
                    }
                    #if os(iOS)
                        LastUpdatedLabel(lastUpdatedAt: lastUpdatedAt)
                            .padding(.horizontal)
                            .padding(.vertical, 6)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    #endif
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
        #if os(iOS)
            .refreshable { await onRefresh() }
        #endif
    }

    @ViewBuilder
    private var scopeBar: some View {
        let urgentCount = store.urgentCount(in: store.scope)
        if Self.stacksScopeBar(at: dynamicTypeSize) {
            VStack(alignment: .leading, spacing: 8) {
                scopePicker
                urgentChip(count: urgentCount)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            HStack(spacing: 8) {
                scopePicker
                urgentChip(count: urgentCount)
            }
        }
    }

    private var scopePicker: some View {
        Picker("Scope", selection: scopeSelection) {
            ForEach(InboxStore.Scope.allCases) { scope in
                Text("\(scope.label) \(store.count(in: scope))").tag(scope)
            }
        }
        .pickerStyle(.segmented)
    }

    @ViewBuilder
    private func urgentChip(count: Int) -> some View {
        if count > 0 {
            StateChip(label: "\(count) urgent", color: .waxText)
        }
    }

    private var projectSelection: Binding<String?> {
        Binding(
            get: { store.projectID },
            set: { projectID in
                guard store.projectID != projectID else { return }
                onFilterChange()
                store.selectProjectFilter(projectID)
            }
        )
    }

    private var scopeSelection: Binding<InboxStore.Scope> {
        Binding(
            get: { store.scope },
            set: { scope in
                guard store.scope != scope else { return }
                onFilterChange()
                store.scope = scope
            }
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

    static func stacksScopeBar(at dynamicTypeSize: DynamicTypeSize) -> Bool {
        dynamicTypeSize >= .xxxLarge
    }

    private func repairSelection() {
        guard store.loadState == .loaded else { return }
        #if os(iOS)
            if let path = navigationPath?.wrappedValue {
                let repairedPath = NavigationModel.repairedPath(
                    path,
                    availableIDs: Set(store.rows.map(\.item.id)))
                if repairedPath != path {
                    navigationPath?.wrappedValue = repairedPath
                }
            }
        #else
            if let selection, !store.rows.contains(where: { $0.item.id == selection }) {
                self.selection = nil
            }
        #endif
    }

    private var orderingCaption: some View {
        Text(Self.orderingCaption(for: store.scope))
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// The project-owned caption and rows without List and Picker, whose
    /// AppKit-backed controls ImageRenderer cannot draw off-screen.
    @ViewBuilder
    func screenshotContent(now: Date) -> some View {
        VStack(spacing: 8) {
            orderingCaption
            ForEach(Array(store.rows.prefix(5)), id: \.item.id) { snapshot in
                InboxRowView(
                    item: snapshot.item,
                    isSelected: selection == snapshot.item.id,
                    now: now
                )
            }
        }
        .padding()
    }
}

/// One inbox row as a ground-2 card. Selection adds geometry as well as
/// color, so Differentiate Without Color retains a visible state change.
struct InboxRowView: View {
    let item: Components.Schemas.AttentionItem
    var isSelected = false
    var now: Date?
    var onRevealTechnicalDetails: () -> Void = {}
    var differentiateWithoutColorOverride: Bool?
    @Environment(\.accessibilityDifferentiateWithoutColor) private var differentiateWithoutColor
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Group {
            if let now {
                row(at: now)
            } else {
                TimelineView(.periodic(from: .now, by: 60)) { context in
                    row(at: context.date)
                }
            }
        }
        .contextMenu { contextMenu }
    }

    private func row(at now: Date) -> some View {
        let context = AttentionDisplay.rowContext(item)
        return HStack(spacing: 0) {
            if isSelected {
                Rectangle()
                    .fill(Color.accentText)
                    .frame(width: 4)
                    .accessibilityHidden(true)
            }

            VStack(alignment: .leading, spacing: 4) {
                if Self.stacksHeader(at: dynamicTypeSize) {
                    VStack(alignment: .leading, spacing: 5) {
                        rowTitle
                        if hasBadges {
                            rowBadges
                        }
                    }
                } else {
                    HStack(alignment: .firstTextBaseline) {
                        rowTitle
                        Spacer()
                        rowBadges
                    }
                }
                Text(AttentionDisplay.rowSummary(item))
                    .font(FreesideFont.subheadline)
                    .foregroundStyle(Color.inkDim)
                    .lineLimit(2)
                HStack(spacing: 6) {
                    contextSegment(context.project)
                    if let workUnit = context.workUnit {
                        separator
                        contextSegment(workUnit)
                    }
                    if let relativeTime = AttentionDisplay.relativeRowTime(item, now: now) {
                        separator
                        timeText(relativeTime, now: now)
                    }
                }
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(
                    isSelected
                        ? (effectiveDifferentiateWithoutColor ? Color.accentWash : .accentWashSoft)
                        : .ground2)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(isSelected ? Color.accentBorder : .rule, lineWidth: 1)
        )
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    private var rowTitle: some View {
        Text(AttentionDisplay.title(item._type))
            .font(FreesideFont.itemTitle)
            .foregroundStyle(Color.ink)
    }

    private var rowBadges: some View {
        HStack(spacing: 5) {
            if AttentionDisplay.showsPriorityBadge(item.priority) {
                PriorityBadge(priority: item.priority)
            }
            if AttentionDisplay.showsLifecycleBadge(item.status) {
                StatusBadge(status: item.status)
            }
            if AttentionDisplay.showsDegradedBadge(item) {
                StateChip(label: "Degraded", color: .waxText)
            }
        }
    }

    private var hasBadges: Bool {
        AttentionDisplay.showsPriorityBadge(item.priority)
            || AttentionDisplay.showsLifecycleBadge(item.status)
            || AttentionDisplay.showsDegradedBadge(item)
    }

    static func stacksHeader(at dynamicTypeSize: DynamicTypeSize) -> Bool {
        dynamicTypeSize >= .xxxLarge
    }

    private var effectiveDifferentiateWithoutColor: Bool {
        differentiateWithoutColorOverride ?? differentiateWithoutColor
    }

    private var separator: some View {
        Text("·")
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .accessibilityHidden(true)
    }

    @ViewBuilder
    private func contextSegment(_ segment: AttentionDisplay.RowContext.Segment) -> some View {
        let text = Text(segment.value)
            .font(segment.isIdentifier ? FreesideFont.monoCaption : FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .lineLimit(1)
            .truncationMode(.middle)
        #if os(macOS)
            text.help(segment.value)
        #else
            text
        #endif
    }

    @ViewBuilder
    private func timeText(_ relativeTime: String, now: Date) -> some View {
        let text = Text(relativeTime)
            .font(FreesideFont.caption)
            .foregroundStyle(Color.inkDim)
            .lineLimit(1)
        #if os(macOS)
            if let exact = AttentionDisplay.exactRowTimestamp(item, now: now) {
                text.help(exact)
            } else {
                text
            }
        #else
            text
        #endif
    }

    @ViewBuilder
    private var contextMenu: some View {
        let evidenceDigests = AttentionDisplay.uniqueEvidenceDigests(item)
        Button("Copy item ID") { copy(item.id) }
        if let subject = AttentionDisplay.copyableSubjectReference(item) {
            Button(subject.label) { copy(subject.value) }
        }
        if evidenceDigests.count == 1, let digest = evidenceDigests.first {
            Button("Copy evidence digest") { copy(digest) }
        } else if evidenceDigests.count > 1 {
            Menu("Copy evidence digest") {
                ForEach(evidenceDigests, id: \.self) { digest in
                    Button(digest) { copy(digest) }
                }
            }
        }
        Divider()
        Button("Reveal in Technical details") { onRevealTechnicalDetails() }
    }

    private func copy(_ value: String) {
        #if os(macOS)
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(value, forType: .string)
        #elseif os(iOS)
            UIPasteboard.general.string = value
        #endif
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
