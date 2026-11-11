import FreesideAPI
import SwiftUI

/// The Unattended Operation row's words: the admission gate as a chip and
/// one line naming why it is closed and since when. It reads the same
/// bootstrap snapshot and freshness as the standing stopped indicator
/// (`UnattendedStoppedPresentation`), so the two never disagree, and like
/// the indicator it reports a non-current snapshot as the last known state.
struct UnattendedGatePresentation: Equatable {
    enum Chip: Equatable {
        case open
        case stopped
        /// No bootstrap has reported the gate yet.
        case unknown
    }

    let chip: Chip
    /// Wax only for a current stop; any other state reads neutral.
    let isWax: Bool
    let line: String

    var chipLabel: String {
        switch chip {
        case .open: "Open"
        case .stopped: "Stopped"
        case .unknown: "Unknown"
        }
    }

    @MainActor
    static func make(
        operation: Components.Schemas.UnattendedOperationSnapshot?,
        freshness: InboxStore.Freshness,
        lastUpdatedAt: Date?,
        now: Date
    ) -> Self {
        guard let operation else {
            return .init(chip: .unknown, isWax: false, line: "The daemon hasn't reported it yet.")
        }
        // The indicator's own reading; the row has no action, so it needs
        // no item reason.
        guard
            let stop = UnattendedStoppedPresentation.make(
                operation: operation, freshness: freshness, lastUpdatedAt: lastUpdatedAt, now: now,
                reason: { _ in nil })
        else {
            let isCurrent = UnattendedStoppedPresentation.isCurrent(
                freshness: freshness, lastUpdatedAt: lastUpdatedAt, now: now)
            return .init(
                chip: .open, isWax: false,
                line: isCurrent
                    ? "New unattended work is admitted."
                    : "At the last successful refresh, new unattended work was admitted.")
        }
        let cause =
            switch stop.cause {
            case .operatorStop: " by operator decision"
            case .blockingFinding: " by a system health finding"
            case .unspecified: ""
            }
        let age = operation.stops.first?.since.map {
            " " + AttentionDisplay.relativeRowTime($0, now: now) + " ago"
        }
        var line =
            (stop.isCurrent ? "Stopped" : "At the last successful refresh, stopped") + cause + (age ?? "") + "."
        if stop.additionalStops == 1 {
            line += " 1 other stop is in force."
        } else if stop.additionalStops > 1 {
            line += " \(stop.additionalStops) other stops are in force."
        }
        return .init(chip: .stopped, isWax: stop.isCurrent, line: line)
    }
}

/// The System Health pane of Settings: findings about this daemon, which
/// the Unified List leaves out (the owner's 8 Nov 2026 decision). Top to
/// bottom: the admission gate, the open findings, and the closed ones
/// behind a fold. A row opens the finding's card, where its actions stay;
/// the pane itself offers none.
struct SystemHealthPane: View {
    let findings: SystemHealthFindings
    let operation: Components.Schemas.UnattendedOperationSnapshot?
    let freshness: InboxStore.Freshness
    let lastUpdatedAt: Date?
    var client: SettingsClient = .current
    let openFinding: (String) -> Void
    @State private var preferences: ListSectionPreferences
    @Environment(\.pinnedNow) private var pinnedNow

    init(
        findings: SystemHealthFindings, operation: Components.Schemas.UnattendedOperationSnapshot?,
        freshness: InboxStore.Freshness, lastUpdatedAt: Date?, client: SettingsClient = .current,
        openFinding: @escaping (String) -> Void, preferences: ListSectionPreferences
    ) {
        self.findings = findings
        self.operation = operation
        self.freshness = freshness
        self.lastUpdatedAt = lastUpdatedAt
        self.client = client
        self.openFinding = openFinding
        _preferences = State(initialValue: preferences)
    }

    var body: some View {
        // A pinned clock draws once; the live pane follows the clock as the
        // standing indicator and the list rows do.
        if let pinnedNow {
            content(at: pinnedNow)
        } else {
            TimelineView(.periodic(from: .now, by: 60)) { context in
                content(at: context.date)
            }
        }
    }

    private func content(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(title: "System Health", line: "Findings about this daemon.")
            gateSection(at: now)
            SettingsSection(keyword: "Open Findings") {
                if findings.open.isEmpty {
                    Text("No open findings.")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                } else {
                    findingGroup(findings.open, now: now)
                }
            }
            if !findings.closed.isEmpty {
                history(at: now)
            }
        }
    }

    private func gateSection(at now: Date) -> some View {
        let gate = UnattendedGatePresentation.make(
            operation: operation, freshness: freshness, lastUpdatedAt: lastUpdatedAt, now: now)
        return SettingsSection(keyword: "Unattended Operation") {
            SettingsGroup(["gate"]) { _ in
                SettingsRow(label: "Admission", line: gate.line) {
                    switch (gate.chip, gate.isWax) {
                    case (.stopped, true): StateChip(label: gate.chipLabel, color: .waxText)
                    case (.unknown, _): StateChip(label: gate.chipLabel, cut: .faint)
                    case (.open, _), (.stopped, false): StateChip(label: gate.chipLabel, cut: .ink)
                    }
                }
            }
        }
    }

    private func history(at now: Date) -> some View {
        VStack(alignment: .leading, spacing: 9) {
            ListSectionHead(
                title: "History", count: findings.closed.count,
                isExpanded: Bindable(preferences).systemHealthHistoryExpanded)
            if preferences.systemHealthHistoryExpanded {
                findingGroup(findings.closed, now: now)
            }
        }
    }

    private func findingGroup(_ items: [Components.Schemas.AttentionItem], now: Date) -> some View {
        SettingsGroup(elements: items, id: \.id) { item in
            Button {
                openFinding(item.id)
            } label: {
                FindingRow(item: item, now: now)
            }
            .buttonStyle(.plain)
        }
    }
}

/// One finding on the scanning row (R31): its posture, or its outcome once
/// closed, on the keyword line, where the list puts the type, since every
/// row here is the same type; the sentence its typed fields make, with the
/// daemon's reason in full under it, since the reason is what names the
/// held run and the refused check (#3026); and what it is about, its task
/// or else its project, with the time it opened or closed.
struct FindingRow: View {
    let item: Components.Schemas.AttentionItem
    let now: Date
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        SidebarRow(keyword: .init(text: SystemHealthFindings.keyword(item)), time: time, grouped: true) {
            EmptyView()
        } title: {
            VStack(alignment: .leading, spacing: 3) {
                Text(AttentionDisplay.rowSummary(item))
                    .font(FreesideFont.rowTitle)
                    .foregroundStyle(Color.ink)
                    .lineLimit(dynamicTypeSize.isAccessibilitySize ? nil : 2)
                if !item.reason.isEmpty {
                    Text(item.reason)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        } context: {
            SidebarRowContext(text: Self.context(item))
        } closing: {
            EmptyView()
        }
        .contentShape(Rectangle())
    }

    /// The task a finding is about by the name the Unified List gives it,
    /// or its project's.
    static func context(_ item: Components.Schemas.AttentionItem) -> String {
        let context = AttentionDisplay.rowContext(item)
        return (context.workUnit ?? context.project).value
    }

    private var time: SidebarRowTime? {
        if item.status == .open {
            return AttentionDisplay.relativeRowTime(item, now: now).map {
                SidebarRowTime(text: $0, exact: AttentionDisplay.exactRowTimestamp(item, now: now))
            }
        }
        return SystemHealthFindings.closedAt(item).map {
            SidebarRowTime(
                text: AttentionDisplay.relativeRowTime($0, now: now), exact: $0.formatted(.iso8601))
        }
    }
}

/// A finding's card in the System Health pane on the Mac, in place of the
/// pane's content, under a back control to it: the push the iPhone makes on
/// its stack. Not a sheet, because a card's actions can take time and a
/// sheet reads as modal. The card takes the pane's width inside the detail
/// card's margins, its prose at the column's measure (#2515).
struct SystemHealthCardSeat: View {
    /// The gap between the back control and the card.
    static let cardTopMargin: CGFloat = 10

    let itemID: String
    let store: InboxStore
    let onBack: () -> Void
    /// A superseded finding's `Replaced by` link: its successor's card.
    var onSelectItem: (String) -> Void = { _ in }
    var rendersInteractiveControls = true
    @Environment(\.pinnedNow) private var pinnedNow
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button(action: onBack) {
                Text("‹ System Health")
                    .font(FreesideFont.noticeAction)
                    .foregroundStyle(Color.accentText)
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Back to System Health")
            .padding(.top, SettingsPaneInsets.grammar(.mac).top)
            .padding(.leading, DecisionCardComposition.Scale.paneMargin(compact: false).leading)
            card
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    @ViewBuilder private var card: some View {
        if rendersInteractiveControls {
            // The card scrolls itself; the pane's own scroll view stays out.
            DecisionDetailView(
                store: store, itemID: itemID, findingReasonPreferences: .shared,
                topMargin: Self.cardTopMargin, onSelectItem: onSelectItem
            )
            .id(itemID)
        } else if let item = store.snapshotsByID[itemID]?.item {
            DecisionDetailView(
                store: store, itemID: itemID, loadsAttachments: false, showsValidationProgress: false,
                now: pinnedNow ?? .now
            )
            .screenshotCard(item, at: dynamicTypeSize, topMargin: Self.cardTopMargin)
        }
    }
}
