import FreesideAPI
import SwiftUI

/// The work status view (plan §8): how many tasks sit at each position
/// right now, under the project filter (`WorkStatus`). Each count is a
/// button that narrows the unified list to the tasks it counted; the Mac
/// draws the view in the detail column beside the filtered sidebar, and
/// the iPhone pushes it and pops back to the list. It reads and links; it
/// never acts on a task.
struct WorkStatusView: View {
    let status: WorkStatus
    let projectID: String?
    /// The position filter on the list, drawn as the selected count.
    let selected: WorkStatus.Position?
    let onSelect: (WorkStatus.Position) -> Void
    var topMargin: CGFloat?

    var body: some View {
        if status.groups.isEmpty {
            UnavailableStateView(
                glyph: .tasks, title: "No work to count",
                description: projectID == nil
                    ? "No task is active or waiting on you." : "No task in this project is active or waiting on you.",
                seat: .detailColumn)
        } else {
            ScrollView { content }
                .background(Color.ground)
        }
    }

    /// The header and the counts, without the scroll view, which
    /// ImageRenderer can't draw off-screen.
    func screenshotContent() -> some View {
        content.background(Color.ground)
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Work Status",
                ask: "Where your work is sitting",
                consequence:
                    "\(UnifiedListView.filterState(projectID: projectID)). Choose a count to show its tasks in the list.",
                askLineLimit: nil)
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
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
        }
        .frame(maxWidth: DecisionCardComposition.Scale.columnCap)
        .padding(.horizontal, Self.sideMargin)
        .padding(.top, topMargin ?? 0)
        .frame(maxWidth: .infinity, alignment: DecisionCardComposition.Scale.columnAlignment)
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
