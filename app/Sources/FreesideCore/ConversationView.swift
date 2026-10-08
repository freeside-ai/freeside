import FreesideAPI
import SwiftUI

#if os(iOS)
    import UIKit
#endif

/// Bounded conversation messages (visual audit D04), and what a message
/// says about itself without printing it (R5).
enum ConversationPresentation {
    /// The lines a collapsed long body shows.
    static let collapsedLineLimit = 6

    /// How much of the thread's width one message may take, so its side
    /// reads as its author.
    static let messageWidthFraction: CGFloat = 0.82

    /// The expanded set after the control under one message is pressed:
    /// only that message changes.
    static func toggling(_ messageID: String, in expanded: Set<String>) -> Set<String> {
        expanded.symmetricDifference([messageID])
    }

    static func authorLabel(_ author: Components.Schemas.Author) -> String {
        switch author {
        case .user: "You"
        case .agent: "Agent"
        case .daemon: "Freeside"
        }
    }

    /// What VoiceOver reads for one message. The thread prints neither an
    /// author nor a time (R5), so the spoken label leads with both and the
    /// body follows.
    static func accessibilityLabel(
        for message: Components.Schemas.Message, now: Date
    ) -> String {
        let time = AttentionDisplay.relativeRowTime(message.created_at, now: now)
        return "\(authorLabel(message.author)), \(time). \(message.body)"
    }
}

/// Offers its content the height of its probe, so a `ViewThatFits` inside
/// can ask whether the whole message fits the collapsed line limit. The
/// answer comes from the layout pass itself, at the real width and text
/// size, with no measured state to arrive a frame late; a body is bounded
/// exactly when the limit would cut it, so the control never reveals
/// nothing. The content keeps whatever height it then needs.
private struct ProbeBoundedLayout: Layout {
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        subviews[1].sizeThatFits(bounded(width: proposal.width, subviews))
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        subviews[0].place(at: bounds.origin, proposal: ProposedViewSize(width: bounds.width, height: nil))
        subviews[1].place(at: bounds.origin, proposal: bounded(width: bounds.width, subviews))
    }

    private func bounded(width: CGFloat?, _ subviews: Subviews) -> ProposedViewSize {
        let probe = subviews[0].sizeThatFits(ProposedViewSize(width: width, height: nil))
        return ProposedViewSize(width: width, height: probe.height)
    }
}

/// One message's row in the thread: the message hugs its text up to
/// `messageWidthFraction` of the row and sits on its author's side.
private struct MessageRowLayout: Layout {
    let alignsTrailing: Bool

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let message = subviews[0].sizeThatFits(capped(proposal))
        return CGSize(width: proposal.width ?? message.width, height: message.height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let capped = capped(ProposedViewSize(width: bounds.width, height: nil))
        let message = subviews[0].sizeThatFits(capped)
        subviews[0].place(
            at: CGPoint(x: alignsTrailing ? bounds.maxX - message.width : bounds.minX, y: bounds.minY),
            proposal: ProposedViewSize(message))
    }

    private func capped(_ proposal: ProposedViewSize) -> ProposedViewSize {
        ProposedViewSize(
            width: proposal.width.map { $0 * ConversationPresentation.messageWidthFraction },
            height: nil)
    }
}

/// The thread (R5): the agent's messages quoted on the left, the operator's
/// bordered on the right, and the daemon's bordered on the left under its
/// producer label, since its side and shape alone do not say who spoke.
/// No message prints an author or a time. VoiceOver reads both with every
/// body, and the exact instant is one gesture away (R17).
struct ConversationView: View {
    let snapshot: Components.Schemas.ConversationSnapshot
    let attachments: AttachmentLoader
    let loadsAttachments: Bool
    var now = Date.now
    var rendersInteractiveControls = true
    /// The long messages shown in full, by message id, so a reorder or an
    /// appended message never moves the state to another message. It lasts
    /// while the conversation stays on screen and is never saved.
    @State private var expandedMessageIDs: Set<String>

    /// `initiallyExpandedMessageIDs` lets a screenshot golden draw the
    /// expanded state; the app always starts with every long body bounded.
    init(
        snapshot: Components.Schemas.ConversationSnapshot,
        attachments: AttachmentLoader,
        loadsAttachments: Bool,
        now: Date = .now,
        rendersInteractiveControls: Bool = true,
        initiallyExpandedMessageIDs: Set<String> = []
    ) {
        self.snapshot = snapshot
        self.attachments = attachments
        self.loadsAttachments = loadsAttachments
        self.now = now
        self.rendersInteractiveControls = rendersInteractiveControls
        _expandedMessageIDs = State(initialValue: initiallyExpandedMessageIDs)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            KeywordLabel(text: "Conversation")
                .accessibilityAddTraits(.isHeader)

            ForEach(snapshot.conversation.messages.sorted(by: { $0.sequence < $1.sequence }), id: \.id) {
                message in
                MessageRowLayout(alignsTrailing: message.author == .user) {
                    exactTime(message, on: messageShape(message))
                }
            }

            if snapshot.conversation.status == .awaiting_agent {
                HStack(spacing: 8) {
                    if rendersInteractiveControls {
                        ProgressView().controlSize(.small)
                    } else {
                        Image(systemName: "clock")
                    }
                    Text("Awaiting the agent's reply")
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                }
                .accessibilityElement(children: .combine)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder
    private func messageShape(_ message: Components.Schemas.Message) -> some View {
        switch message.author {
        case .agent:
            messageContent(message)
                .quoteSurface(cornerRadius: 8)
        case .user, .daemon:
            messageContent(message)
                .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
        }
    }

    private func messageContent(_ message: Components.Schemas.Message) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            if message.author == .daemon {
                KeywordLabel(text: ConversationPresentation.authorLabel(message.author))
                    // The spoken label already leads with the author.
                    .accessibilityHidden(true)
            }
            messageBody(message)
            ForEach(Array(message.attachments.enumerated()), id: \.offset) { index, digest in
                DecisionDetailView.AttachmentRow(
                    label: "Attachment \(index + 1)",
                    digest: digest,
                    attachments: attachments,
                    loadsAttachments: loadsAttachments,
                    rendersInteractiveControls: rendersInteractiveControls)
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    /// The body in full when it fits the collapsed line limit, otherwise
    /// bounded with the disclosure that expands it in place (R3).
    private func messageBody(_ message: Components.Schemas.Message) -> some View {
        let isExpanded = Binding(
            get: { expandedMessageIDs.contains(message.id) },
            set: { expanded in
                if expanded != expandedMessageIDs.contains(message.id) {
                    expandedMessageIDs = ConversationPresentation.toggling(
                        message.id, in: expandedMessageIDs)
                }
            })
        return ProbeBoundedLayout {
            bodyText(message, bounded: true)
                .hidden()
                .accessibilityHidden(true)
            ViewThatFits(in: .vertical) {
                bodyText(message, bounded: false)
                VStack(alignment: .leading, spacing: 6) {
                    bodyText(message, bounded: !isExpanded.wrappedValue)
                    SentenceDisclosure(label: "Full Message", isExpanded: isExpanded) {}
                }
            }
        }
    }

    /// Always the whole body: the line limit bounds what is drawn, never
    /// the text, so selection and copy are not handed a shortened string.
    private func bodyText(_ message: Components.Schemas.Message, bounded: Bool) -> some View {
        Text(message.body)
            .font(message.author == .agent ? FreesideFont.message : FreesideFont.cardBody)
            .foregroundStyle(Color.ink)
            .lineLimit(bounded ? ConversationPresentation.collapsedLineLimit : nil)
            .fixedSize(horizontal: false, vertical: true)
            .wholeBodySelection()
            .accessibilityLabel(ConversationPresentation.accessibilityLabel(for: message, now: now))
    }

    /// The exact instant of a message (R17): hover help on the Mac. On iOS
    /// one long-press menu carries it beside Copy Message, because a
    /// selectable body would claim the long press for its own menu and leave
    /// the time reachable only from the message's padding.
    @ViewBuilder
    private func exactTime(_ message: Components.Schemas.Message, on content: some View) -> some View {
        #if os(iOS)
            let exact = FreesideFormat.exactTime(message.created_at)
            content.contextMenu {
                Button("Copy Message") {
                    UIPasteboard.general.string = message.body
                }
                Button("Copy \(exact)") {
                    UIPasteboard.general.string = exact
                }
            }
        #else
            content.exactInstant(message.created_at)
        #endif
    }
}

extension View {
    /// Selection of a message body where a pointer can drag one. On iOS a
    /// selectable `Text` offers only its own whole-body Copy, which the
    /// message's long-press menu carries instead.
    @ViewBuilder
    fileprivate func wholeBodySelection() -> some View {
        #if os(iOS)
            self
        #else
            textSelection(.enabled)
        #endif
    }
}

extension View {
    /// Opens a scroll view on the end of its content. Before iOS 18 and
    /// macOS 15 the anchor also pins content shorter than the view to its
    /// bottom, so those systems keep opening on the start.
    @ViewBuilder
    fileprivate func opensOnItsEnd(_ opens: Bool) -> some View {
        if #available(iOS 18, macOS 15, *) {
            defaultScrollAnchor(opens ? .bottom : nil, for: .initialOffset)
        } else {
            self
        }
    }
}

struct MessageComposerSheet: View {
    /// The conversation a composer draws above its field, so the operator
    /// writes with the thread in view.
    struct Thread {
        let snapshot: Components.Schemas.ConversationSnapshot
        let attachments: AttachmentLoader
        let loadsAttachments: Bool
        var now = Date.now
    }

    static let fieldPlaceholder = "Write to the agent\u{2026}"

    @Environment(\.dismiss) private var dismiss
    @State private var message = ""
    /// The keyword naming the command the sheet carries out.
    let eyebrow: String
    /// What the operator is asked to write.
    let ask: String
    /// What sending does, where there is more to say than what to type.
    var consequence: String? = nil
    let submitLabel: String
    var byteLimit: Int?
    var rendersInteractiveControls = true
    /// The answer routes this composer lets the operator choose between, in
    /// display order; empty for a composer that carries no route (#1083). The
    /// first is the default selection.
    var routeOptions: [Components.Schemas.AnswerRoute] = []
    /// `nil` for every use but Discuss, and for a first message, which has
    /// no thread to draw.
    var thread: Thread? = nil
    let submit: (String, Components.Schemas.AnswerRoute?) async -> Bool
    @State private var isSubmitting = false
    @State private var chosenRoute: Components.Schemas.AnswerRoute?
    @FocusState private var fieldIsFocused: Bool

    private var selectedRoute: Components.Schemas.AnswerRoute? {
        chosenRoute ?? routeOptions.first
    }

    /// The thread the sheet draws: none for a first message.
    private var drawnThread: Thread? {
        thread.flatMap { $0.snapshot.conversation.messages.isEmpty ? nil : $0 }
    }

    private var trimmedMessage: String {
        message.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private var byteCount: Int {
        trimmedMessage.lengthOfBytes(using: .utf8)
    }

    private var canSubmit: Bool {
        !trimmedMessage.isEmpty && byteLimit.map { byteCount <= $0 } != false
    }

    var body: some View {
        VStack(spacing: 0) {
            // The header, the thread, and the field scroll together; the
            // footer stays put (R11).
            if rendersInteractiveControls {
                // The thread is history: a sheet that draws one opens on
                // the field under it, not on the oldest message with the
                // field out of view.
                ScrollView { composerContent }
                    .opensOnItsEnd(drawnThread != nil)
            } else {
                composerContent
            }

            // The submit lives in the sheet body rather than a toolbar so it
            // carries the design language's primary recipe; the row keeps
            // the Return and Escape bindings the placements supplied.
            FreesideSheetActionRow(
                submitLabel: submitLabel,
                isSubmitEnabled: canSubmit && !isSubmitting,
                cancelIsOutlined: true,
                submit: performSubmit,
                cancel: { dismiss() })
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 340)
    }

    private var composerContent: some View {
        VStack(spacing: 0) {
            // The header scrolls with the field, so the ask can take the
            // lines it needs at the largest text sizes.
            FreesideSheetHeader(
                eyebrow: eyebrow, ask: ask, consequence: consequence, askLineLimit: nil)
            VStack(alignment: .leading, spacing: 22) {
                if let thread = drawnThread {
                    ConversationView(
                        snapshot: thread.snapshot,
                        attachments: thread.attachments,
                        loadsAttachments: thread.loadsAttachments,
                        now: thread.now,
                        rendersInteractiveControls: rendersInteractiveControls)
                }
                messageField
                if routeOptions.count > 1, let defaultRoute = routeOptions.first {
                    // The daemon types the routes, so they are plain items:
                    // no agent proposed the default, and the source states
                    // no consequence for either (R29).
                    VStack(alignment: .leading, spacing: 10) {
                        KeywordLabel(text: "Route")
                        ChoiceList(
                            accessibilityLabel: "What to do with the answer",
                            options: routeOptions.map {
                                .init(
                                    value: $0, label: AgentQuestionPresentation.answerRouteLabel($0),
                                    register: .item)
                            },
                            selection: Binding(
                                get: { selectedRoute ?? defaultRoute },
                                set: { chosenRoute = $0 }))
                    }
                }
            }
            .padding(.horizontal, 16)
            .padding(.top, 6)
            .padding(.bottom, 16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var messageField: some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: "Message")
                // The editor carries the same name.
                .accessibilityHidden(true)
            if rendersInteractiveControls {
                TextEditor(text: $message)
                    .font(FreesideFont.callout)
                    .foregroundStyle(Color.ink)
                    .scrollContentBackground(.hidden)
                    .focused($fieldIsFocused)
                    .padding(Self.editorPadding)
                    .frame(minHeight: Self.fieldMinHeight)
                    .background(fieldFrame(isFocused: fieldIsFocused))
                    // A TextEditor has no prompt of its own. This one is
                    // drawn over the empty editor, never written into
                    // `message`, and leaves clicks and VoiceOver to the
                    // editor beneath it.
                    .overlay(alignment: .topLeading) {
                        if message.isEmpty {
                            placeholder
                                .allowsHitTesting(false)
                                .accessibilityHidden(true)
                        }
                    }
                    .accessibilityLabel("Message")
            } else {
                placeholder
                    .frame(maxWidth: .infinity, minHeight: Self.fieldMinHeight, alignment: .topLeading)
                    .background(fieldFrame(isFocused: false))
            }
            if let byteLimit {
                // A fixed face: the platform caption draws under the 11.5pt
                // floor on macOS.
                Text("\(byteCount) of \(byteLimit) bytes")
                    .font(FreesideFont.trailingSummary)
                    .foregroundStyle(byteCount > byteLimit ? Color.waxText : Color.inkDim)
                    .frame(maxWidth: .infinity, alignment: .trailing)
            }
        }
    }

    private var placeholder: some View {
        Text(Self.fieldPlaceholder)
            .font(FreesideFont.callout)
            .foregroundStyle(Color.inkFaint)
            .padding(.horizontal, 12)
            .padding(.vertical, 10)
    }

    /// The field's frame: a strong rule on the sheet's own ground, and the
    /// accent ring just outside it while the editor has keyboard focus
    /// (R19).
    private func fieldFrame(isFocused: Bool) -> some View {
        RoundedRectangle(cornerRadius: 6)
            .strokeBorder(Color.ruleStrong, lineWidth: 1)
            .overlay(
                RoundedRectangle(cornerRadius: 6)
                    .strokeBorder(isFocused ? Color.accentBorder : .clear, lineWidth: 1)
                    .padding(-3))
    }

    private static let fieldMinHeight: CGFloat = 120

    // A TextEditor insets a line by 5pt on both platforms, and UITextView
    // adds 8pt above and below the text; the padding makes up the rest of
    // the field's 12pt by 10pt.
    #if os(iOS)
        private static let editorPadding = EdgeInsets(top: 2, leading: 7, bottom: 2, trailing: 7)
    #else
        private static let editorPadding = EdgeInsets(top: 10, leading: 7, bottom: 10, trailing: 7)
    #endif

    private func performSubmit() {
        let draft = trimmedMessage
        let route = selectedRoute
        isSubmitting = true
        Task {
            let didClaimCommand = await submit(draft, route)
            isSubmitting = false
            if didClaimCommand {
                dismiss()
            }
        }
    }
}
