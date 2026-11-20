import FreesideModel
import SwiftUI

/// A writer's transcript read as a conversation (#1131's design rules), with
/// the raw stream one link away. Everything in it is the agent's own
/// account, so the head says Unverified and the sheet adds no daemon facts.
/// The only time it shows is the invocation's working time, which the
/// opener reads from the run's clock so it agrees with the run's strip.
///
/// Nothing is cut at the head: both views hold the whole transcript the
/// loader fetched, and each opens at its end. Both are lazy, because a
/// failed writer's transcript runs to megabytes.
struct AgentTranscriptSheet: View {
    enum Outcome: Equatable {
        /// The run's work folds behind `Worked for` above the final message.
        case finished
        /// Nothing folds, and the sheet closes on `Failed after`.
        case failed
    }

    /// The transcript's digest: which transcript the remembered folds
    /// belong to.
    let digest: String
    let bytes: [UInt8]
    let mediaType: String
    /// The stage and attempt, such as `Implementation · Attempt 1`.
    let title: String
    let outcome: Outcome
    /// The invocation's working time; nil drops the time from the closing
    /// line rather than guessing one.
    var workingTime: TimeInterval? = nil
    /// The prompt the writer received, which the stream doesn't carry; nil
    /// starts the conversation at the agent's first message.
    var prompt: String? = nil
    var folds: AgentTranscriptFoldPreferences = .shared
    /// The screenshot composition lays the content out in place of the
    /// scroll view, whose height ImageRenderer cannot settle, and clips it
    /// to this height when one is given: the conversation from its end,
    /// the raw stream from its head, as each opens.
    var rendersScrollableContent = true
    var screenshotHeight: CGFloat? = nil

    @State private var presentation: AgentTranscriptPresentation?
    @State private var showsRaw: Bool
    @Environment(\.dismiss) private var dismiss
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @Environment(\.locale) private var locale

    /// `presentation` given skips the parse, for screenshots; the live
    /// sheet parses `bytes` off the main actor when it opens.
    init(
        digest: String, bytes: [UInt8], mediaType: String, title: String, outcome: Outcome,
        workingTime: TimeInterval? = nil, prompt: String? = nil, folds: AgentTranscriptFoldPreferences = .shared,
        presentation: AgentTranscriptPresentation? = nil, showsRaw: Bool = false,
        rendersScrollableContent: Bool = true, screenshotHeight: CGFloat? = nil
    ) {
        self.digest = digest
        self.bytes = bytes
        self.mediaType = mediaType
        self.title = title
        self.outcome = outcome
        self.workingTime = workingTime
        self.prompt = prompt
        self.folds = folds
        self.rendersScrollableContent = rendersScrollableContent
        self.screenshotHeight = screenshotHeight
        _presentation = State(initialValue: presentation)
        _showsRaw = State(initialValue: showsRaw)
    }

    /// The phone's sheet: `Raw` beside the `Done` pill at the top, no foot.
    private var isCompact: Bool { horizontalSizeClass == .compact }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            if isCompact {
                HStack {
                    viewLink
                    Spacer(minLength: 12)
                    Button("Done") { dismiss() }
                        .buttonStyle(FreesideActionButtonStyle(tone: .secondary, expands: false))
                        .keyboardShortcut(.cancelAction)
                }
                .padding(.horizontal, 16)
                .padding(.top, 14)
            }
            HStack(alignment: .top, spacing: 12) {
                VStack(alignment: .leading, spacing: 8) {
                    KeywordLabel(text: "Agent transcript · Unverified")
                    Text(title)
                        .font(FreesideFont.sheetAsk)
                        .foregroundStyle(Color.ink)
                        .accessibilityAddTraits(.isHeader)
                }
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
                if !isCompact { viewLink }
            }
            .padding(.horizontal, isCompact ? 16 : 20)
            .padding(.top, isCompact ? 14 : 16)
            // The head keeps 8 of the 20 points above the content, so
            // a scrolled row never meets the title's descenders.
            .padding(.bottom, 8)
            Group {
                if let presentation {
                    scrolled(presentation)
                } else {
                    ProgressView("Reading the transcript…")
                        .font(FreesideFont.cardBody)
                        .padding(isCompact ? 16 : 20)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            if !isCompact {
                FreesideSheetActionRow.done { dismiss() }
            }
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .modifier(MacSheetSize(applies: rendersScrollableContent))
        .task {
            guard presentation == nil else { return }
            let bytes = bytes
            let parsing = Task.detached(priority: .userInitiated) {
                AgentTranscriptPresentation(bytes: bytes, streamEnded: true)
            }
            let parsed = await parsing.value
            guard !Task.isCancelled else { return }
            presentation = parsed
        }
    }

    /// `Raw` swaps the conversation for the stream; the stream's link
    /// reads `Conversation`.
    private var viewLink: some View {
        Button(showsRaw ? "Conversation" : "Raw") { showsRaw.toggle() }
            .buttonStyle(.plain)
            .font(FreesideFont.noticeAction)
            .foregroundStyle(Color.accentText)
    }

    @ViewBuilder
    private func scrolled(_ presentation: AgentTranscriptPresentation) -> some View {
        let content = Group {
            if showsRaw {
                RawView(presentation: presentation, mediaType: mediaType, lazy: rendersScrollableContent)
            } else {
                ConversationContent(
                    presentation: presentation, outcome: outcome, workingTime: workingTime, prompt: prompt,
                    lazy: rendersScrollableContent,
                    fold: { key in
                        Binding(
                            get: { folds.isOpen(key, transcript: digest) },
                            set: { folds.setOpen($0, fold: key, transcript: digest) })
                    })
            }
        }
        .padding(.horizontal, isCompact ? 16 : 20)
        .padding(.top, 12)
        .padding(.bottom, 24)
        .frame(maxWidth: .infinity, alignment: .leading)
        // A long conversation opens at its end (rule 10); the raw stream
        // opens at its head, under the line that says what it is.
        let anchor: UnitPoint = showsRaw ? .top : .bottom
        if rendersScrollableContent {
            // Only the first offset is anchored, so a fold opened higher up
            // grows downward, and each view opens at its own anchor.
            ScrollView {
                content
            }
            .defaultScrollAnchor(anchor, for: .initialOffset)
            .id(showsRaw)
        } else if let screenshotHeight {
            content
                .fixedSize(horizontal: false, vertical: true)
                .frame(
                    height: screenshotHeight,
                    alignment: Alignment(horizontal: .leading, vertical: anchor == .top ? .top : .bottom)
                )
                .clipped()
        } else {
            content
        }
    }
}

/// A Mac sheet takes its content's ideal size, and a scroll view has none:
/// the width and height the card's other long sheets take. The screenshot
/// composition lays its content out in place, so it takes none.
private struct MacSheetSize: ViewModifier {
    let applies: Bool

    func body(content: Content) -> some View {
        #if os(macOS)
            if applies {
                content.frame(minWidth: 480, idealWidth: 720, minHeight: 420, idealHeight: 640)
            } else {
                content
            }
        #else
            content
        #endif
    }
}

/// A lazy stack in the live sheet, so a transcript of megabytes lays out
/// only the rows on screen; a plain one in a screenshot, which takes the
/// sheet's ideal height, because a lazy stack's ideal height is an
/// estimate and its overflow draws over the head.
private struct TranscriptStack<Content: View>: View {
    let lazy: Bool
    let spacing: CGFloat
    @ViewBuilder let content: Content

    var body: some View {
        if lazy {
            LazyVStack(alignment: .leading, spacing: spacing) { content }
        } else {
            VStack(alignment: .leading, spacing: spacing) { content }
        }
    }
}

/// The conversation: the agent's prose, its work collapsed to dim
/// sentences, and the run's ending.
private struct ConversationContent: View {
    let presentation: AgentTranscriptPresentation
    let outcome: AgentTranscriptSheet.Outcome
    let workingTime: TimeInterval?
    let prompt: String?
    let lazy: Bool
    /// The open state of the fold with this key, remembered per transcript.
    let fold: (String) -> Binding<Bool>
    @Environment(\.locale) private var locale

    var body: some View {
        TranscriptStack(lazy: lazy, spacing: 13) {
            // Rule 2: the conversation starts with the prompt, above the
            // `Worked for` fold and the failed run's entries alike.
            if let prompt {
                PromptBubble(text: prompt)
            }
            switch outcome {
            case .finished:
                // Rule 8: the work folds behind `Worked for`, above a
                // hairline and the final message.
                FoldRow(
                    isOpen: fold("worked"), isFoldable: !presentation.entries.isEmpty
                ) {
                    Text(AgentTranscriptSheetText.worked(workingTime, locale: locale))
                        .foregroundStyle(Color.inkDim)
                }
                .padding(.bottom, 9)
                .overlay(alignment: .bottom) { Hairline() }
                if fold("worked").wrappedValue {
                    entries
                    if presentation.result?.message != nil { Hairline() }
                }
                if let message = presentation.result?.message {
                    MessageText(text: message)
                }
            case .failed:
                // Rule 9: nothing folds, and the run's end is said.
                entries
                if let message = presentation.result?.message {
                    MessageText(text: message)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(AgentTranscriptSheetText.failed(workingTime, locale: locale))
                        .font(FreesideFont.cardBody(strong: true, code: false))
                        .foregroundStyle(Color.waxText)
                    Text(presentation.failedEnding)
                        .foregroundStyle(Color.inkDim)
                }
                .padding(.top, 11)
                .frame(maxWidth: .infinity, alignment: .leading)
                .overlay(alignment: .top) { Hairline() }
            }
            if !presentation.unreadable.isEmpty {
                unreadable
            }
        }
        .font(FreesideFont.factLabel)
        .textSelection(.enabled)
    }

    @ViewBuilder
    private var entries: some View {
        ForEach(presentation.entries) { entry in
            switch entry {
            case .message(_, let text):
                MessageText(text: text)
            case .reasoning(let reasoning):
                ReasoningRow(reasoning: reasoning, isOpen: fold("thought-\(reasoning.id)"))
            case .work(let work):
                WorkGroup(work: work, fold: fold)
            }
        }
    }

    /// Rule 11: one quiet line after the conversation, opening to each
    /// line with its number. Each line is its own row, so a stream the
    /// parser can't read at all stays lazy.
    @ViewBuilder
    private var unreadable: some View {
        let count = presentation.unreadable.count
        FoldRow(isOpen: fold("unreadable")) {
            Text("\(count.formatted()) line\(count == 1 ? "" : "s") Freeside could not read")
                .foregroundStyle(Color.inkDim)
        }
        if fold("unreadable").wrappedValue {
            ForEach(presentation.unreadable) { line in
                VStack(alignment: .leading, spacing: 2) {
                    Text("Line \(line.id.formatted())").foregroundStyle(Color.inkDim)
                    Text(line.text).foregroundStyle(Color.ink)
                }
                .font(FreesideFont.trailingSummary)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 12)
                .padding(.vertical, 9)
                .background(Color.ground, in: RoundedRectangle(cornerRadius: 6))
            }
        }
    }
}

/// The words the sheet builds from the run's clock and the stream's end.
@MainActor enum AgentTranscriptSheetText {
    static func worked(_ time: TimeInterval?, locale: Locale) -> String {
        time.map { "Worked for \(RunTimeStripView.duration($0, locale: locale))" } ?? "Worked"
    }

    static func failed(_ time: TimeInterval?, locale: Locale) -> String {
        time.map { "Failed after \(RunTimeStripView.duration($0, locale: locale))" } ?? "Failed"
    }

    /// The agent's inline Markdown with every link's action removed and
    /// its destination shown after its label, the policy
    /// `SpecificationMarkdown` keeps for agent text: a label on an
    /// unverified transcript must not hide where it points.
    nonisolated static func inertMarkdown(_ text: String) -> AttributedString {
        guard
            var attributed = try? AttributedString(
                markdown: text, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))
        else { return AttributedString(text) }
        // Back to front, so an insertion leaves the earlier ranges valid.
        for (link, range) in attributed.runs[\.link].reversed() {
            guard let link else { continue }
            attributed[range].link = nil
            let destination = link.absoluteString
            if String(attributed[range].characters) != destination {
                attributed.insert(AttributedString(" (\(destination))"), at: range.upperBound)
            }
        }
        return attributed
    }
}

/// The agent's prose on the leading side, with its inline Markdown
/// (code, emphasis) drawn, its links inert, and its line breaks kept.
private struct MessageText: View {
    let text: String

    var body: some View {
        Text(AgentTranscriptSheetText.inertMarkdown(text))
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.ink)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// Rule 2: the prompt on the trailing side, the operator's side of the
/// conversation. It is daemon-rendered text, so it draws verbatim, with no
/// Markdown read into it.
private struct PromptBubble: View {
    let text: String

    var body: some View {
        Text(verbatim: text)
            .font(FreesideFont.cardBody)
            .foregroundStyle(Color.ink)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.horizontal, 12)
            .padding(.vertical, 9)
            .background(Color.ground3, in: RoundedRectangle(cornerRadius: 10))
            .padding(.leading, 48)
            .frame(maxWidth: .infinity, alignment: .trailing)
            // Its own element before the label, so the label never reaches
            // the selectable Text through the wrappers (#2993).
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("Prompt: \(text)")
    }
}

/// A one-line control that opens a fold: its label, then a chevron.
private struct FoldRow<Label: View>: View {
    @Binding var isOpen: Bool
    var isFoldable = true
    @ViewBuilder let label: () -> Label

    var body: some View {
        if isFoldable {
            Button {
                isOpen.toggle()
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    label()
                    Chevron(isOpen: isOpen)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityValue(isOpen ? "Expanded" : "Collapsed")
            .frame(maxWidth: .infinity, alignment: .leading)
        } else {
            label().frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

private struct Chevron: View {
    let isOpen: Bool

    var body: some View {
        Image(systemName: isOpen ? "chevron.down" : "chevron.right")
            .font(FreesideFont.disclosureGlyph)
            .foregroundStyle(Color.inkFaint)
            .accessibilityHidden(true)
    }
}

private struct Hairline: View {
    var body: some View {
        Rectangle().fill(Color.rule).frame(height: 1).frame(maxWidth: .infinity)
    }
}

/// Rule 7: `Thought` and the reasoning's first line, cut at the column's
/// edge, opening to the whole text.
private struct ReasoningRow: View {
    let reasoning: AgentTranscriptPresentation.Reasoning
    @Binding var isOpen: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            FoldRow(isOpen: $isOpen) {
                HStack(alignment: .firstTextBaseline, spacing: 7) {
                    Text("Thought").foregroundStyle(Color.inkDim).fixedSize()
                    if !isOpen {
                        Text(reasoning.preview).foregroundStyle(Color.inkFaint).lineLimit(1)
                    }
                }
            }
            if isOpen {
                Text(reasoning.text)
                    .foregroundStyle(Color.inkDim)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.leading, 14)
            }
        }
    }
}

/// Rules 4 and 5: a run of calls as one dim sentence, opening to one line
/// a call. A group of one call is that call's line.
private struct WorkGroup: View {
    let work: AgentTranscriptPresentation.Work
    let fold: (String) -> Binding<Bool>

    var body: some View {
        if let summary = work.summary {
            let isOpen = fold("work-\(work.id)")
            VStack(alignment: .leading, spacing: 5) {
                FoldRow(isOpen: isOpen) {
                    Text(summary).foregroundStyle(Color.inkDim)
                }
                if isOpen.wrappedValue {
                    VStack(alignment: .leading, spacing: 3) {
                        ForEach(work.steps) { step in
                            switch step {
                            case .call(let call):
                                CallRow(call: call, fold: fold)
                            case .reasoning(let reasoning):
                                ReasoningRow(reasoning: reasoning, isOpen: fold("thought-\(reasoning.id)"))
                            }
                        }
                    }
                    .padding(.leading, 14)
                }
            }
        } else if let call = work.calls.first {
            CallRow(call: call, fold: fold)
        }
    }
}

/// One call: a dim verb and its identifier in mono, a failed command's
/// exit code in wax, and the call's output a fold away.
private struct CallRow: View {
    let call: AgentTranscriptPresentation.Call
    let fold: (String) -> Binding<Bool>

    /// A successful edit's result only says the file changed, so it has
    /// no fold; every other call with output opens to it.
    private var output: AgentTranscriptPresentation.Output? {
        call.kind == .edit && !call.failed ? nil : call.output
    }

    var body: some View {
        let isOpen = fold("call-\(call.id)")
        VStack(alignment: .leading, spacing: 6) {
            FoldRow(isOpen: isOpen, isFoldable: output != nil) {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Text(call.verb).foregroundStyle(Color.inkDim).fixedSize()
                    if let identifier = call.identifier {
                        Text(identifier)
                            .font(FreesideFont.monoValue)
                            .foregroundStyle(Color.ink)
                            .lineLimit(1)
                            .truncationMode(.tail)
                            .copyMenu {
                                Button("Copy") { Clipboard.copy(identifier) }
                            }
                    }
                    if call.failed {
                        Text(call.exitCode.map { "exit \($0)" } ?? "failed")
                            .font(FreesideFont.monoValue)
                            .foregroundStyle(Color.waxText)
                            .fixedSize()
                            .padding(.leading, 3)
                    }
                }
            }
            if let output, isOpen.wrappedValue {
                OutputBlock(output: output, showsAll: fold("all-\(call.id)"))
            }
        }
    }
}

/// Rule 6: a call's output on the ground, showing a long one's end.
private struct OutputBlock: View {
    let output: AgentTranscriptPresentation.Output
    @Binding var showsAll: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let tail = output.tail {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Text(
                        showsAll
                            ? "All \(output.lineCount.formatted()) lines"
                            : "Last \(AgentTranscriptPresentation.Output.tailLineLimit) of \(output.lineCount.formatted()) lines"
                    )
                    .foregroundStyle(Color.inkDim)
                    Button(showsAll ? "Show Last \(AgentTranscriptPresentation.Output.tailLineLimit)" : "Show All") {
                        showsAll.toggle()
                    }
                    .buttonStyle(.plain)
                    .foregroundStyle(Color.accentText)
                }
                Text(showsAll ? output.text : tail).foregroundStyle(Color.ink)
            } else {
                Text(output.text).foregroundStyle(Color.ink)
            }
        }
        .font(FreesideFont.trailingSummary)
        .fixedSize(horizontal: false, vertical: true)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .background(Color.ground, in: RoundedRectangle(cornerRadius: 6))
    }
}

/// Rule 12: the stream as written, every line numbered, under a dim line
/// with its media type, size, and line count.
private struct RawView: View {
    let presentation: AgentTranscriptPresentation
    let mediaType: String
    let lazy: Bool

    var body: some View {
        let lines = presentation.lines
        let size = ByteCountFormatter.string(fromByteCount: Int64(presentation.byteCount), countStyle: .file)
        let digits = String(lines.count).count
        TranscriptStack(lazy: lazy, spacing: 2) {
            Text("Raw stream · \(mediaType) · \(size) · \(lines.count.formatted()) lines")
                .foregroundStyle(Color.inkDim)
                .padding(.bottom, 8)
            ForEach(lines.indices, id: \.self) { index in
                let number = String(index + 1)
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    // Padded in the mono face, so every line's text starts
                    // at one column.
                    Text(String(repeating: " ", count: digits - number.count) + number)
                        .foregroundStyle(Color.inkFaint)
                        .accessibilityLabel("Line \(number)")
                    Text(lines[index])
                        .foregroundStyle(Color.ink)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
        .font(FreesideFont.trailingSummary)
        .textSelection(.enabled)
    }
}
