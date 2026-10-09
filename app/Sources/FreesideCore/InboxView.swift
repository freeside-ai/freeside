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
    /// The count shown beside Inbox: the number the Open segment carried
    /// before the scope control lost its counts, so it follows the project
    /// filter as the list does. Absent until the inbox has loaded, when a
    /// zero would read as an empty inbox.
    static func openCount(in store: InboxStore) -> Int? {
        store.loadState == .loaded ? store.count(in: .open) : nil
    }

    /// The iPhone navigation title, which carries the open count because
    /// the tab bar has no room for it; macOS shows it in the section
    /// switcher instead.
    static func phoneNavigationTitle(openCount: Int?) -> String {
        openCount.map { "Inbox · \($0)" } ?? "Inbox"
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

    /// What an empty scope says (R13). The Open scope answers the question
    /// the inbox exists for, naming the project when one is filtered; the
    /// detail pane beside it says the same two lines. The other scopes hold
    /// a record, so they say only that nothing is here yet.
    static func emptyScope(
        _ scope: InboxStore.Scope, projectID: String?
    ) -> (title: String, description: String) {
        switch scope {
        case .open:
            (
                "No open items",
                projectID == nil ? "Nothing needs you." : "Nothing in this project needs you."
            )
        case .resolved:
            ("No resolved items", "Attention items in this scope will appear here.")
        case .all:
            // "All" names no kind of item, so the title drops the scope word.
            ("No items", "Attention items in this scope will appear here.")
        }
    }

    var body: some View {
        Group {
            switch store.loadState {
            case .idle, .loading:
                ProgressView()
            case .failed(let message):
                UnavailableStateView(title: "Couldn't load the inbox", description: message)
            case .loaded:
                VStack(spacing: 0) {
                    scopeBar
                        .padding(.horizontal)
                        .padding(.bottom, 8)

                    projectMenu
                        .padding(.horizontal)
                        .padding(.bottom, 8)

                    if store.rows.isEmpty {
                        let empty = Self.emptyScope(store.scope, projectID: store.projectID)
                        #if os(macOS)
                            Spacer(minLength: 0)
                            SidebarEmptyState(title: empty.title, description: empty.description)
                            Spacer(minLength: 0)
                        #else
                            UnavailableStateView(title: empty.title, description: empty.description)
                        #endif
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
        #if os(iOS)
            .navigationTitle(Self.phoneNavigationTitle(openCount: Self.openCount(in: store)))
        #else
            .navigationTitle("Inbox")
        #endif
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
        FreesideSegmentedControl(
            accessibilityLabel: "Scope",
            segments: InboxStore.Scope.allCases.map {
                .init(value: $0, label: $0.label)
            },
            selection: scopeSelection)
    }

    /// The project filter: a Freeside trigger over the system popup list.
    private var projectMenu: some View {
        Menu {
            Picker("Project", selection: projectSelection) {
                Text("All projects").tag(String?.none)
                ForEach(store.projects, id: \.self) { project in
                    Text(project).tag(String?.some(project))
                }
            }
            .pickerStyle(.inline)
        } label: {
            FreesideMenuTriggerLabel(title: store.projectID ?? "All projects")
        }
        .menuStyle(.button)
        .buttonStyle(.plain)
        .menuIndicator(.hidden)
        .accessibilityLabel("Project")
        .accessibilityValue(store.projectID ?? "All projects")
    }

    @ViewBuilder
    private func urgentChip(count: Int) -> some View {
        if count > 0 {
            StateChip(label: "\(count) Urgent", color: .waxText)
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

    /// The sidebar chrome as the operator sees it on macOS: the section
    /// switcher with both counts (the caller supplies the task list's, which
    /// this view does not hold), the scope control and urgent chip, the
    /// project trigger (its label standing in for the Menu, which
    /// ImageRenderer cannot open), and the first rows on the sidebar ground,
    /// or the empty state when the scope holds none.
    func screenshotSidebar(now: Date, activeTaskCount: Int?) -> some View {
        VStack(spacing: 0) {
            FreesideSegmentedControl(
                accessibilityLabel: "Section",
                segments: FreesideRootView.sectionSegments(
                    openCount: Self.openCount(in: store), activeTaskCount: activeTaskCount),
                selection: .constant(.inbox)
            )
            .padding()
            scopeBar
                .padding(.horizontal)
                .padding(.bottom, 8)
            FreesideMenuTriggerLabel(title: store.projectID ?? "All projects")
                .padding(.horizontal)
                .padding(.bottom, 8)
            if store.rows.isEmpty {
                let empty = Self.emptyScope(store.scope, projectID: store.projectID)
                SidebarEmptyState(title: empty.title, description: empty.description)
                    .padding(.bottom, 32)
            } else {
                VStack(spacing: 8) {
                    ForEach(Array(store.rows.prefix(2)), id: \.item.id) { snapshot in
                        InboxRowView(
                            item: snapshot.item,
                            isSelected: selection == snapshot.item.id,
                            now: now)
                    }
                }
                .padding(.horizontal)
                .padding(.bottom)
            }
        }
        .background(Color.sidebarGround)
    }

    /// The rows without List, whose AppKit-backed control ImageRenderer
    /// cannot draw off-screen.
    @ViewBuilder
    func screenshotContent(now: Date) -> some View {
        VStack(spacing: 8) {
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

/// The surface a sidebar row sits on (R19), shared by the inbox and task
/// rows so the two lists select and hover alike. An unselected row is a
/// bordered item on ground-2; the selected row drops the border for the
/// accent wash under a 4pt accent bar, so selection adds geometry as well as
/// color and reads the same with Differentiate Without Color. On macOS an
/// unselected row takes the hover cut under the pointer.
struct SidebarRowSurface: ViewModifier {
    let isSelected: Bool

    /// The row's padding as the frames draw it: 10 by 12 on the Mac, 11 by
    /// 13 on iPhone.
    #if os(macOS)
        static let padding = CGSize(width: 12, height: 10)
    #else
        static let padding = CGSize(width: 13, height: 11)
    #endif
    /// The gap between a row's lines, as the frames draw it.
    static let lineGap: CGFloat = 5

    /// From xxxLarge the chips leave the keyword's line for one of their own.
    static func stacksHeader(at dynamicTypeSize: DynamicTypeSize) -> Bool {
        dynamicTypeSize >= .xxxLarge
    }

    func body(content: Content) -> some View {
        HStack(spacing: 0) {
            if isSelected {
                Rectangle()
                    .fill(Color.accentText)
                    .frame(width: 4)
                    .accessibilityHidden(true)
            }
            hoverable(
                content
                    .padding(.vertical, Self.padding.height)
                    // The selected row gives 2pt of its inset to the bar.
                    .padding(.leading, Self.padding.width - (isSelected ? 2 : 0))
                    .padding(.trailing, Self.padding.width)
                    .frame(maxWidth: .infinity, alignment: .leading))
        }
        .background(RoundedRectangle(cornerRadius: 8).fill(isSelected ? Color.accentWash : .ground2))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .strokeBorder(isSelected ? Color.clear : .itemBorder, lineWidth: 1)
        )
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder
    private func hoverable(_ row: some View) -> some View {
        #if os(macOS)
            if isSelected {
                row
            } else {
                row.freesideHover(cornerRadius: 8)
            }
        #else
            row
        #endif
    }
}

/// One sidebar row in the grammar both lists draw (R31), top to bottom: a
/// keyword line with the exceptional chips trailing, a serif line, one mono
/// context line with the time trailing, and at most one closing line. The
/// inbox and the task list differ only in what they put in each slot; the
/// layout, its stacking at the large text sizes, and the surface are this
/// view's, so the two lists cannot drift apart.
struct SidebarRow<Badges: View, Title: View, Context: View, Closing: View>: View {
    let keyword: SidebarRowKeyword
    var time: SidebarRowTime? = nil
    var isSelected = false
    /// False draws no chip line where the header stacks.
    var hasBadges = false
    @ViewBuilder var badges: Badges
    @ViewBuilder var title: Title
    @ViewBuilder var context: Context
    @ViewBuilder var closing: Closing
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        VStack(alignment: .leading, spacing: SidebarRowSurface.lineGap) {
            if SidebarRowSurface.stacksHeader(at: dynamicTypeSize) {
                keywordLine
                if hasBadges {
                    badges
                }
            } else {
                HStack(alignment: .center, spacing: 10) {
                    keywordLine
                    Spacer(minLength: 0)
                    badges
                }
            }
            title
            if dynamicTypeSize.isAccessibilitySize {
                // The time drops under the context line, which then has the
                // row's width to itself.
                context
                timeText
            } else {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    context
                    if time != nil {
                        Spacer(minLength: 8)
                        timeText
                            .fixedSize()
                    }
                }
            }
            closing
        }
        .modifier(SidebarRowSurface(isSelected: isSelected))
    }

    /// The keyword sized to its own height so a long one wraps at the large
    /// text sizes; left flexible, the stacked header truncated it. The
    /// Agent mark sits beside it where both fit on one line and under it
    /// otherwise, so a long status wraps at its words, never inside one.
    private var keywordLine: some View {
        let word = KeywordLabel(text: keyword.text, color: keyword.color)
            .fixedSize(horizontal: false, vertical: true)
        return Group {
            if keyword.marksAgent {
                ViewThatFits(in: .horizontal) {
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        word
                        KeywordLabel(text: "Agent")
                    }
                    VStack(alignment: .leading, spacing: 2) {
                        word
                        KeywordLabel(text: "Agent")
                    }
                }
            } else {
                word
            }
        }
        .accessibilityHidden(!keyword.isSpoken)
    }

    @ViewBuilder
    private var timeText: some View {
        if let time {
            let text = Text(time.text)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .lineLimit(1)
                .accessibilityHidden(!time.isSpoken)
            #if os(macOS)
                if let exact = time.exact {
                    text.help(exact)
                } else {
                    text
                }
            #else
                text
            #endif
        }
    }
}

/// A sidebar row's keyword line: the leading word, with `AGENT` after it
/// when the row's serif line is agent prose (plan §9).
struct SidebarRowKeyword {
    let text: String
    var color: Color = .inkDim
    var marksAgent = false
    /// False where the row speaks this line elsewhere, in its own order.
    var isSpoken = true
}

/// The time trailing a sidebar row's context line; `exact` is the macOS
/// hover help.
struct SidebarRowTime {
    let text: String
    var exact: String? = nil
    /// False where the context line already speaks the time.
    var isSpoken = true
}

/// One inbox row (R31) on the shared row: the type as a keyword with its
/// chips trailing, the summary, and one context line.
struct InboxRowView: View {
    let item: Components.Schemas.AttentionItem
    var isSelected = false
    var now: Date?
    var onRevealTechnicalDetails: () -> Void = {}
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
        let time = AttentionDisplay.relativeRowTime(item, now: now).map {
            SidebarRowTime(text: $0, exact: AttentionDisplay.exactRowTimestamp(item, now: now))
        }
        // The keyword is the word the decision card's eyebrow uses for this
        // item (R27), so the row and the card it opens name the type alike.
        return SidebarRow(
            keyword: .init(text: DecisionCardComposition.eyebrow(for: item).keyword),
            time: time, isSelected: isSelected, hasBadges: hasBadges
        ) {
            rowBadges
        } title: {
            // The summary says what needs attention, so it takes the serif
            // and the main ink; the type above it is the quiet line (visual
            // audit D01).
            // Two lines at the standard sizes. An accessibility size fits
            // fewer words on a line, so the cap lifts there rather than hide
            // what the default size shows (R22).
            Text(AttentionDisplay.rowSummary(item))
                .font(FreesideFont.rowTitle)
                .foregroundStyle(Color.ink)
                .lineLimit(dynamicTypeSize.isAccessibilitySize ? nil : 2)
        } context: {
            contextLine(context)
        } closing: {
            EmptyView()
        }
    }

    private func contextLine(_ context: AttentionDisplay.RowContext) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            contextSegment(context.project)
            if let workUnit = context.workUnit {
                separator
                contextSegment(workUnit)
                if workUnit.isAgentClaim {
                    CompactMark(text: "Agent")
                }
            }
        }
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
        SidebarRowSurface.stacksHeader(at: dynamicTypeSize)
    }

    private var separator: some View {
        Text("·")
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .accessibilityHidden(true)
    }

    @ViewBuilder
    private func contextSegment(_ segment: AttentionDisplay.RowContext.Segment) -> some View {
        let text = Text(segment.value)
            .font(FreesideFont.trailingSummary)
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
        Button("Reveal in Technical Details") { onRevealTechnicalDetails() }
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
