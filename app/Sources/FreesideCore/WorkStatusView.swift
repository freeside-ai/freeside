import FreesideAPI
import SwiftUI

/// The work status view (plan §8), in two tabs. Now counts how many tasks
/// sit at each position right now, under the project filter
/// (`WorkStatus`); each count is a button that narrows the unified list to
/// the tasks it counted. Over Time (#3242) draws how a period's hours
/// divided by class (`WorkHoursView`). The Mac draws the view in the detail
/// column beside the filtered sidebar, and the iPhone pushes it and pops
/// back to the list. It reads and links; it never acts on a task.
struct WorkStatusView: View {
    enum Tab: Hashable {
        case now
        case overTime
    }

    let status: WorkStatus
    let projectID: String?
    /// The position filter on the list, drawn as the selected count.
    let selected: WorkStatus.Position?
    let onSelect: (WorkStatus.Position) -> Void
    /// The Over Time tab's period and its fetch. Nil only in a screenshot,
    /// which draws the tab from `screenshotContent(overTime:)`.
    let hours: WorkHoursModel?
    /// Opens a task page from the Over Time tab.
    let onOpenTask: (String) -> Void
    var topMargin: CGFloat?
    /// The view opens on Now; the choice isn't kept across launches.
    @State private var tab = Tab.now
    @Environment(\.timeZone) private var timeZone

    init(
        status: WorkStatus, projectID: String?, selected: WorkStatus.Position?,
        onSelect: @escaping (WorkStatus.Position) -> Void, hours: WorkHoursModel? = nil,
        onOpenTask: @escaping (String) -> Void = { _ in }, topMargin: CGFloat? = nil
    ) {
        self.status = status
        self.projectID = projectID
        self.selected = selected
        self.onSelect = onSelect
        self.hours = hours
        self.onOpenTask = onOpenTask
        self.topMargin = topMargin
    }

    var body: some View {
        ScrollView { content(overTime: nil) }
            .background(Color.ground)
    }

    /// The header, the tabs, and a tab's content, without the scroll view,
    /// which ImageRenderer can't draw off-screen. `overTime` draws the Over
    /// Time tab selected, from a fixed state; nil draws Now.
    func screenshotContent(overTime: WorkHoursView? = nil) -> some View {
        content(overTime: overTime).background(Color.ground)
    }

    private func content(overTime screenshot: WorkHoursView?) -> some View {
        let shown = screenshot == nil ? tab : .overTime
        return VStack(alignment: .leading, spacing: 12) {
            SettingsPaneHead(
                keyword: "Work Status",
                title: "Where your work is sitting",
                line: "\(UnifiedListView.filterState(projectID: projectID)). "
                    + (shown == .now
                        ? "Choose a count to show its tasks in the list." : "Choose a day to see its tasks."))
            VStack(alignment: .leading, spacing: 16) {
                FreesideSegmentedControl(
                    accessibilityLabel: "Work status",
                    segments: [.init(value: Tab.now, label: "Now"), .init(value: .overTime, label: "Over Time")],
                    selection: screenshot == nil ? $tab : .constant(.overTime))
                switch shown {
                case .now: counts
                case .overTime:
                    if let screenshot { screenshot } else { liveHours }
                }
            }
        }
        .padding(16)
        .frame(maxWidth: DecisionCardComposition.Scale.columnCap)
        .padding(.horizontal, Self.sideMargin)
        .padding(.top, topMargin ?? 0)
        .frame(maxWidth: .infinity, alignment: DecisionCardComposition.Scale.columnAlignment)
    }

    /// The Now tab: the counts, or the statement that nothing is counted,
    /// which keeps the tabs above it reachable.
    @ViewBuilder private var counts: some View {
        if status.groups.isEmpty {
            EmptyStateBlock(
                glyph: .tasks, title: "No work to count",
                description: projectID == nil
                    ? "No task is active or waiting on you." : "No task in this project is active or waiting on you."
            )
            .padding(.vertical, 40)
            .frame(maxWidth: .infinity)
        } else {
            let largest = status.largestCount
            VStack(alignment: .leading, spacing: 16) {
                let unheld = Self.unheld(status.groups)
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(unheld) { group in
                        row(group, largest: largest)
                            // The one row the operator owes stands apart:
                            // 18pt to the next row, the usual 6 plus 12.
                            // Alone, it keeps the Held head's usual 16.
                            .padding(.bottom, group.position == .waitingOnYou && unheld.count > 1 ? 12 : 0)
                    }
                }
                let held = Self.held(status.groups)
                if !held.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        ListSectionHead(title: "Held", count: held.reduce(0) { $0 + $1.count })
                        ForEach(held) { row($0, largest: largest) }
                    }
                }
            }
        }
    }

    /// The Over Time tab over the model. It fetches when it shows and when
    /// the period, the reader's zone, or the project filter changes; the
    /// sync heartbeat doesn't refetch it.
    @ViewBuilder private var liveHours: some View {
        if let hours {
            WorkHoursView(
                display: hours.display, period: hours.period(in: timeZone), canGoLater: hours.canGoLater,
                onEarlier: { hours.earlier() }, onLater: { hours.later() },
                onRetry: { Task { await hours.load(timeZone: timeZone, projectID: projectID) } },
                onOpenTask: onOpenTask
            )
            .task(id: HoursRequest(period: hours.period(in: timeZone), timeZone: timeZone, projectID: projectID)) {
                await hours.load(timeZone: timeZone, projectID: projectID)
            }
        }
    }

    /// Keyed on the period itself, not the steps back, so a redraw after
    /// midnight fetches the period that now ends today.
    private struct HoursRequest: Hashable {
        let period: WorkHoursPeriod
        let timeZone: TimeZone
        let projectID: String?
    }

    /// The detail column's x on the Mac, as the run's return row sits.
    private static let sideMargin: CGFloat = {
        #if os(macOS)
            8
        #else
            0
        #endif
    }()

    /// Waiting on you, the stages, and not started.
    static func unheld(_ groups: [WorkStatus.Group]) -> [WorkStatus.Group] {
        groups.filter { if case .held = $0.position { false } else { true } }
    }

    static func held(_ groups: [WorkStatus.Group]) -> [WorkStatus.Group] {
        groups.filter { if case .held = $0.position { true } else { false } }
    }

    private func row(_ group: WorkStatus.Group, largest: Int) -> some View {
        WorkStatusRow(group: group, largest: largest, isSelected: selected == group.position) {
            onSelect(group.position)
        }
    }
}

/// One position's row: the label, its bar, and its count as text.
/// VoiceOver reads the label and the count; the bar only repeats the
/// count. Every row's track starts and ends at the same x, so the bars
/// compare on the tab's one scale (`NameTrackCountLayout`). From xxxLarge
/// the bar takes a line of its own under the name and count, as Over
/// Time's day rows draw it, so a wrapped name never squeezes the track.
///
/// Its own view, not a method on `WorkStatusView`, so it reads the text
/// size from the environment when `screenshotContent` draws the tab
/// outside that view's body.
private struct WorkStatusRow: View {
    let group: WorkStatus.Group
    /// The tab's scale: `WorkStatus.largestCount`.
    let largest: Int
    let isSelected: Bool
    let onSelect: () -> Void
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    var body: some View {
        Button(action: onSelect) {
            Group {
                if SidebarRowSurface.stacksHeader(at: dynamicTypeSize) {
                    VStack(alignment: .leading, spacing: 6) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            label
                            Spacer(minLength: 8)
                            count
                        }
                        bar.padding(.leading, -shift)
                    }
                } else {
                    NameTrackCountLayout(shift: shift) {
                        label
                        bar
                        // A hidden 000 holds the column at three digits'
                        // width at every text size; a longer count runs
                        // into the gap before it rather than truncating.
                        Text(verbatim: "000")
                            .font(FreesideFont.monoValue)
                            .hidden()
                            .overlay(alignment: .trailing) { count.fixedSize() }
                    }
                }
            }
            .modifier(SidebarRowSurface(isSelected: isSelected))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .freesideFocusRing(cornerRadius: 4)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(group.label), \(group.count)")
        .accessibilityHint("Shows these tasks in the list")
        .accessibilityAddTraits(isSelected ? [.isButton, .isSelected] : .isButton)
    }

    /// The selected row's content starts further right; its track reaches
    /// back by as much, so it lines up with the others.
    private var shift: CGFloat {
        isSelected ? SidebarRowSurface.selectedInsetGiven : 0
    }

    private var bar: some View {
        MeterTrack(fraction: WorkStatus.barFraction(group.count, largest: largest), fill: Self.barColor(group.position))
            .accessibilityHidden(true)
    }

    private var label: some View {
        Text(group.label)
            .font(FreesideFont.rowTitle)
            .foregroundStyle(Color.ink)
            .fixedSize(horizontal: false, vertical: true)
    }

    private var count: some View {
        Text("\(group.count)")
            .font(FreesideFont.monoValue)
            .foregroundStyle(Color.ink)
    }

    /// The accent for the row the operator owes, water for a stage, as the
    /// agent's working time is, and the neutral held time takes for work
    /// that waits on neither.
    private static func barColor(_ position: WorkStatus.Position) -> Color {
        switch position {
        case .waitingOnYou: .accentText
        case .stage: .waterText
        case .notStarted, .held: RunTimeStripView.color(.held)
        }
    }
}

/// A Work Status row's name, track, and count, side by side. The name
/// takes half the row, capped at the frame's 190pt column, and wraps
/// inside it; the count takes its ideal width at the trailing edge, on
/// the name's first baseline; the track fills between them, centred on
/// the row. Every row in the tab is offered the same width, so every
/// track starts and ends at the same x. A width, not the platform, sets
/// the column, so a narrow Mac column and the iPhone get the same room
/// for the track. `shift` is how much further right this row's content
/// starts (the selected row's); the name column narrows by as much, so
/// the track still starts where the others do.
private struct NameTrackCountLayout: Layout {
    let shift: CGFloat

    private static let nameShare: CGFloat = 0.5
    private static let nameCap: CGFloat = 190
    private static let spacing: CGFloat = 12

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? (Self.nameCap / Self.nameShare - shift)
        return CGSize(width: width, height: columns(width: width, subviews: subviews).height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let columns = columns(width: bounds.width, subviews: subviews)
        let nameProposal = ProposedViewSize(width: columns.name, height: nil)
        let name = subviews[0].dimensions(in: nameProposal)
        let count = subviews[2].dimensions(in: .unspecified)
        let nameTop = bounds.midY - name.height / 2
        subviews[0].place(at: CGPoint(x: bounds.minX, y: nameTop), anchor: .topLeading, proposal: nameProposal)
        subviews[1].place(
            at: CGPoint(x: bounds.minX + columns.name + Self.spacing, y: bounds.midY), anchor: .leading,
            proposal: ProposedViewSize(width: columns.track, height: nil))
        subviews[2].place(
            at: CGPoint(x: bounds.maxX, y: nameTop + name[.firstTextBaseline] - count[.firstTextBaseline]),
            anchor: .topTrailing, proposal: .unspecified)
    }

    private func columns(width: CGFloat, subviews: Subviews) -> (name: CGFloat, track: CGFloat, height: CGFloat) {
        let name = max(0, min(Self.nameCap, (width + shift) * Self.nameShare) - shift)
        let count = subviews[2].sizeThatFits(.unspecified)
        let track = max(0, width - name - count.width - 2 * Self.spacing)
        let height = max(
            subviews[0].sizeThatFits(ProposedViewSize(width: name, height: nil)).height,
            subviews[1].sizeThatFits(ProposedViewSize(width: track, height: nil)).height,
            count.height)
        return (name, track, height)
    }
}
