import FreesideAPI
import SwiftUI

/// The final review's Review Guide module (plan §9 revision 112): the
/// agent's reading order for the pull request, drawn in the layering the
/// card keeps. The stops, their summaries, the captions, the questions, and
/// the notes are the agent's claim, quoted under the unverified register;
/// each excerpt's path, lines, and text are daemon facts. Every string
/// draws verbatim, so agent prose carries no live link and excerpt text is
/// never reformatted.
struct DecisionReviewGuideView: View {
    let guide: Components.Schemas.ReviewGuide
    /// The daemon's record of what it cut to fit the guide to its caps,
    /// drawn as daemon fact before the stops. Empty when nothing was cut.
    let trims: [Components.Schemas.ReviewGuideTrim]
    /// The invocation that wrote the guide, from its claim's provenance.
    /// Nil when the item carries no claim with the guide's digest.
    let producerInvocationID: String?
    let carriesInfo: Bool
    let rendersInteractiveControls: Bool
    /// Opens the pull request at one line of one file. Nil when the card
    /// offers no open-at-stop action (`opensReviewStops`).
    let openAtStop: ((_ path: String, _ line: Int) -> Void)?
    let actionsEnabled: Bool

    private typealias CardScale = DecisionCardComposition.Scale

    var body: some View {
        // Each stop, and the questions and notes after them, is set apart
        // by the section gap, so one stop's excerpts never read as the
        // next stop's.
        VStack(alignment: .leading, spacing: CardScale.sectionGap) {
            VStack(alignment: .leading, spacing: CardScale.controlGap) {
                UnverifiedLabel(
                    text: "Review Guide", carriesInfo: carriesInfo,
                    rendersInteractiveControls: rendersInteractiveControls)
                if let producerInvocationID {
                    Text(
                        "Source: agent invocation \(Text(verbatim: producerInvocationID).font(FreesideFont.monoValue))"
                    )
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
                }
            }
            if !trims.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    KeywordLabel(text: "Trimmed to Fit")
                    ForEach(Array(trims.enumerated()), id: \.offset) { _, trim in
                        Text(verbatim: AttentionDisplay.reviewGuideTrim(trim))
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .accessibilityElement(children: .combine)
            }
            ForEach(Array(guide.stops.enumerated()), id: \.offset) { index, stop in
                stopView(stop, number: index + 1)
            }
            if !guide.questions.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    KeywordLabel(text: guide.questions.count == 1 ? "Question" : "Questions")
                    QuoteBlock {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(Array(guide.questions.enumerated()), id: \.offset) { _, question in
                                prose(question)
                            }
                        }
                        .proseMeasure()
                    }
                }
            }
            if let notes = guide.notes, !notes.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    KeywordLabel(text: "Notes")
                    QuoteBlock { prose(notes).proseMeasure() }
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func stopView(_ stop: Components.Schemas.ReviewStop, number: Int) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            VStack(alignment: .leading, spacing: 4) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(verbatim: "\(number)")
                        .font(FreesideFont.monoValue)
                        .foregroundStyle(Color.inkDim)
                    KeywordLabel(text: AttentionDisplay.label(stop.attention))
                }
                Text(verbatim: stop.title)
                    .font(FreesideFont.factLabel)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                QuoteBlock { prose(stop.summary).proseMeasure() }
            }
            .accessibilityElement(children: .combine)
            .accessibilityLabel(
                AttentionDisplay.reviewStopAccessibilityLabel(
                    stop, number: number, of: guide.stops.count))
            ForEach(Array(stop.excerpts.enumerated()), id: \.offset) { _, excerpt in
                excerptView(excerpt)
            }
            if let openAtStop, let excerpt = DecisionCardComposition.openingExcerpt(stop) {
                // Outlined, never filled: the card keeps one filled control
                // (R6), and this is navigation beside it.
                Button {
                    openAtStop(excerpt.path, excerpt.start_line)
                } label: {
                    Text("Open in Pull Request")
                }
                .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
                .disabled(!actionsEnabled)
                .accessibilityLabel("Open stop \(number) in the pull request")
                .accessibilityHint(Text(verbatim: AttentionDisplay.excerptSpokenRange(excerpt)))
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func excerptView(_ excerpt: Components.Schemas.ReviewExcerpt) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                if excerpt.side == .base {
                    KeywordLabel(text: AttentionDisplay.label(excerpt.side))
                }
                Text(verbatim: AttentionDisplay.excerptRange(excerpt))
                    .font(FreesideFont.factLabel)
                    .foregroundStyle(Color.inkDim)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityElement(children: .ignore)
                    .accessibilityAddTraits(.isStaticText)
                    .accessibilityLabel(Text(verbatim: AttentionDisplay.excerptSpokenRange(excerpt)))
            }
            if let text = excerpt.text {
                excerptText(text, startLine: excerpt.start_line)
            } else if let reason = excerpt.unresolved_reason?.value1 {
                Text(AttentionDisplay.label(reason, side: excerpt.side))
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
            }
            QuoteBlock { prose(excerpt.caption).proseMeasure() }
        }
    }

    /// The daemon's read of the lines, numbered from the excerpt's first
    /// line. Lines never wrap: the block scrolls sideways, and an offscreen
    /// render, which cannot draw a scroll view, clips it instead. Each
    /// number shares a row with its line, so a line drawn taller than the
    /// rest (an emoji, a tall script) keeps its number beside it.
    @ViewBuilder
    private func excerptText(_ text: String, startLine: Int) -> some View {
        let lines = Self.lines(text)
        let block = Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 0) {
            ForEach(lines.indices, id: \.self) { index in
                GridRow(alignment: .firstTextBaseline) {
                    Text(verbatim: "\(startLine + index)")
                        .foregroundStyle(Color.inkDim)
                        .fixedSize()
                        .gridColumnAlignment(.trailing)
                        .accessibilityHidden(true)
                    // An empty line keeps its height.
                    Text(verbatim: lines[index].isEmpty ? " " : lines[index])
                        .foregroundStyle(Color.ink)
                        .fixedSize()
                        .textSelection(.enabled)
                }
            }
        }
        .font(FreesideFont.monoValue)
        .padding(10)
        Group {
            if rendersInteractiveControls {
                ScrollView(.horizontal) { block }
            } else {
                // A zero minimum, so the unwrapped lines clip at the card's
                // edge instead of widening the card.
                block.frame(minWidth: 0, maxWidth: .infinity, alignment: .leading).clipped()
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.ground2, in: RoundedRectangle(cornerRadius: 6))
    }

    /// The excerpt's lines, split where git splits them: at a line feed,
    /// which in a CRLF file Swift reads as one `"\r\n"` character. A final
    /// newline ends the last line rather than starting an empty one.
    nonisolated static func lines(_ text: String) -> [String] {
        var lines = text.split(omittingEmptySubsequences: false) { $0 == "\n" || $0 == "\r\n" }
            .map(String.init)
        if lines.count > 1, lines.last == "" { lines.removeLast() }
        return lines
    }

    private func prose(_ text: String) -> some View {
        Text(verbatim: text)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.ink)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// The Review Guide module of a ready card whose guide the daemon left out
/// (`review_guide_notice.omission`): a daemon fact, so it carries no
/// unverified register, and it says why the card shows no guide.
struct DecisionReviewGuideOmissionView: View {
    let omission: Components.Schemas.ReviewGuideOmission

    private typealias CardScale = DecisionCardComposition.Scale

    var body: some View {
        VStack(alignment: .leading, spacing: CardScale.controlGap) {
            KeywordLabel(text: "Review Guide")
            Text(verbatim: AttentionDisplay.label(omission))
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)
        }
        .accessibilityElement(children: .combine)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
