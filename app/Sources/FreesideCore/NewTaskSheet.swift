import FreesideAPI
import SwiftUI

/// The New Task composer (plan §5.11): an operator starts work from the app
/// instead of the daemon host's CLI. Shaped like the discuss composer
/// (`MessageComposerSheet`): an inline serif title over a project picker, a
/// multi-line source field, and an optional name, each under its keyword
/// label, and a Cancel/Submit footer that Return and Escape drive, with no
/// navigation bar. The draft lives in this sheet's own state and no failure
/// clears it: a lost response keeps the text and points to separate recovery,
/// a daemon rejection keeps the text and shows the reason inline. The sheet
/// expects a sentence or a paragraph, not a document; the follow-up
/// conversation happens on the task's cards.
struct NewTaskSheet: View {
    @Environment(\.dismiss) private var dismiss
    /// The projects the picker offers: the sorted unique ids of the synced
    /// tasks (`TaskDisplay.knownProjects`), the same set the Tasks filter uses.
    let projects: [String]
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
    /// `TextEditor`, a `Picker`, or a system control, so those render as static
    /// stand-ins that still show the draft.
    var rendersInteractiveControls = true

    @State private var projectID: String
    @State private var source: String
    @State private var name: String
    @State private var isSubmitting = false
    /// The in-flight submit, held so dismissing the sheet cancels it
    /// and suppresses the completion's navigation (the submission itself may
    /// still land server-side, which the idempotency key reconciles).
    @State private var submitTask: Task<Void, Never>?

    init(
        projects: [String],
        model: TaskSubmissionModel,
        onSubmitted: @escaping (String) -> Void,
        rendersInteractiveControls: Bool = true,
        initialProjectID: String? = nil,
        initialSource: String = "",
        initialName: String = ""
    ) {
        self.projects = projects
        _model = State(initialValue: model)
        self.onSubmitted = onSubmitted
        self.rendersInteractiveControls = rendersInteractiveControls
        _projectID = State(initialValue: initialProjectID ?? projects.first ?? "")
        _source = State(initialValue: initialSource)
        _name = State(initialValue: initialName)
    }

    static let sourcePlaceholder = "Describe the task…"
    static let namePlaceholder = "Name (optional)"

    /// What a field draws: the muted placeholder while the draft holds no
    /// characters, otherwise the draft exactly as typed. Whitespace is typed
    /// text, so it hides the placeholder as it hides the native name
    /// field's prompt; `submission` is what trims it away. Typed text that
    /// happens to read like the placeholder is still a value (visual audit
    /// D05: an empty field looks empty, and a placeholder is never content).
    enum FieldDisplay: Equatable {
        case placeholder(String)
        case value(String)
    }

    static func fieldDisplay(_ draft: String, placeholder: String) -> FieldDisplay {
        draft.isEmpty ? .placeholder(placeholder) : .value(draft)
    }

    /// What Submit sends for a draft: both fields trimmed, and no name at
    /// all when the optional field holds nothing.
    struct Submission: Equatable {
        let source: String
        let name: String?
    }

    static func submission(source: String, name: String) -> Submission {
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        return Submission(
            source: source.trimmingCharacters(in: .whitespacesAndNewlines),
            name: name.isEmpty ? nil : name)
    }

    private var trimmedSource: String {
        Self.submission(source: source, name: name).source
    }

    /// A lost response may have committed a task server-side, so the sheet
    /// cannot submit the same draft again. Recovery belongs to the separate
    /// Unconfirmed submissions screen; reopening New Task starts new work.
    private var isLost: Bool {
        if case .lost = model.state { return true }
        return false
    }

    private var canSubmit: Bool {
        !trimmedSource.isEmpty && !projectID.isEmpty && projects.contains(projectID)
            && !isSubmitting && !isLost
            && TaskSubmissionModel.canCompose(freshness: model.freshness)
    }

    var body: some View {
        VStack(spacing: 0) {
            if rendersInteractiveControls {
                ScrollView { composerContent }
            } else {
                composerContent
            }

            // The submit lives in the sheet body, not a toolbar, so it carries
            // the design language's primary recipe and the row's Return and
            // Escape bindings (matching the discuss composer).
            FreesideSheetActionRow(
                submitLabel: "Submit",
                isSubmitEnabled: canSubmit,
                submit: performSubmit,
                cancel: { dismiss() })
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
        .frame(minWidth: 380, minHeight: 320)
        // Canceling, Escape, or an interactive dismiss ends the presentation
        // while a submit is still in flight; cancel the task so its completion
        // does not route the operator to a task they navigated away from.
        .onDisappear { submitTask?.cancel() }
    }

    // Keep the form scrollable at large text sizes, with fixed actions.
    private var composerContent: some View {
        VStack(spacing: 0) {
            FreesideSheetHeader(
                ask: "New task",
                consequence:
                    "Describe the work in a sentence or a paragraph. The agent asks before it specifies when the source is a sketch."
            )
            VStack(alignment: .leading, spacing: 12) {
                // Freeze an uncertain submission so editing cannot imply
                // that another Submit would retry the saved request.
                Group {
                    labeled("Project") { projectPicker }
                    labeled("Work to do") { sourceField }
                    labeled("Name (optional)") { nameField }
                }
                .disabled(isLost || isSubmitting)
                status
            }
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
            .frame(maxWidth: .infinity, alignment: .topLeading)

        }
    }

    /// A field under its visible label. Each control carries the same words
    /// as its accessibility label, so VoiceOver reads the label once.
    private func labeled(_ label: String, @ViewBuilder field: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            KeywordLabel(text: label)
                .accessibilityHidden(true)
            field()
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
        .font(FreesideFont.callout)
    }

    // Where a TextEditor draws its first character inside the field's own
    // padding: both platforms inset a line by 5pt, and UITextView adds 8pt
    // above the text.
    #if os(iOS)
        private static let sourcePlaceholderInsets = EdgeInsets(top: 16, leading: 13, bottom: 8, trailing: 13)
    #else
        private static let sourcePlaceholderInsets = EdgeInsets(top: 8, leading: 13, bottom: 8, trailing: 13)
    #endif

    @ViewBuilder private var projectPicker: some View {
        // Only the trigger is Freeside's; the popup list stays system chrome,
        // which `ImageRenderer` cannot draw, so the goldens show the trigger.
        if rendersInteractiveControls {
            Menu {
                Picker("Project", selection: $projectID) {
                    ForEach(projects, id: \.self) { project in
                        Text(project).tag(project)
                    }
                }
                .pickerStyle(.inline)
            } label: {
                FreesideMenuTriggerLabel(title: projectLabel)
            }
            .menuStyle(.button)
            .buttonStyle(.plain)
            .menuIndicator(.hidden)
            .accessibilityLabel("Project")
            .accessibilityValue(projectLabel)
        } else {
            FreesideMenuTriggerLabel(title: projectLabel)
        }
    }

    private var projectLabel: String {
        projectID.isEmpty ? "No project" : projectID
    }

    @ViewBuilder private var sourceField: some View {
        if rendersInteractiveControls {
            TextEditor(text: $source)
                .font(FreesideFont.callout)
                .scrollContentBackground(.hidden)
                .padding(8)
                .frame(minHeight: 150)
                .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                // A TextEditor has no prompt of its own. This one is drawn
                // over the empty editor, never written into `source`, and
                // leaves clicks and VoiceOver to the editor beneath it.
                .overlay(alignment: .topLeading) {
                    if case .placeholder(let placeholder) = Self.fieldDisplay(
                        source, placeholder: Self.sourcePlaceholder)
                    {
                        Text(placeholder)
                            .font(FreesideFont.callout)
                            .foregroundStyle(Color.inkDim)
                            .padding(Self.sourcePlaceholderInsets)
                            .allowsHitTesting(false)
                            .accessibilityHidden(true)
                    }
                }
                .accessibilityLabel("Work to do")
        } else {
            staticFieldText(source, placeholder: Self.sourcePlaceholder)
                .padding(8)
                .frame(maxWidth: .infinity, minHeight: 150, alignment: .topLeading)
                .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
        }
    }

    @ViewBuilder private var nameField: some View {
        if rendersInteractiveControls {
            TextField(Self.namePlaceholder, text: $name)
                .textFieldStyle(.plain)
                .font(FreesideFont.callout)
                .padding(8)
                .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
                .accessibilityLabel("Name (optional)")
        } else {
            staticFieldText(name, placeholder: Self.namePlaceholder)
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.ground, in: RoundedRectangle(cornerRadius: 8))
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.rule))
        }
    }

    @ViewBuilder private var status: some View {
        switch model.state {
        case .lost:
            Text("The result couldn't be confirmed. Your request is saved in Unconfirmed submissions in Tasks.")
                .font(FreesideFont.callout)
                .foregroundStyle(Color.waxText)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        case .rejected(let reason):
            Text(reason)
                .font(FreesideFont.callout)
                .foregroundStyle(Color.waxText)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        case .idle, .submitting, .submitted:
            EmptyView()
        }
    }

    private func performSubmit() {
        isSubmitting = true
        submitTask = Task {
            let draft = Self.submission(source: source, name: name)
            let taskID = await model.submit(projectID: projectID, source: draft.source, name: draft.name)
            isSubmitting = false
            guard !Task.isCancelled else { return }
            if let taskID {
                onSubmitted(taskID)
                dismiss()
            }
        }
    }
}
