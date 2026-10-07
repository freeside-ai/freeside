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

struct MessageComposerSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var message = ""
    let title: String
    let prompt: String
    let submitLabel: String
    var byteLimit: Int?
    var rendersInteractiveControls = true
    /// The answer routes this composer lets the operator choose between, in
    /// display order; empty for a composer that carries no route (#1083). The
    /// first is the default selection.
    var routeOptions: [Components.Schemas.AnswerRoute] = []
    let submit: (String, Components.Schemas.AnswerRoute?) async -> Bool
    @State private var isSubmitting = false
    @State private var chosenRoute: Components.Schemas.AnswerRoute?

    private var selectedRoute: Components.Schemas.AnswerRoute? {
        chosenRoute ?? routeOptions.first
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
            FreesideSheetHeader(ask: title, consequence: prompt)
            VStack(alignment: .leading, spacing: 12) {
                if rendersInteractiveControls {
                    TextEditor(text: $message)
                        .font(FreesideFont.callout)
                        .scrollContentBackground(.hidden)
                        .padding(8)
                        .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                        .accessibilityLabel("Message")
                } else {
                    Text("Message")
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.inkDim)
                        .padding(8)
                        .frame(maxWidth: .infinity, minHeight: 190, alignment: .topLeading)
                        .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                }
                if let byteLimit {
                    Text("\(byteCount) of \(byteLimit) bytes")
                        .font(FreesideFont.caption)
                        .foregroundStyle(byteCount > byteLimit ? Color.waxText : Color.inkDim)
                        .frame(maxWidth: .infinity, alignment: .trailing)
                }
                if routeOptions.count > 1, let defaultRoute = routeOptions.first {
                    FreesideSegmentedControl(
                        accessibilityLabel: "What to do with the answer",
                        segments: routeOptions.map {
                            .init(value: $0, label: AgentQuestionPresentation.answerRouteLabel($0))
                        },
                        selection: Binding(
                            get: { selectedRoute ?? defaultRoute },
                            set: { chosenRoute = $0 }))
                }
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
            .frame(maxWidth: .infinity, maxHeight: .infinity)

            // The submit lives in the sheet body rather than a toolbar so it
            // carries the design language's primary recipe; the row keeps
            // the Return and Escape bindings the placements supplied.
            FreesideSheetActionRow(
                submitLabel: submitLabel,
                isSubmitEnabled: canSubmit && !isSubmitting,
                submit: performSubmit,
                cancel: { dismiss() })
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 300)
    }

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
