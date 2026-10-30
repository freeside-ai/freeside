import FreesideAPI
import SwiftUI

/// One task as a sidebar row (R31), on the row the inbox draws
/// (`SidebarRow`): the status as the keyword line, in its cut's color, with
/// `AGENT` after it for an agent-proposed name; the name in the serif; one
/// mono context line of project, issue, current phase, and round with the
/// last activity trailing; and at most one closing line. The four-phase
/// sentence and the hold are the task timeline's to show, as armed
/// schedules are. VoiceOver reads the row as drawn: keyword, name, context,
/// time, then the progress strings the row doesn't draw.
struct TaskRowView: View {
    let task: Components.Schemas.Task
    let position: TaskDisplay.Position?
    var isSelected = false
    /// A fixed clock for tests and screenshots. A live row leaves it nil and
    /// ticks its own, so the relative last-active text ages without a data
    /// change, as `InboxRowView` does.
    var now: Date?
    /// Set when another visible task shares this one's name, so the context
    /// line carries a short task id to tell the two rows apart.
    var showsIdentifier = false
    /// The Working or Finished section's reading of this row, when a section
    /// draws it.
    var sectionRow: TaskDisplay.SectionRow?
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
        .contextMenu {
            Button("Copy task name") { Clipboard.copy(task.display_names.task.text) }
        }
    }

    private func row(at now: Date) -> some View {
        let lines = TaskDisplay.rowLines(task, position: position)
        let name = task.display_names.task
        let closing = Closing(
            recorded: TaskDisplay.stopConfirmation(task),
            guidance: lines.visibleGuidance(attention: position?.attention == true))
        // The status is the keyword's to speak; one element after the row's
        // lines speaks every other progress string, the closing line where
        // the row draws one.
        let spoken = lines.all.dropFirst().joined(separator: ", ")
        return SidebarRow(
            keyword: .init(
                text: sectionRow?.keyword ?? lines.status,
                color: (sectionRow?.cut ?? TaskDisplay.statusCut(task, position: position)).text,
                marksAgent: name.source == .agent),
            time: .init(
                text: TaskDisplay.rowTime(task, now: now), exact: TaskDisplay.exactActivityTimestamp(task)),
            isSelected: isSelected
        ) {
            EmptyView()
        } title: {
            // Two lines at the standard sizes; an accessibility size lifts
            // the cap, so larger text hides no part of the name (R22).
            TaskNameLabel(
                name: name, font: FreesideFont.rowTitle, monoFont: FreesideFont.monoValue,
                lineLimit: dynamicTypeSize.isAccessibilitySize ? nil : 2, drawsAgentMark: false)
        } context: {
            contextText
        } closing: {
            if closing.isDrawn {
                closingLine(closing)
                    .accessibilityElement(children: .ignore)
                    .accessibilityLabel(spoken)
            } else if !spoken.isEmpty {
                // Nothing closes the row, but VoiceOver still reads the
                // progress strings last, and an element needs a frame to be
                // reached. The negative padding takes the line and its gap
                // back, so the row is no taller than an inbox row.
                Color.clear.frame(height: 1)
                    .padding(.top, -(SidebarRowSurface.lineGap + 1))
                    .accessibilityElement(children: .ignore)
                    .accessibilityLabel(spoken)
            }
        }
    }

    /// The context line. It wraps where the sidebar is too narrow for one
    /// line, since the phase and round at its end are what the row kept
    /// when the phase sentence left it; each separator stays with the
    /// segment before it.
    private var contextText: some View {
        let identifier = showsIdentifier ? " · \(ShortIdentifier.short(task.id))" : ""
        let context = (sectionRow?.context ?? TaskDisplay.rowContext(task, position: position)) + identifier
        return SidebarRowContext(text: context)
    }

    /// What closes the row: a recorded stop confirmation, then the guidance
    /// that says what the row does not, Inbox guidance as a link or a
    /// capacity wait as a sentence. Most rows draw neither.
    private struct Closing {
        let recorded: String?
        let guidance: TaskDisplay.RowLines.VisibleGuidance?

        var isDrawn: Bool { recorded != nil || guidance != nil }
    }

    private func closingLine(_ closing: Closing) -> some View {
        VStack(alignment: .leading, spacing: SidebarRowSurface.lineGap) {
            if let recorded = closing.recorded { closingText(recorded) }
            switch closing.guidance {
            case .link(let title):
                // Wraps like a sentence; left to the row it ends in an
                // ellipsis at larger sizes.
                FreesideLink(title: title, face: FreesideFont.noticeAction)
                    .multilineTextAlignment(.leading)
                    .fixedSize(horizontal: false, vertical: true)
            case .sentence(let sentence): closingText(sentence)
            case nil: EmptyView()
            }
        }
    }

    private func closingText(_ line: String) -> some View {
        Text(line)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }
}

/// A task row's mono context line. It wraps where the sidebar is too narrow
/// for one line, keeping each separator with the segment before it.
struct SidebarRowContext: View {
    let text: String

    var body: some View {
        Text(text.replacingOccurrences(of: " · ", with: "\u{00A0}· "))
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .fixedSize(horizontal: false, vertical: true)
    }
}
