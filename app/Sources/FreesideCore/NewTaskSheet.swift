import FreesideAPI
import SwiftUI
import UniformTypeIdentifiers

#if os(iOS)
    import PhotosUI
#endif

/// The New Task composer (plan §5.11): an operator starts work from the app
/// instead of the daemon host's CLI. Drawn as the 4 Nov 2026 handoff's
/// canvas band 3 (`docs/design/handoff-2026-11-04-settings-usage-new-task/`):
/// a serif text area whose placeholder is the ask, over a foot holding the
/// project chip and the Agents chip (R33). Open, the Agents chip shows one
/// role chip per task-line role above the foot, each a menu over the agent
/// catalog. The daemon's task namer names the task, so the composer asks
/// for no name. On the Mac, Return submits, Shift-Return starts a line, and
/// Escape cancels; on iPhone, Cancel and Submit sit in the sheet's bar.
///
/// The Attach chip adds files and images (#2590): on the Mac through a
/// file picker, a drop on the composer, or a paste into the text area; on
/// iPhone through a menu of Photos, Files, and Paste. Each shows as a chip
/// above the foot with its size and a remove control. Whatever the daemon's
/// limits or name rules would refuse is refused before upload, with the
/// reason under the composer. The text stays required: attachments alone
/// can't submit.
///
/// On the Mac the same routes take a folder (#2591). A folder outside the
/// project's repository attaches as one chip holding all its files; a
/// folder inside it adds its repository path to the text instead, since the
/// agent already has the checkout.
///
/// The draft lives in the coordinator's `NewTaskDraftStore`, not this
/// sheet (#2935): closing the sheet by any route keeps it, and every route
/// that opens New Task restores it. Submit takes the draft out of the store
/// while the request is in flight, so a composer reopened meanwhile can't
/// send it again. A created task or a lost response, whose request may have
/// committed and is saved in Unconfirmed submissions, leaves it out; the
/// frozen sheet keeps showing what it sent. A daemon rejection puts it back
/// and shows the reason under the composer. Discard clears it at once. The sheet expects a sentence or a
/// paragraph, not a document; the follow-up conversation happens on the
/// task's cards.
struct NewTaskSheet: View {
    typealias Role = AgentChoiceDisplay.Role

    @Environment(\.dismiss) private var dismiss
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize
    /// The projects the picker offers: the sorted unique ids of the synced
    /// tasks (`TaskDisplay.knownProjects`), the same set the Tasks filter uses.
    let projects: [String]
    /// Each project's `owner/repo`, where the client knows it
    /// (`TaskDisplay.projectRepositories`), to tell a dropped folder inside
    /// the project's repository.
    let projectRepositories: [String: String]
    /// Sheet-owned so the idempotency key survives a parent recompose. The
    /// presenter builds this inside the `.sheet` content closure, which re-runs
    /// on every observed `coordinator` change (an iOS heartbeat or a foreground
    /// refresh); holding the passed model as plain state would let such a
    /// recompose swap in a fresh model and discard `lastCommand` and the
    /// `.lost` state. `@State` keeps the first model for the
    /// presentation's lifetime; a re-presentation gets a fresh one.
    @State private var model: TaskSubmissionModel
    /// Called with the created task's id after a successful submit, so the
    /// host routes to its detail.
    let onSubmitted: (String) -> Void
    /// False for the screenshot goldens: `ImageRenderer` cannot draw a
    /// `TextEditor`, a `Menu`, or a navigation bar, so those render as static
    /// stand-ins that still show the draft and the chips.
    var rendersInteractiveControls = true

    /// The draft: text, project, agent choices, the Agents chip's state,
    /// and attachments. A chosen agent the newer catalog drops stays chosen
    /// and blocks Submit until changed; a role left out goes to the daemon.
    let draftStore: NewTaskDraftStore
    /// What Submit took out of the store, shown while the request is in
    /// flight and while the sheet stays frozen on a lost response.
    @State private var sentDraft: NewTaskDraft?
    @State private var promptBrowse = TaskPromptBrowseState()
    /// Each image attachment's chip thumbnail, by digest, decoded once when
    /// it is added.
    @State private var thumbnails: [String: CGImage]
    /// Why the latest items the operator added were refused; the next add
    /// replaces them.
    @State private var refusals: [String]
    @State private var showsFileImporter = false
    #if os(iOS)
        @State private var showsPhotoPicker = false
        @State private var photoSelection: [PhotosPickerItem] = []
    #endif
    /// The Mac text view's laid-out height, which the text area grows to.
    @State private var textHeight: CGFloat = 0
    @State private var isSubmitting = false
    /// The in-flight submit, held so dismissing the sheet cancels it
    /// and suppresses the completion's navigation (the submission itself may
    /// still land server-side, which the idempotency key reconciles).
    @State private var submitTask: Task<Void, Never>?

    init(
        projects: [String],
        projectRepositories: [String: String] = [:],
        model: TaskSubmissionModel,
        draftStore: NewTaskDraftStore,
        onSubmitted: @escaping (String) -> Void,
        rendersInteractiveControls: Bool = true,
        initialRefusals: [String] = []
    ) {
        self.projects = projects
        self.projectRepositories = projectRepositories
        _model = State(initialValue: model)
        self.draftStore = draftStore
        self.onSubmitted = onSubmitted
        self.rendersInteractiveControls = rendersInteractiveControls
        _thumbnails = State(
            initialValue: draftStore.draft.attachments.items.reduce(into: [:]) { thumbnails, item in
                if item.isImage { thumbnails[item.digest] = ComposerAttachmentIntake.thumbnail(item.bytes) }
            })
        _refusals = State(initialValue: initialRefusals)
    }

    static let sourcePlaceholder = "What should the agent work on?"

    /// The draft the sheet draws: the store's, or what a settled submit sent.
    private var shown: NewTaskDraft { sentDraft ?? draftStore.draft }
    private var source: String { shown.source }
    private var choices: [Role: AgentChoiceDisplay.Selection] { shown.choices }
    private var agentsOpen: Bool { shown.agentsOpen }
    private var attachments: TaskAttachmentDraft { shown.attachments }
    /// The draft's project when the synced projects still hold it, else the
    /// first of them, as a fresh composer starts.
    private var projectID: String { Self.resolvedProject(shown.projectID, in: projects) }

    static func resolvedProject(_ stored: String?, in projects: [String]) -> String {
        if let stored, projects.contains(stored) { return stored }
        return projects.first ?? ""
    }

    private var sourceBinding: Binding<String> {
        Binding(get: { shown.source }, set: { draftStore.draft.source = $0 })
    }

    private var projectBinding: Binding<String> {
        Binding(get: { projectID }, set: { draftStore.draft.projectID = $0 })
    }

    /// Whether a submit's result ends the draft: a created task, or a lost
    /// response whose request may have committed and is saved for Retry, so
    /// a restored draft must not offer to send it again. A rejection
    /// recorded nothing, and an idle result was canceled before sending;
    /// both keep the draft for another try.
    static func settles(_ state: TaskSubmissionModel.SubmissionState, taskID: String?) -> Bool {
        if taskID != nil { return true }
        switch state {
        case .lost, .submitted: return true
        case .idle, .submitting, .rejected: return false
        }
    }

    /// Takes the draft out of the store as Submit sends it, so nothing can
    /// send it twice while the request is in flight.
    static func detach(_ draftStore: NewTaskDraftStore) -> NewTaskDraft {
        let sent = draftStore.draft
        draftStore.clear()
        return sent
    }

    /// Puts a sent draft back when the submit's result doesn't settle it,
    /// and says whether it settled.
    @discardableResult
    static func settle(
        _ draftStore: NewTaskDraftStore, sent: NewTaskDraft, after state: TaskSubmissionModel.SubmissionState,
        taskID: String?
    ) -> Bool {
        guard settles(state, taskID: taskID) else {
            draftStore.restore(sent)
            return false
        }
        return true
    }

    /// Closing mid-browse keeps the operator's own text, not the recalled
    /// prompt the editor shows, unless a submit already took the draft.
    static func close(_ draftStore: NewTaskDraftStore, browse: TaskPromptBrowseState, submitted: Bool) {
        guard let typed = browse.end(), !submitted else { return }
        draftStore.draft.source = typed
    }

    /// Discard offers itself only while there is a draft to lose.
    static func canDiscard(_ draft: NewTaskDraft, isFrozen: Bool) -> Bool {
        !draft.isEmpty && !isFrozen
    }

    static let droppedAttachmentsSentence = "Attachments didn't survive the relaunch. Add them again."

    /// What the text area draws: the muted placeholder while the draft holds
    /// no characters, otherwise the draft exactly as typed. Whitespace is
    /// typed text, so it hides the placeholder; `submission` is what trims
    /// it away. Typed text that happens to read like the placeholder is
    /// still a value (visual audit D05: an empty field looks empty, and a
    /// placeholder is never content).
    enum FieldDisplay: Equatable {
        case placeholder(String)
        case value(String)
    }

    static func fieldDisplay(_ draft: String, placeholder: String) -> FieldDisplay {
        draft.isEmpty ? .placeholder(placeholder) : .value(draft)
    }

    /// What Submit sends for a draft: the trimmed source and nothing else.
    struct Submission: Equatable {
        let source: String
    }

    static func submission(source: String) -> Submission {
        Submission(source: source.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// Sends a draft with the per-role lines, the attachments, and no name:
    /// the daemon's task namer names the task.
    static func send(
        _ submission: Submission, projectID: String, choices: [Role: AgentChoiceDisplay.Selection],
        attachments: TaskAttachmentDraft = TaskAttachmentDraft(), through model: TaskSubmissionModel
    ) async -> String? {
        await model.submit(
            projectID: projectID, source: submission.source,
            taskLines: AgentChoiceDisplay.taskLines(choices: choices), attachments: attachments)
    }

    /// The Mac text area's ceiling: 60% of a 600pt sheet. A macOS sheet
    /// sizes to its content, so it has no height of its own to take 60% of.
    static let macTextCap: CGFloat = 0.6 * 600
    /// The iPhone text area's share of the sheet's measured height.
    static let phoneTextShare: CGFloat = 0.6

    /// A lost response may have committed a task server-side, so the sheet
    /// cannot submit the same draft again. Recovery belongs to the separate
    /// Unconfirmed submissions screen; reopening New Task starts new work.
    private var isLost: Bool {
        if case .lost = model.state { return true }
        return false
    }

    /// An uncertain or in-flight submission freezes the composer.
    private var isFrozen: Bool { isLost || isSubmitting }

    private var canSubmit: Bool {
        !Self.submission(source: source).source.isEmpty && !projectID.isEmpty && projects.contains(projectID)
            && !isSubmitting && !isLost
            && TaskSubmissionModel.canCompose(freshness: model.freshness)
            && missingChoices.isEmpty
    }

    private var missingChoices: [AgentChoiceDisplay.MissingChoice] {
        AgentChoiceDisplay.missingChoices(choices: choices, catalog: model.agentCatalog)
    }

    var body: some View {
        sheet
            .freesideSheetPresentation()
            // Canceling, Escape, or an interactive dismiss ends the
            // presentation while a submit is still in flight; cancel the task
            // so its completion does not route the operator to a task they
            // navigated away from.
            .onDisappear {
                submitTask?.cancel()
                Self.close(draftStore, browse: promptBrowse, submitted: sentDraft != nil)
            }
            // Pin the project the composer shows, so a later sync can't
            // move a draft whose stored project is gone or was never chosen.
            .onAppear {
                if draftStore.draft.projectID != projectID, !projectID.isEmpty {
                    draftStore.draft.projectID = projectID
                }
            }
            .onChange(of: projectID) {
                if let draft = promptBrowse.end() { draftStore.draft.source = draft }
            }
            .modifier(AttachmentPickers(sheet: self))
    }

    @ViewBuilder private var sheet: some View {
        if rendersInteractiveControls {
            #if os(iOS)
                // ◆3 (4 Nov 2026): the one sheet with a navigation bar, the
                // iOS form idiom for Cancel and Submit.
                NavigationStack {
                    GeometryReader { proxy in
                        ScrollView {
                            composer(textCap: Self.phoneTextShare * proxy.size.height)
                                .padding(.horizontal, 12)
                                .padding(.vertical, 8)
                        }
                    }
                    .background(Color.ground)
                    .navigationTitle("New Task")
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar {
                        ToolbarItem(placement: .cancellationAction) {
                            Button("Cancel") { dismiss() }
                        }
                        ToolbarItem(placement: .topBarLeading) {
                            Button("Discard", role: .destructive) { discard() }
                                .disabled(!Self.canDiscard(draftStore.draft, isFrozen: isFrozen))
                        }
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Submit") { performSubmit() }
                                .fontWeight(.semibold)
                                .disabled(!canSubmit)
                        }
                    }
                }
            #else
                composer(textCap: Self.macTextCap)
                    .padding(12)
                    .frame(minWidth: 380, idealWidth: 560, alignment: .top)
                    .background(Color.ground)
                    // No button draws: Return reaches the text view, which
                    // submits, so nothing in the sheet may bind the default
                    // action. Escape cancels through a button that exists
                    // for its shortcut alone.
                    .background(
                        Button("", action: { dismiss() })
                            .buttonStyle(.plain)
                            .keyboardShortcut(.cancelAction)
                            .frame(width: 0, height: 0)
                            .opacity(0)
                            .accessibilityHidden(true)
                    )
            #endif
        } else {
            composer(textCap: Self.macTextCap)
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .top)
                .background(Color.ground)
        }
    }

    /// The composer box, then the lost or rejected notice under it.
    private func composer(textCap: CGFloat) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            VStack(alignment: .leading, spacing: 0) {
                // Freeze an uncertain submission so editing cannot imply
                // that another Submit would retry the saved request.
                Group {
                    textArea(cap: textCap)
                    if !attachments.isEmpty { attachmentChips }
                    if agentsOpen { roleChips }
                    missingAgentNotice
                    refusalNotice
                    foot
                }
                .disabled(isFrozen)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.ground2, in: RoundedRectangle(cornerRadius: 12))
            .clipShape(RoundedRectangle(cornerRadius: 12))
            .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(Color.itemBorder))
            // A file or image dropped anywhere on the composer attaches;
            // the Mac text view routes its own drops the same way.
            .onDrop(of: [.fileURL, .image], isTargeted: nil) { providers in
                guard rendersInteractiveControls, !isFrozen else { return false }
                let (repository, held) = (projectRepositories[projectID], attachments.usage)
                attach {
                    await ComposerAttachmentIntake.items(
                        from: providers, origin: .pasted, repository: repository, held: held)
                }
                return true
            }
            status
            draftNotices
        }
    }

    // MARK: Text area

    /// Four lines of the statement face: the text area's floor.
    private static var fourLines: some View {
        Text("A\nA\nA\nA").font(FreesideFont.statement)
    }

    @ViewBuilder private func textArea(cap: CGFloat) -> some View {
        if rendersInteractiveControls {
            sourceEditor(cap: cap)
                // A text editor has no prompt of its own. This one is drawn
                // over the empty editor, never written into `source`, and
                // leaves clicks and VoiceOver to the editor beneath it.
                .overlay(alignment: .topLeading) {
                    if case .placeholder(let placeholder) = Self.fieldDisplay(
                        source, placeholder: Self.sourcePlaceholder)
                    {
                        Text(placeholder)
                            .font(FreesideFont.statement)
                            .foregroundStyle(Color.inkDim)
                            .padding(Self.editorTextInsets)
                            .allowsHitTesting(false)
                            .accessibilityHidden(true)
                    }
                }
                .padding(.vertical, 14 - Self.editorTextInsets.top)
                .padding(.horizontal, 16 - Self.editorTextInsets.leading)
        } else {
            // The goldens' stand-in: the draft at its full height, clipped at
            // the cap as the editor would scroll it.
            ZStack(alignment: .topLeading) {
                Self.fourLines.hidden()
                staticFieldText(source, placeholder: Self.sourcePlaceholder)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .frame(maxWidth: .infinity, maxHeight: cap, alignment: .topLeading)
            .clipped()
            .padding(.vertical, 14)
            .padding(.horizontal, 16)
        }
    }

    /// A static field's text, for the goldens: placeholder or draft.
    private func staticFieldText(_ draft: String, placeholder: String) -> some View {
        let display = Self.fieldDisplay(draft, placeholder: placeholder)
        return Group {
            switch display {
            case .placeholder(let text): Text(text).foregroundStyle(Color.inkDim)
            case .value(let text): Text(text).foregroundStyle(Color.ink)
            }
        }
        .font(FreesideFont.statement)
    }

    // Where each platform's editor draws its first character inside its own
    // frame: the line fragment's 5pt padding, and UITextView's 8pt above
    // the text.
    #if os(iOS)
        private static let editorTextInsets = EdgeInsets(top: 8, leading: 5, bottom: 8, trailing: 5)
    #else
        private static let editorTextInsets = EdgeInsets(top: 0, leading: 5, bottom: 0, trailing: 5)
    #endif

    @ViewBuilder private func sourceEditor(cap: CGFloat) -> some View {
        #if os(macOS)
            TaskPromptEditor(
                text: sourceBinding, history: model.promptHistory.prompts(for: projectID), browse: promptBrowse,
                onSubmit: performSubmit, contentHeight: $textHeight, onAttach: attach(pasted:)
            )
            .frame(height: min(max(textHeight, TaskPromptEditor.height(lines: 4)), cap))
        #else
            // A hidden copy of the draft sizes the editor: four lines at
            // least, the draft's height as it grows, the cap at most, past
            // which the editor scrolls inside.
            ZStack(alignment: .topLeading) {
                Self.fourLines
                // A trailing newline's empty line takes no height in a Text.
                Text(source.hasSuffix("\n") ? source + " " : source)
                    .font(FreesideFont.statement)
            }
            .padding(Self.editorTextInsets)
            .hidden()
            .frame(maxWidth: .infinity, maxHeight: cap, alignment: .topLeading)
            .overlay {
                TextEditor(text: sourceBinding)
                    .font(FreesideFont.statement)
                    .scrollContentBackground(.hidden)
                    .accessibilityLabel("Work to do")
                    .accessibilityAction(named: "Submit") { performSubmit() }
            }
        #endif
    }

    // MARK: Agents

    static let noTreeText = "The daemon has no agent tree, so it picks every agent."

    /// One chip per task-line role, in role order, each a menu over the
    /// catalog.
    private var roleChips: some View {
        WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
            ForEach(AgentChoiceDisplay.roleOrder, id: \.self) { role in
                roleChip(role)
            }
        }
        .padding(.horizontal, 10)
        .padding(.bottom, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder private func roleChip(_ role: Role) -> some View {
        let title = AgentChoiceDisplay.title(for: role)
        let catalog = model.agentCatalog
        let chosen = choices[role]
        let chosenLabel = chosen.map { AgentChoiceDisplay.choiceLabel($0, roles: [role], catalog: catalog) }
        let chip = RoleChip(
            role: title, value: chosenLabel,
            defaultValue: AgentChoiceDisplay.defaultSettingsLabel(
                role: role, policy: model.routingPolicy, catalog: catalog))
        // Only the chip is Freeside's; the menu stays system chrome, which
        // `ImageRenderer` cannot draw, so the goldens show the chip.
        if rendersInteractiveControls {
            let defaultLabel = AgentChoiceDisplay.defaultLabel(
                role: role, policy: model.routingPolicy, catalog: catalog)
            Menu {
                RoleSelectionMenu(
                    offer: Self.roleMenuOffer(role, choices: choices, model: model), lineupLabel: defaultLabel
                ) { draftStore.draft.choices[role] = $0 }
            } label: {
                chip
            }
            .menuStyle(.button)
            .buttonStyle(.plain)
            .menuIndicator(.hidden)
            .accessibilityLabel("\(title) agent")
            .accessibilityValue(chosenLabel ?? defaultLabel)
        } else {
            chip
        }
    }

    /// What a role chip's menu offers: the synced catalog and usage pools,
    /// with the role's choice chosen.
    static func roleMenuOffer(
        _ role: Role, choices: [Role: AgentChoiceDisplay.Selection], model: TaskSubmissionModel
    ) -> RoleSelectionMenu.Offer {
        .init(role: role, catalog: model.agentCatalog, pools: model.usagePools, current: choices[role])
    }

    /// Why a stale choice blocks Submit, one sentence per selection the
    /// catalog no longer offers, naming the part it dropped. It draws
    /// whether or not the role chips show, so Submit is never blocked
    /// without a reason on screen.
    @ViewBuilder private var missingAgentNotice: some View {
        if !missingChoices.isEmpty {
            VStack(alignment: .leading, spacing: 4) {
                ForEach(missingChoices, id: \.selection) { missing in
                    Text(missing.sentence)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.waxText)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .padding(.horizontal, 12)
            .padding(.bottom, 8)
        }
    }

    // MARK: Attachments

    /// One chip per file and one per folder, in the order added.
    private var attachmentChips: some View {
        WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
            ForEach(attachments.chips) { chip in
                switch chip {
                case .file(_, let item):
                    ComposerAttachmentChip(attachment: item, thumbnail: thumbnails[item.digest]) {
                        remove(item)
                    }
                case .folder(let name, let items):
                    ComposerFolderChip(
                        name: name, fileCount: items.count, totalBytes: items.reduce(0) { $0 + $1.sizeBytes }
                    ) {
                        removeFolder(name)
                    }
                }
            }
        }
        .padding(.horizontal, 10)
        .padding(.bottom, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// Why the latest items weren't attached, one sentence each, in wax
    /// like the missing-agent notice.
    @ViewBuilder private var refusalNotice: some View {
        if !refusals.isEmpty {
            VStack(alignment: .leading, spacing: 4) {
                ForEach(Array(refusals.enumerated()), id: \.offset) { _, refusal in
                    Text(refusal)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.waxText)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .padding(.horizontal, 12)
            .padding(.bottom, 8)
            .accessibilityElement(children: .combine)
        }
    }

    @ViewBuilder private var attachChip: some View {
        let chip = ComposerChip(glyph: "+", text: "Attach")
        if rendersInteractiveControls {
            #if os(iOS)
                Menu {
                    Button("Photos", systemImage: "photo.on.rectangle") { showsPhotoPicker = true }
                    Button("Files", systemImage: "folder") { showsFileImporter = true }
                    PasteButton(supportedContentTypes: [.image, .fileURL]) { providers in
                        let held = attachments.usage
                        attach {
                            await ComposerAttachmentIntake.items(
                                from: providers, origin: .pasted, repository: nil, held: held)
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
                .help("Attach files or folders. You can also drop or paste them.")
                .accessibilityLabel("Attach files or folders")
            #endif
        } else {
            chip
        }
    }

    /// Reads what `load` returns, off the main actor where it reads files,
    /// and adds each item in order. The batch's refusals replace the last.
    private func attach(_ load: @escaping @Sendable () async -> [ComposerAttachmentIntake.Item]) {
        guard !isFrozen else { return }
        Task {
            let items = await load()
            // A submit that started while the items loaded owns the draft now.
            guard !isFrozen else { return }
            var batchRefusals: [String] = []
            for item in items {
                switch item {
                case .refused(let sentence):
                    batchRefusals.append(sentence)
                case .attachment(let name, let claim, let bytes):
                    switch draftStore.draft.attachments.add(name: name, claimedMediaType: claim, bytes: bytes) {
                    case .added(let added):
                        if added.isImage, thumbnails[added.digest] == nil {
                            thumbnails[added.digest] = ComposerAttachmentIntake.thumbnail(bytes)
                        }
                    case .refused(let sentence):
                        batchRefusals.append(sentence)
                    }
                case .folder(let name, let files, let links):
                    switch draftStore.draft.attachments.addFolder(name: name, files: files) {
                    // A folder's chip draws no thumbnails, so none are
                    // decoded for its files.
                    case .added(let folder, _):
                        if let sentence = ComposerAttachmentIntake.skippedLinksSentence(folder: folder, links: links) {
                            batchRefusals.append(sentence)
                        }
                    case .refused(let sentence):
                        batchRefusals.append(sentence)
                    }
                case .repositoryPath(let path):
                    draftStore.draft.source = Self.appendingPath(path, to: draftStore.draft.source)
                }
            }
            refusals = batchRefusals
        }
    }

    /// The draft with a repository path added at its end, after a space
    /// when the draft isn't empty and doesn't already end in whitespace.
    static func appendingPath(_ path: String, to draft: String) -> String {
        guard let last = draft.last, !last.isWhitespace else { return draft + path }
        return draft + " " + path
    }

    /// Reads the files off the main actor, deciding a folder against the
    /// project and draft as they stand when the operator added it.
    private func attach(files urls: [URL]) {
        let (repository, held) = (projectRepositories[projectID], attachments.usage)
        attach {
            await Task.detached {
                ComposerAttachmentIntake.files(at: urls, repository: repository, held: held)
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
        draftStore.draft.attachments.remove(name: item.name)
        dropUnheldThumbnails()
    }

    private func removeFolder(_ name: String) {
        draftStore.draft.attachments.removeFolder(name: name)
        dropUnheldThumbnails()
    }

    /// Drops the thumbnails of digests no remaining item holds.
    private func dropUnheldThumbnails() {
        let held = Set(attachments.items.map(\.digest))
        thumbnails = thumbnails.filter { held.contains($0.key) }
    }

    /// The pickers the Attach chip opens, kept off the static goldens.
    private struct AttachmentPickers: ViewModifier {
        let sheet: NewTaskSheet

        #if os(macOS)
            static let pickableTypes: [UTType] = [.data, .folder]
        #else
            static let pickableTypes: [UTType] = [.data]
        #endif

        func body(content: Content) -> some View {
            if sheet.rendersInteractiveControls {
                content
                    // `.data` leaves out folders and packages. The Mac also
                    // takes folders (#2591); iPhone refuses a dropped or
                    // pasted folder by name.
                    .fileImporter(
                        isPresented: sheet.$showsFileImporter, allowedContentTypes: Self.pickableTypes,
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

    // MARK: Foot

    private var foot: some View {
        HStack(alignment: .top, spacing: 6) {
            WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                projectChip
                agentsChip
                attachChip
            }
            #if os(macOS)
                // The goldens leave it out, as they leave out iPhone's bar.
                if rendersInteractiveControls {
                    Spacer(minLength: 0)
                    Button("Discard") { discard() }
                        .buttonStyle(.plain)
                        .font(FreesideFont.cardBody)
                        .foregroundStyle(Color.inkDim)
                        .padding(.vertical, 4)
                        .disabled(!Self.canDiscard(draftStore.draft, isFrozen: isFrozen))
                        .help("Discard this draft")
                }
            #endif
        }
        .padding(.top, 8)
        .padding(.horizontal, 10)
        .padding(.bottom, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    @ViewBuilder private var projectChip: some View {
        let chip = ComposerChip(glyph: "▣", text: projectLabel)
        if rendersInteractiveControls {
            Menu {
                Picker("Project", selection: projectBinding) {
                    ForEach(projects, id: \.self) { project in
                        Text(project).tag(project)
                    }
                }
                .pickerStyle(.inline)
            } label: {
                chip
            }
            .menuStyle(.button)
            .buttonStyle(.plain)
            .menuIndicator(.hidden)
            .accessibilityLabel("Project")
            .accessibilityValue(projectLabel)
        } else {
            chip
        }
    }

    private var projectLabel: String {
        projectID.isEmpty ? "No project" : projectID
    }

    /// Closed, the chip reads the choices as one sentence; open, it reads
    /// `Agents` over the role chips. The sentence keeps to one line and
    /// truncates its tail, since a choice in full can outgrow the sheet;
    /// the role chips, the accessibility value, and the Mac tooltip carry it
    /// whole. At the accessibility sizes it wraps instead, where one line
    /// would leave a word or two. With no catalog there is nothing to
    /// choose, unless a choice made against an earlier catalog is still set:
    /// the chip stays enabled so the operator can see and clear it.
    @ViewBuilder private var agentsChip: some View {
        let summary =
            AgentChoiceDisplay.summary(AgentChoiceDisplay.taskLines(choices: choices), catalog: model.agentCatalog)
            ?? "Default agents"
        // An open chip stays enabled so it can close, even once clearing
        // the last stale choice leaves nothing to choose. A daemon with no
        // tree serves a catalog with no lineup revision, so there is nothing
        // to choose.
        let unavailable = model.agentCatalog?.lineup_revision == nil && choices.isEmpty && !agentsOpen
        let chip = ComposerChip(glyph: "◆", text: agentsOpen ? "Agents" : summary, isOpen: agentsOpen)
            .lineLimit(dynamicTypeSize.isAccessibilitySize ? nil : 1)
        if rendersInteractiveControls {
            Button {
                draftStore.draft.agentsOpen.toggle()
            } label: {
                chip
            }
            .buttonStyle(.plain)
            .disabled(unavailable)
            .help(unavailable ? Self.noTreeText : summary)
            .accessibilityLabel("Agents")
            .accessibilityValue(summary)
            .accessibilityHint(unavailable ? Self.noTreeText : "")
        } else {
            chip.disabled(unavailable)
        }
    }

    // MARK: Status and submit

    @ViewBuilder private var status: some View {
        switch model.state {
        case .lost:
            Notice(
                tone: .accent, keyword: "Unconfirmed",
                sentence: Self.unconfirmedSentence
            )
            .accessibilityElement(children: .combine)
        case .rejected(let reason):
            Notice(tone: .wax, keyword: "Failed", sentence: reason)
                .accessibilityElement(children: .combine)
        case .idle, .submitting, .submitted:
            EmptyView()
        }
    }

    /// What the draft says about itself under the composer: attachments a
    /// relaunch left out, and a draft this device couldn't save. Neither
    /// blocks composing or submitting.
    @ViewBuilder private var draftNotices: some View {
        if draftStore.attachmentsDroppedAtRelaunch, sentDraft == nil {
            Text(Self.droppedAttachmentsSentence)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
        }
        if let warning = draftStore.saveWarning {
            Notice(tone: .wax, keyword: "Not saved", sentence: warning)
                .accessibilityElement(children: .combine)
        }
    }

    private func discard() {
        draftStore.clear()
        dismiss()
    }

    /// True whether the response was lost or an attachment's upload was:
    /// either way the request is saved for Retry.
    static let unconfirmedSentence =
        "Freeside couldn't confirm this request. It's saved in Unconfirmed submissions in Tasks."

    /// Submits when the draft can send and says whether it did; the Mac
    /// text view asks on Return and VoiceOver's Submit action.
    @discardableResult private func performSubmit() -> Bool {
        guard canSubmit else { return false }
        isSubmitting = true
        let projectID = projectID
        let sent = Self.detach(draftStore)
        sentDraft = sent
        submitTask = Task {
            let taskID = await Self.send(
                Self.submission(source: sent.source), projectID: projectID, choices: sent.choices,
                attachments: sent.attachments, through: model)
            isSubmitting = false
            // Settled before the cancel check: a sheet closed mid-submit
            // still puts back a draft the daemon didn't take.
            if !Self.settle(draftStore, sent: sent, after: model.state, taskID: taskID) { sentDraft = nil }
            guard !Task.isCancelled else { return }
            if let taskID {
                onSubmitted(taskID)
                dismiss()
            }
        }
        return true
    }
}
