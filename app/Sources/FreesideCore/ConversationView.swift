import CoreGraphics
import FreesideAPI
import FreesideModel
import SwiftUI
import UniformTypeIdentifiers

#if os(iOS)
    import PhotosUI
    import UIKit
#elseif os(macOS)
    import AppKit
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
        case .unknown: "Unknown"
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
    /// A comment the operator left on an earlier specification revision and
    /// what the agent claims it did about it. The pair is part of deciding
    /// on the revision, so it reads as conversation (frame 7.2) ahead of the
    /// live thread.
    struct PriorExchange: Equatable, Identifiable {
        let id: String
        let iteration: Int
        let comment: String
        /// The agent's addressal claim, absent when it claimed none.
        let response: String?

        var marker: String { "on version \(iteration)" }

        var accessibilityLabel: String {
            let answer = response.map { "Agent claims addressed: \($0)" } ?? "No addressal claimed"
            return "You, on version \(iteration): \(comment). \(answer)"
        }
    }

    /// The link under the thread that opens the reply composer.
    struct Reply {
        var isEnabled = true
        let open: () -> Void
    }

    let snapshot: Components.Schemas.ConversationSnapshot?
    /// Whether the thread opens with its `Conversation` keyword. Off where
    /// a fold's label already names it.
    var showsKeyword = true
    var priorExchanges: [PriorExchange] = []
    var reply: Reply? = nil
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
        snapshot: Components.Schemas.ConversationSnapshot?,
        showsKeyword: Bool = true,
        priorExchanges: [PriorExchange] = [],
        reply: Reply? = nil,
        attachments: AttachmentLoader,
        loadsAttachments: Bool,
        now: Date = .now,
        rendersInteractiveControls: Bool = true,
        initiallyExpandedMessageIDs: Set<String> = []
    ) {
        self.snapshot = snapshot
        self.showsKeyword = showsKeyword
        self.priorExchanges = priorExchanges
        self.reply = reply
        self.attachments = attachments
        self.loadsAttachments = loadsAttachments
        self.now = now
        self.rendersInteractiveControls = rendersInteractiveControls
        _expandedMessageIDs = State(initialValue: initiallyExpandedMessageIDs)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if showsKeyword {
                KeywordLabel(text: "Conversation")
                    .accessibilityAddTraits(.isHeader)
            }

            ForEach(priorExchanges) { exchange in
                priorExchange(exchange)
            }

            let messages = snapshot?.conversation.messages ?? []
            ForEach(messages.sorted(by: { $0.sequence < $1.sequence }), id: \.id) {
                message in
                MessageRowLayout(alignsTrailing: message.author == .user) {
                    exactTime(message, on: messageShape(message))
                }
            }

            if snapshot?.conversation.status == .awaiting_agent {
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

            if let reply {
                replyLink(reply)
            }
        }
        // The thread keeps the prose measure, so a message's share of it
        // stays a line of reading length however wide the column grows.
        .proseMeasure()
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// The comment on the right as the operator's own message, the agent's
    /// addressal quoted on the left under a compact mark. The quote surface
    /// is what says the addressal is the agent's claim, as it does for every
    /// agent message in the thread.
    @ViewBuilder
    private func priorExchange(_ exchange: PriorExchange) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(exchange.marker)
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
            MessageRowLayout(alignsTrailing: true) {
                Text(exchange.comment)
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.ink)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, 14)
                    .padding(.vertical, 10)
                    .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
            }
            if let response = exchange.response {
                MessageRowLayout(alignsTrailing: false) {
                    VStack(alignment: .leading, spacing: 6) {
                        KeywordLabel(text: "Addressed", color: .accentText)
                        Text(response)
                            .font(FreesideFont.message)
                            .foregroundStyle(Color.ink)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 10)
                    .quoteSurface(cornerRadius: 8)
                }
            } else {
                Text("No addressal claimed")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(Text(exchange.accessibilityLabel))
    }

    /// A real `Button`, so the keyboard and VoiceOver reach the composer
    /// from the thread as they do from the action.
    @ViewBuilder
    private func replyLink(_ reply: Reply) -> some View {
        let label = Text("Reply\u{2026}")
            .font(FreesideFont.actionLabel)
            .foregroundStyle(Color.accentText)
        if rendersInteractiveControls {
            Button(action: reply.open) {
                label
                    .frame(minHeight: 28)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .disabled(!reply.isEnabled)
            .accessibilityLabel("Reply to the agent")
        } else {
            label
        }
    }

    @ViewBuilder
    private func messageShape(_ message: Components.Schemas.Message) -> some View {
        switch message.author {
        case .agent, .unknown:
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
            .accessibilityElement(children: .ignore)
            .accessibilityAddTraits(.isStaticText)
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
            content.copyMenu {
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
    /// The files the message carries, for a composer that attaches them:
    /// a spec approval's Discuss (#2995), the one discussion that hands
    /// files to an agent. `nil` for every other use, which draws no attach
    /// control. The presenter owns the draft and sends it with the message.
    var attachments: Binding<TaskAttachmentDraft>? = nil
    let submit: (String, Components.Schemas.AnswerRoute?) async -> Bool
    @State private var isSubmitting = false
    @State private var chosenRoute: Components.Schemas.AnswerRoute?
    @FocusState private var fieldIsFocused: Bool
    /// The attachments' decoded image thumbnails, by digest.
    @State private var thumbnails: [String: CGImage] = [:]
    /// Why the latest items offered weren't attached, one sentence each.
    @State private var refusals: [String] = []
    /// Offered items still being read; Send waits for them, so none is
    /// dropped from the message without a word.
    @State private var loadsInFlight = 0
    /// Cleared when the sheet goes away, so a read that finishes after a
    /// Cancel or a send never adds to the presenter's next draft.
    @State private var presentation = Presentation()
    @State private var showsFileImporter = false
    #if os(iOS)
        @State private var showsPhotoPicker = false
        @State private var photoSelection: [PhotosPickerItem] = []
    #else
        /// The Mac field that attaches pasted files draws its own focus
        /// ring from this, since it is an AppKit view.
        @State private var editorIsFocused = false
        @State private var browse = TaskPromptBrowseState()
    #endif

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
                isSubmitEnabled: canSubmit && !isSubmitting && loadsInFlight == 0,
                submit: performSubmit,
                cancel: { dismiss() })
        }
        .background(Color.ground2)
        .modifier(AttachmentPickers(sheet: self))
        .onDisappear { presentation.isOpen = false }
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
            if rendersInteractiveControls, attachments != nil {
                attachingEditor
            } else if rendersInteractiveControls {
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
            if let attachments {
                attachmentRow(attachments.wrappedValue)
                    .padding(.top, 4)
                    .disabled(isSubmitting)
            }
        }
    }

    /// The field of a composer that attaches files. On the Mac it is the
    /// New Task composer's text view, which takes a pasted or dropped file
    /// or image as an attachment where SwiftUI's editor would insert its
    /// path. iPhone's editor never pastes a file, so it keeps the SwiftUI
    /// one, and its Paste is in the Attach menu.
    @ViewBuilder private var attachingEditor: some View {
        #if os(macOS)
            TaskPromptEditor(
                text: $message, history: nil, browse: browse, fieldLabel: "Message", textFont: Self.fieldFont,
                onAttach: attach(pasted:), isFocused: $editorIsFocused
            )
            .padding(Self.editorPadding)
            .frame(minHeight: Self.fieldMinHeight)
            .background(fieldFrame(isFocused: editorIsFocused))
            .overlay(alignment: .topLeading) {
                if message.isEmpty {
                    placeholder
                        .allowsHitTesting(false)
                        .accessibilityHidden(true)
                }
            }
        #else
            TextEditor(text: $message)
                .font(FreesideFont.callout)
                .foregroundStyle(Color.ink)
                .scrollContentBackground(.hidden)
                .focused($fieldIsFocused)
                .padding(Self.editorPadding)
                .frame(minHeight: Self.fieldMinHeight)
                .background(fieldFrame(isFocused: fieldIsFocused))
                .overlay(alignment: .topLeading) {
                    if message.isEmpty {
                        placeholder
                            .allowsHitTesting(false)
                            .accessibilityHidden(true)
                    }
                }
                .accessibilityLabel("Message")
        #endif
    }

    #if os(macOS)
        /// The callout face the SwiftUI field draws in, for the AppKit one.
        private static var fieldFont: NSFont {
            _ = FreesideFont.registration
            let size = FreesideFont.size(of: .callout)
            return NSFont(name: "IBMPlexSans", size: size) ?? .systemFont(ofSize: size)
        }
    #endif

    // MARK: Attachments

    /// Each attachment as a removable chip, in the order added, then the
    /// Attach control, then why the latest items weren't attached.
    private func attachmentRow(_ draft: TaskAttachmentDraft) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                ForEach(draft.items) { item in
                    ComposerAttachmentChip(attachment: item, thumbnail: thumbnail(for: item)) {
                        remove(item)
                    }
                }
                attachControl
            }
            if !refusals.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(Array(refusals.enumerated()), id: \.offset) { _, refusal in
                        Text(refusal)
                            .font(FreesideFont.cardBody)
                            .foregroundStyle(Color.waxText)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .accessibilityElement(children: .combine)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// An image's thumbnail, decoded when it was attached. A static render
    /// (the goldens) never attached it, so it decodes the draft's own.
    private func thumbnail(for item: DraftAttachment) -> CGImage? {
        guard item.isImage else { return nil }
        if let decoded = thumbnails[item.digest] { return decoded }
        return rendersInteractiveControls ? nil : ComposerAttachmentIntake.thumbnail(item.bytes)
    }

    /// The New Task composer's Attach chip: on iPhone a menu of Photos,
    /// Files, and Paste; on the Mac a file picker, beside a drop on the
    /// sheet and a paste into the field.
    @ViewBuilder private var attachControl: some View {
        let chip = ComposerChip(glyph: "+", text: "Attach")
        if rendersInteractiveControls {
            #if os(iOS)
                Menu {
                    Button("Photos", systemImage: "photo.on.rectangle") { showsPhotoPicker = true }
                    Button("Files", systemImage: "folder") { showsFileImporter = true }
                    PasteButton(supportedContentTypes: [.image, .fileURL]) { providers in
                        let held = attachments?.wrappedValue.usage ?? TaskAttachmentDraft(for: .message).usage
                        attach {
                            await ComposerAttachmentIntake.items(
                                from: providers, origin: .pasted, repository: nil, held: held, takesFolders: false)
                        }
                    }
                } label: {
                    chip
                }
                .menuStyle(.button)
                .buttonStyle(.plain)
                .menuIndicator(.hidden)
                .accessibilityLabel("Attach")
            #else
                Button {
                    showsFileImporter = true
                } label: {
                    chip
                }
                .buttonStyle(.plain)
                .help("Attach files or images. You can also drop or paste them.")
                .accessibilityLabel("Attach files or images")
            #endif
        } else {
            chip
        }
    }

    /// Reads what `load` returns, off the main actor where it reads files,
    /// and adds each item in order. The batch's refusals replace the last.
    private func attach(_ load: @escaping @Sendable () async -> [ComposerAttachmentIntake.Item]) {
        guard let attachments, !isSubmitting else { return }
        let presentation = presentation
        loadsInFlight += 1
        Task {
            let items = await load()
            loadsInFlight -= 1
            guard presentation.isOpen else { return }
            var batchRefusals: [String] = []
            for item in items {
                switch item {
                case .refused(let sentence):
                    batchRefusals.append(sentence)
                case .attachment(let name, let claim, let bytes):
                    switch attachments.wrappedValue.add(name: name, claimedMediaType: claim, bytes: bytes) {
                    case .added(let added):
                        if added.isImage, thumbnails[added.digest] == nil {
                            thumbnails[added.digest] = ComposerAttachmentIntake.thumbnail(bytes)
                        }
                    case .refused(let sentence):
                        batchRefusals.append(sentence)
                    }
                // The intake refuses folders for this composer, so neither
                // arrives; refuse them the same way should one.
                case .folder(let name, _, _):
                    batchRefusals.append(ComposerAttachmentIntake.noFoldersSentence(name))
                case .repositoryPath(let path):
                    batchRefusals.append(ComposerAttachmentIntake.noFoldersSentence(path))
                }
            }
            refusals = batchRefusals
        }
    }

    private func attach(files urls: [URL]) {
        let held = attachments?.wrappedValue.usage ?? TaskAttachmentDraft(for: .message).usage
        attach {
            await Task.detached {
                ComposerAttachmentIntake.files(at: urls, repository: nil, held: held, takesFolders: false)
            }.value
        }
    }

    private func attach(pasted: ComposerAttachmentIntake.Pasted) {
        switch pasted {
        case .files(let urls):
            attach(files: urls)
        case .image(let data):
            attach { [ComposerAttachmentIntake.image(data, origin: .pasted)] }
        }
    }

    private func remove(_ item: DraftAttachment) {
        guard let attachments else { return }
        attachments.wrappedValue.remove(name: item.name)
        thumbnails[item.digest] = nil
    }

    /// Whether this presentation of the sheet is still on screen. A class,
    /// so a read that outlives the sheet sees the change.
    @MainActor private final class Presentation {
        var isOpen = true
    }

    /// The pickers the Attach control opens and the sheet's drop target,
    /// kept off the static goldens and off every composer that attaches
    /// nothing.
    private struct AttachmentPickers: ViewModifier {
        let sheet: MessageComposerSheet

        func body(content: Content) -> some View {
            if sheet.rendersInteractiveControls, let attachments = sheet.attachments {
                content
                    // A file or image dropped anywhere on the sheet
                    // attaches; the Mac text view routes its own drops the
                    // same way.
                    .onDrop(of: [.fileURL, .image], isTargeted: nil) { providers in
                        guard !sheet.isSubmitting else { return false }
                        let held = attachments.wrappedValue.usage
                        sheet.attach {
                            await ComposerAttachmentIntake.items(
                                from: providers, origin: .pasted, repository: nil, held: held, takesFolders: false)
                        }
                        return true
                    }
                    // `.data` leaves out folders and packages.
                    .fileImporter(
                        isPresented: sheet.$showsFileImporter, allowedContentTypes: [.data],
                        allowsMultipleSelection: true
                    ) { result in
                        if case .success(let urls) = result { sheet.attach(files: urls) }
                    }
                    #if os(iOS)
                        .photosPicker(
                            isPresented: sheet.$showsPhotoPicker, selection: sheet.$photoSelection,
                            matching: .images
                        )
                        .onChange(of: sheet.photoSelection) {
                            let picked = sheet.photoSelection
                            guard !picked.isEmpty else { return }
                            sheet.photoSelection = []
                            sheet.attach {
                                var items: [ComposerAttachmentIntake.Item] = []
                                for item in picked {
                                    if let data = try? await item.loadTransferable(type: Data.self) {
                                        items.append(ComposerAttachmentIntake.image(data, origin: .photo))
                                    } else {
                                        items.append(.refused("A photo couldn't be loaded."))
                                    }
                                }
                                return items
                            }
                        }
                    #endif
            } else {
                content
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
