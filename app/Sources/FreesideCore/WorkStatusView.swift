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
            VStack(alignment: .leading, spacing: 16) {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Self.unheld(status.groups)) { row($0) }
                }
                let held = Self.held(status.groups)
                if !held.isEmpty {
                    VStack(alignment: .leading, spacing: 6) {
                        ListSectionHead(title: "Held", count: held.reduce(0) { $0 + $1.count })
                        ForEach(held) { row($0) }
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

    /// The label and its count as text; VoiceOver reads both.
    private func row(_ group: WorkStatus.Group) -> some View {
        let isSelected = selected == group.position
        return Button {
            onSelect(group.position)
        } label: {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(group.label)
                    .font(FreesideFont.rowTitle)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                Spacer(minLength: 8)
                Text("\(group.count)")
                    .font(FreesideFont.monoValue)
                    .foregroundStyle(Color.ink)
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
}
