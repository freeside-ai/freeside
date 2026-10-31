import FreesideAPI
import SwiftUI

/// A chronology as one open rail, newest first: the task's events, a run's
/// milestones on its own page, and the same milestones in the task page's
/// run card. One grammar for all three (devlog/2026-10-11-1140-task-stage-timeline.md):
/// the newest entry leads in ink, a stage start is marked in the diff green
/// and a failure in wax, and a connector joins every marker to the next.
/// Rows touch, each carrying its own gap, so the connector runs through the
/// gap unbroken.
struct TimelineRail: View {
    /// One line on the rail: a recorded event, or the start of a stage drawn
    /// from one.
    struct Row: Equatable {
        enum Kind: Equatable {
            case event
            /// An event that records a failure. The rail marks it in wax.
            case failure
            /// The start of a stage, named when the rail knows which: a run
            /// rail marks a run's submission without naming the stage it
            /// enters, since only the task's events track where it stands.
            case stageStart(Components.Schemas.StageName?)
        }

        let kind: Kind
        let label: String
        /// Nil when the record holds no instant for the entry; the row then
        /// draws no time.
        let recordedAt: Date?
        /// The attempt the entry belongs to, drawn as a dim sub line under
        /// the label. The task's events name their attempt in the label and
        /// carry none.
        var context: String?
        /// The newest entry, and on the task's events the stage start drawn
        /// from it: the two share one instant, so both read as the head of
        /// the rail.
        var isNewest = false
        /// The attention item the entry records a decision on, which opens
        /// its card. Nil where the client holds no such item.
        var itemID: String?

        init(
            kind: Kind, label: String, recordedAt: Date?, context: String? = nil, isNewest: Bool = false,
            itemID: String? = nil
        ) {
            self.kind = kind
            self.label = label
            self.recordedAt = recordedAt
            self.context = context
            self.isNewest = isNewest
            self.itemID = itemID
        }

        var isStageStart: Bool {
            if case .stageStart = kind { true } else { false }
        }
    }

    let rows: [Row]
    /// Opens a row's item. Nil (a run's page, a screenshot of one) draws
    /// every row as plain text, never as a link that goes nowhere.
    var onOpenItem: ((String) -> Void)? = nil
    @Environment(\.timeZone) private var timeZone
    @Environment(\.locale) private var locale
    @Environment(\.pinnedNow) private var pinnedNow

    /// The gap under a row, and the wider one under a stage start, which
    /// sets the start apart from the older entry beneath it.
    private static let railGap: CGFloat = 10
    private static let railGapUnderStageStart: CGFloat = 18
    /// How far a connector is drawn from its marker before the row cuts it
    /// off: longer than any row at any text size.
    private static let connectorReach: CGFloat = 2000

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(rows.enumerated()), id: \.offset) { index, row in
                let isLast = index == rows.count - 1
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    ChronologyMarker(kind: Self.marker(row))
                        .background { connector(above: index > 0, below: !isLast) }
                        .alignmentGuide(.firstTextBaseline) { $0[.bottom] - 1 }
                    ViewThatFits(in: .horizontal) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            label(row)
                            Spacer(minLength: 8)
                            time(row)
                        }
                        VStack(alignment: .leading, spacing: 4) {
                            label(row)
                            time(row)
                        }
                    }
                }
                .padding(.bottom, isLast ? 0 : row.isStageStart ? Self.railGapUnderStageStart : Self.railGap)
                // Cuts the connector off at the row's edges, where the next
                // row's connector takes over.
                .clipped()
                .accessibilityElement(children: .combine)
            }
        }
    }

    /// A row's marker. A stage start keeps its green and a failure its wax
    /// at the head of the rail, where any other entry takes the ink dot.
    nonisolated static func marker(_ row: Row) -> ChronologyMarker.Kind {
        switch row.kind {
        case .stageStart: .stageStart
        case .failure: .failure
        case .event: row.isNewest ? .current : .prior
        }
    }

    /// The connector's two halves, each drawn from the marker's edge out
    /// past the row. The marker sits on the label's baseline, so its place
    /// in the row is not known here; the row's bounds end each half instead.
    /// Clipping hides the overflow without removing it from hit testing, so
    /// the connector takes no hits: it would otherwise cover a strip of the
    /// controls above and below the rail.
    private func connector(above: Bool, below: Bool) -> some View {
        let offset = (Self.connectorReach + ChronologyMarker.diameter) / 2
        return ZStack {
            if above { connectorHalf.offset(y: -offset) }
            if below { connectorHalf.offset(y: offset) }
        }
        .allowsHitTesting(false)
        .accessibilityHidden(true)
    }

    private var connectorHalf: some View {
        Rectangle()
            .fill(Color.milestoneConnector)
            .frame(width: 2, height: Self.connectorReach)
    }

    /// The newest entry and the stage start drawn from it are semibold. A
    /// failure reads in wax and a stage start in full ink wherever they
    /// sit; an older event is dimmed. The context, when there is one, sits
    /// under the label in the mono a trailing summary uses.
    private func label(_ row: Row) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            if let itemID = row.itemID, let onOpenItem {
                Button {
                    onOpenItem(itemID)
                } label: {
                    FreesideLink(
                        title: row.label,
                        face: FreesideFont.sans(.callout, weight: row.isNewest ? .semibold : .regular)
                    )
                    .multilineTextAlignment(.leading)
                    .fixedSize(horizontal: false, vertical: true)
                }
                .buttonStyle(.plain)
            } else {
                Text(row.label)
                    .font(FreesideFont.sans(.callout, weight: row.isNewest ? .semibold : .regular))
                    .foregroundStyle(Self.labelColor(row))
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let context = row.context {
                Text(context)
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    private static func labelColor(_ row: Row) -> Color {
        switch row.kind {
        case .failure: .waxText
        case .stageStart: .ink
        case .event: row.isNewest ? .ink : .inkDim
        }
    }

    @ViewBuilder
    private func time(_ row: Row) -> some View {
        if let recordedAt = row.recordedAt {
            Text(
                FreesideFormat.shortTime(
                    recordedAt, now: pinnedNow ?? Date(), locale: locale, timeZone: timeZone)
            )
            .font(FreesideFont.trailingSummary)
            .foregroundStyle(Color.inkDim)
            .exactInstant(recordedAt)
        }
    }
}
