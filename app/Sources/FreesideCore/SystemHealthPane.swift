import FreesideAPI
import SwiftUI

/// The Unattended Operation row's words: the admission gate as a chip and
/// one line naming why it is closed and since when. It reads the same
/// bootstrap snapshot and freshness as the standing stopped indicator
/// (`UnattendedStoppedPresentation`), so the two never disagree, and like
/// the indicator it reports a non-current snapshot as the last known state.
/// It is the pane's one statement of admission (#3678): a stop by blocking
/// findings counts the snapshot's blocking stops, one per open blocking
/// finding, and points at the rows below rather than restating them.
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
        let lead = stop.isCurrent ? "Stopped" : "At the last successful refresh, stopped"
        func age(_ since: Date?) -> String {
            since.map { " " + AttentionDisplay.relativeRowTime($0, now: now) + " ago" } ?? ""
        }
        var line: String
        let otherStops: Int
        switch stop.cause {
        case .blockingFinding:
            // One stop per open blocking finding, so the count names the
            // rows below that stop admission; the other stops are the rest.
            // The daemon lists blocking stops in no time order, so the age
            // is the oldest one's: how long the findings have held admission.
            let blocking = operation.stops.filter { $0.kind == .blocking_system_health }
            let findings = blocking.count == 1 ? "finding" : "findings"
            line = lead + age(blocking.compactMap(\.since).min()) + " by \(blocking.count) blocking \(findings) below."
            otherStops = operation.stops.count - blocking.count
        case .operatorStop:
            line = lead + " by operator decision" + age(operation.stops.first?.since) + "."
            otherStops = stop.additionalStops
        case .unspecified:
            line = lead + age(operation.stops.first?.since) + "."
            otherStops = stop.additionalStops
        }
        if otherStops == 1 {
            line += " 1 other stop is in force."
        } else if otherStops > 1 {
            line += " \(otherStops) other stops are in force."
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
                FindingRow(
                    item: item, now: now, isFirst: item.id == items.first?.id,
                    isLast: item.id == items.last?.id)
            }
            .buttonStyle(.plain)
        }
    }
}

/// One finding on the scanning row (R31): its posture, or its outcome once
/// closed, on the keyword line, where the list puts the type, since every
/// row here is the same type, with an open finding's effect on admission
/// beside it (#3678); the finding's name, with the daemon's reason in full
/// under it, since the reason is what names the held run and the refused
/// check (#3026); and what it is about, its task or else its project, with
/// the time it opened or closed. An accent chevron, centered on the row,
/// says the row opens the finding's card.
///
/// An open blocking finding is marked in wax (#3356): its keyword in wax
/// semibold and a 3pt wax rule down its leading edge, the color the
/// Stopped chip above it uses for a stop.
struct FindingRow: View {
    /// The rule's width, and its inset from the group's edge: the group's
    /// 1pt border, so the rule sits inside it and the content, at the
    /// grouped row's 14pt padding, keeps 10pt from it.
    static let ruleWidth: CGFloat = 3
    static let ruleInset: CGFloat = 1
    /// The chevron's inset from the group's trailing edge: the grouped
    /// row's padding, so it lines up with the content's leading inset.
    static let chevronTrailingPadding: CGFloat = 14
    /// `SettingsGroup`'s corner radius, which the rule follows.
    static let groupCornerRadius: CGFloat = 8

    let item: Components.Schemas.AttentionItem
    let now: Date
    /// The row's place in its group, so the rule rounds where it meets the
    /// group's corner.
    var isFirst = false
    var isLast = false
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    /// Whether the row takes the wax mark: open, and blocking as the pane
    /// sorts and counts it, an unknown posture included. A closed finding
    /// never does; its keyword is its outcome.
    static func isMarkedBlocking(_ item: Components.Schemas.AttentionItem) -> Bool {
        item.status == .open && SystemHealthFindings.isBlocking(item)
    }

    var body: some View {
        let marked = Self.isMarkedBlocking(item)
        HStack(alignment: .center, spacing: 0) {
            row(marked: marked)
            // Says the row opens the finding's card; the row is already a
            // button to VoiceOver.
            Text("›")
                .font(FreesideFont.factLabel)
                .foregroundStyle(Color.accentText)
                .padding(.trailing, Self.chevronTrailingPadding)
                .accessibilityHidden(true)
        }
        // A background, so the rule takes the row's full height without
        // moving its content. Only the rule clips to the group's corner, as
        // the quote block's does; the border, drawn over the rows, covers
        // the rest. Clipping the whole group would cut the rows' focus rings.
        .background {
            if marked {
                Rectangle()
                    .fill(Color.waxText)
                    .frame(width: Self.ruleWidth)
                    .padding(.leading, Self.ruleInset)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
                    .clipShape(
                        UnevenRoundedRectangle(
                            topLeadingRadius: isFirst ? Self.groupCornerRadius : 0,
                            bottomLeadingRadius: isLast ? Self.groupCornerRadius : 0)
                    )
                    .accessibilityHidden(true)
            }
        }
        .contentShape(Rectangle())
    }

    private func row(marked: Bool) -> some View {
        SidebarRow(
            keyword: .init(
                text: SystemHealthFindings.keyword(item), color: marked ? .waxText : .inkDim, isStrong: marked,
                effect: SystemHealthFindings.effect(item)),
            time: time, grouped: true
        ) {
            EmptyView()
        } title: {
            VStack(alignment: .leading, spacing: 3) {
                Text(AttentionDisplay.findingName(item))
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
    /// The tasks the card's Task link resolves against, and what opens one.
    var tasks: [Components.Schemas.TaskSnapshot] = []
    var onSelectTask: (String) -> Void = { _ in }
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
                topMargin: Self.cardTopMargin, onSelectItem: onSelectItem, tasks: tasks,
                onSelectTask: onSelectTask
            )
            .id(itemID)
        } else if let item = store.snapshotsByID[itemID]?.item {
            DecisionDetailView(
                store: store, itemID: itemID, loadsAttachments: false, showsValidationProgress: false,
                now: pinnedNow ?? .now, tasks: tasks
            )
            .screenshotCard(item, at: dynamicTypeSize, topMargin: Self.cardTopMargin)
        }
    }
}
