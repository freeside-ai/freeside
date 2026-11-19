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
        /// A dim sub line under the label, never folded: the attempt the
        /// entry belongs to on a run's rail (the task's events name theirs
        /// in the label), and on the task's events the reason a triage got
        /// no answer.
        var context: String?
        /// Why the entry happened, in the daemon's words: today a triage
        /// event's advisory reason. A rail given `explanationFold` folds it
        /// behind the entry's marker, except on a failure, whose reason is
        /// what the reader came for; otherwise it prints under the label.
        var explanation: String?
        /// The newest entry, which reads as the head of the rail. On the
        /// task's events a stage start merges into the event it is read
        /// from, so the newest entry can be a stage start.
        var isNewest = false
        /// The attention item the entry records a decision on, which opens
        /// its card. Nil where the client holds no such item.
        var itemID: String?

        init(
            kind: Kind, label: String, recordedAt: Date?, context: String? = nil, explanation: String? = nil,
            isNewest: Bool = false, itemID: String? = nil
        ) {
            self.kind = kind
            self.label = label
            self.recordedAt = recordedAt
            self.context = context
            self.explanation = explanation
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
    /// Drawn behind each row's top, by the row's `holdKeys` key: the task
    /// page's mark for holding the reader's place. Nil draws nothing.
    var rowMarker: ((String) -> AnyView)? = nil
    /// Gives each row its `holdKeys` key as its view id, so a scroll reader
    /// can bring a row into view: the run page's time strip does. Off by
    /// default, which leaves every row's identity as it was.
    var identifiesRows = false
    /// Where a row's explanation fold keeps its state. Nil (a run's page)
    /// prints every explanation; set, an entry with one folds it
    /// (`folds(_:)`).
    var explanationFold: ((Row) -> Binding<Bool>)? = nil
    @State private var hoveredRow: Int?
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

    /// A key per row that survives rows arriving above it: the row's
    /// content, and for rows alike in every field, which of them it is
    /// counted from the oldest. An index would move with every new row,
    /// since the rail runs newest first.
    nonisolated static func holdKeys(_ rows: [Row]) -> [String] {
        var seen: [String: Int] = [:]
        return rows.reversed().map { row in
            let content = [
                "\(row.kind)", row.label, row.recordedAt.map { "\($0.timeIntervalSince1970)" } ?? "-",
                row.context ?? "-", row.explanation ?? "-",
            ].joined(separator: "|")
            let count = seen[content, default: 0]
            seen[content] = count + 1
            return "\(content)#\(count)"
        }.reversed()
    }

    var body: some View {
        let keys = rowMarker == nil && !identifiesRows ? [] : Self.holdKeys(rows)
        VStack(alignment: .leading, spacing: 0) {
            ForEach(Array(rows.enumerated()), id: \.offset) { index, row in
                let isLast = index == rows.count - 1
                let fold = explanationFold.flatMap { Self.folds(row) ? $0(row) : nil }
                rowContent(row, index: index, isLast: isLast, fold: fold)
                    .padding(.bottom, isLast ? 0 : row.isStageStart ? Self.railGapUnderStageStart : Self.railGap)
                    // Cuts the connector off at the row's edges, where the
                    // next row's connector takes over.
                    .clipped()
                    .background(alignment: .top) {
                        if let rowMarker, index < keys.count { rowMarker(keys[index]) }
                    }
                    .rowID(identifiesRows && index < keys.count ? keys[index] : nil)
            }
        }
    }

    /// Whether a rail that folds explanations folds this row's: it has one
    /// and it isn't a failure, whose reason stays printed.
    nonisolated static func folds(_ row: Row) -> Bool {
        row.explanation != nil && row.kind != .failure
    }

    /// A row as one element. A folding row is one button that opens and
    /// closes its explanation, its marker the disclosure triangle.
    @ViewBuilder
    private func rowContent(_ row: Row, index: Int, isLast: Bool, fold: Binding<Bool>?) -> some View {
        if let fold {
            Button {
                fold.wrappedValue.toggle()
            } label: {
                line(row, index: index, isLast: isLast, disclosure: fold.wrappedValue ? .open : .closed)
                    .contentShape(Rectangle())
                    // Under the marker and its connector, inside the row,
                    // which clips everything past its edges.
                    .background {
                        RoundedRectangle(cornerRadius: 4)
                            .fill(Color.hover)
                            .opacity(hoveredRow == index ? 1 : 0)
                    }
            }
            .buttonStyle(.plain)
            .freesideFocusRing(cornerRadius: 4)
            // A button's label can't be selected, so the row's menu copies
            // what it draws (app/README.md, Copyable Text).
            .copyMenu {
                Button("Copy event") { Clipboard.copy(row.label) }
                if let explanation = row.explanation {
                    Button("Copy reason") { Clipboard.copy(explanation) }
                }
            }
            .onHover { hovering in
                if hovering {
                    hoveredRow = index
                } else if hoveredRow == index {
                    hoveredRow = nil
                }
            }
            .accessibilityElement(children: .combine)
            .accessibilityValue(fold.wrappedValue ? "Expanded" : "Collapsed")
            .accessibilityHint(fold.wrappedValue ? "Hides why it happened." : "Shows why it happened.")
        } else {
            line(row, index: index, isLast: isLast, disclosure: nil)
                .accessibilityElement(children: .combine)
        }
    }

    private func line(
        _ row: Row, index: Int, isLast: Bool, disclosure: ChronologyMarker.Disclosure?
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            ChronologyMarker(kind: Self.marker(row), disclosure: disclosure)
                .background { connector(above: index > 0, below: !isLast) }
                .alignmentGuide(.firstTextBaseline) { $0[.bottom] - 1 }
            VStack(alignment: .leading, spacing: 2) {
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
                // Under the label and its time, so a long explanation
                // wraps beneath them rather than pushing the time down.
                if disclosure != .closed, let explanation = row.explanation {
                    Text(explanation)
                        .font(FreesideFont.trailingSummary)
                        .foregroundStyle(Color.inkDim)
                        .fixedSize(horizontal: false, vertical: true)
                }
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

    /// Every label is the regular event title; color carries emphasis. The
    /// newest entry and a stage start read in full ink and a failure in wax
    /// wherever they sit; an older event is dimmed. The context, when there
    /// is one, sits under the label in the mono a trailing summary uses.
    private func label(_ row: Row) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            if let itemID = row.itemID, let onOpenItem {
                Button {
                    onOpenItem(itemID)
                } label: {
                    FreesideLink(
                        title: row.label,
                        face: FreesideFont.eventTitle
                    )
                    .multilineTextAlignment(.leading)
                    .fixedSize(horizontal: false, vertical: true)
                }
                .buttonStyle(.plain)
            } else {
                Text(row.label)
                    .font(FreesideFont.eventTitle)
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

extension View {
    /// The row's view id when it has one; with none the row keeps the
    /// identity its `ForEach` gives it.
    @ViewBuilder
    fileprivate func rowID(_ id: String?) -> some View {
        if let id { self.id(id) } else { self }
    }
}
