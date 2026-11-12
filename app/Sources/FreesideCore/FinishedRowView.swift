import FreesideAPI
import SwiftUI

/// A Finished row in its compact form (owner decision, 11 Nov 2026): the
/// task's name, or a closed item's summary, on one dim serif line with the
/// time it finished trailing, and no card. Finished work is a log to look
/// back through, so it gives up the keyword and context lines a row that
/// still asks something of the operator needs. The status the full row's
/// keyword drew is spoken, not drawn. Selection and hover follow the full
/// row: the accent wash under a 4pt bar, the hover cut on the Mac.
struct FinishedRowView: View {
    enum Subject {
        /// `status` is the recorded status the full row's keyword drew;
        /// `showsIdentifier` marks a look-alike task
        /// (`TaskDisplay.ambiguousTaskIDs`): the one-line row draws its
        /// short id, and VoiceOver reads the full marker.
        case task(Components.Schemas.Task, status: String, showsIdentifier: Bool)
        case item(Components.Schemas.AttentionItem)
    }

    let subject: Subject
    var isSelected = false
    /// A fixed clock for tests and screenshots; a live row ticks its own.
    var now: Date?
    var onRevealTechnicalDetails: () -> Void = {}
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    /// The marker's clock reads the view's locale and time zone, as the
    /// task page's header does, so the two print the same string.
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone

    var body: some View {
        Group {
            if let now {
                line(at: now)
            } else {
                TimelineView(.periodic(from: .now, by: 60)) { context in
                    line(at: context.date)
                }
            }
        }
        .contextMenu { contextMenu }
    }

    private func line(at now: Date) -> some View {
        let time = time(at: now)
        return HStack(alignment: .firstTextBaseline, spacing: 8) {
            title
            Spacer(minLength: 8)
            if let time {
                timeText(time)
            }
        }
        .padding(.vertical, 6)
        // The selected row gives 2pt of its inset to the bar, as a full row
        // does.
        .padding(.leading, SidebarRowSurface.padding.width - (isSelected ? 2 : 0))
        .padding(.trailing, SidebarRowSurface.padding.width)
        .frame(maxWidth: .infinity, alignment: .leading)
        .modifier(Surface(isSelected: isSelected))
        .textSelection(.disabled)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel([titleText(at: now), status, time?.text].compactMap { $0 }.joined(separator: ", "))
    }

    /// One line at the standard sizes; an accessibility size lifts the cap,
    /// so larger text hides no part of the name (R22).
    @ViewBuilder
    private var title: some View {
        let lineLimit = dynamicTypeSize.isAccessibilitySize ? nil : 1
        switch subject {
        case .task(let task, _, let showsIdentifier):
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                TaskNameLabel(
                    name: task.display_names.task, font: FreesideFont.rowTitle, monoFont: FreesideFont.monoValue,
                    color: .inkDim, lineLimit: lineLimit)
                if showsIdentifier {
                    Text(ShortIdentifier.short(task.id))
                        .font(FreesideFont.trailingSummary)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize()
                }
            }
        case .item(let item):
            Text(AttentionDisplay.rowSummary(item))
                .font(FreesideFont.rowTitle)
                .foregroundStyle(Color.inkDim)
                .lineLimit(lineLimit)
        }
    }

    /// The title VoiceOver reads. A look-alike task's carries the whole
    /// marker the one-line row has no room to draw.
    private func titleText(at now: Date) -> String {
        switch subject {
        case .task(let task, _, let showsIdentifier):
            showsIdentifier
                ? "\(task.display_names.task.text), \(TaskDisplay.lookAlikeMarker(task, now: now, locale: locale, timeZone: timeZone))"
                : task.display_names.task.text
        case .item(let item):
            AttentionDisplay.rowSummary(item)
        }
    }

    private var status: String {
        switch subject {
        case .task(_, let status, _): status
        case .item(let item): AttentionDisplay.label(item.status)
        }
    }

    /// When the row finished: the task's last activity, or when the item
    /// was decided, the instant Finished sorts by. Always the short
    /// relative form a Needs You row draws, never a full row's date, so the
    /// title keeps the line.
    private func time(at now: Date) -> SidebarRowTime? {
        switch subject {
        case .task(let task, _, _):
            SidebarRowTime(
                text: AttentionDisplay.relativeRowTime(task.last_activity_at, now: now),
                exact: TaskDisplay.exactActivityTimestamp(task))
        case .item(let item):
            (item.decided_at ?? item.created_at).map {
                SidebarRowTime(
                    text: AttentionDisplay.relativeRowTime($0, now: now), exact: $0.formatted(.iso8601))
            }
        }
    }

    @ViewBuilder
    private func timeText(_ time: SidebarRowTime) -> some View {
        let text = Text(time.text)
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .lineLimit(1)
            .fixedSize()
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

    @ViewBuilder
    private var contextMenu: some View {
        switch subject {
        case .task(let task, _, _):
            Button("Copy task name") { Clipboard.copy(task.display_names.task.text) }
        case .item(let item):
            AttentionItemMenu(item: item, onRevealTechnicalDetails: onRevealTechnicalDetails)
        }
    }

    /// No fill and no border until selected, so finished rows read as a
    /// list under the carded rows above them.
    private struct Surface: ViewModifier {
        let isSelected: Bool

        func body(content: Content) -> some View {
            HStack(spacing: 0) {
                if isSelected {
                    Rectangle()
                        .fill(Color.accentText)
                        .frame(width: 4)
                        .accessibilityHidden(true)
                }
                hoverable(content)
            }
            .background(RoundedRectangle(cornerRadius: 8).fill(isSelected ? Color.accentWash : .clear))
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
}
