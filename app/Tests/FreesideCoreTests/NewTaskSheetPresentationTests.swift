import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The New Task composer's text and what it sends (visual audit D05): an
/// empty text area shows a placeholder that is never its value and never
/// submitted, and a submission carries the trimmed draft and no name.
@MainActor
@Suite struct NewTaskSheetPresentationTests {
    private let placeholder = NewTaskSheet.sourcePlaceholder

    @Test func emptyDraftShowsThePlaceholderAndSubmitsNothing() {
        #expect(placeholder == "What should the agent work on?")
        #expect(NewTaskSheet.fieldDisplay("", placeholder: placeholder) == .placeholder(placeholder))
        #expect(NewTaskSheet.submission(source: "").source.isEmpty)
    }

    /// One rule for the editor's drawn placeholder and the static render:
    /// any typed character hides it. The whitespace is still not submitted.
    @Test func whitespaceOnlyDraftHidesThePlaceholderAndSubmitsNothing() {
        for blank in ["  ", "\n\t"] {
            #expect(NewTaskSheet.fieldDisplay(blank, placeholder: placeholder) == .value(blank))
        }
        #expect(NewTaskSheet.submission(source: " \n") == .init(source: ""))
    }

    @Test func typedTextIsAValueEvenWhenItReadsLikeThePlaceholder() {
        #expect(
            NewTaskSheet.fieldDisplay("Fix the flaky rig", placeholder: placeholder) == .value("Fix the flaky rig"))
        // The operator typed these words, so they are the draft.
        #expect(NewTaskSheet.fieldDisplay(placeholder, placeholder: placeholder) == .value(placeholder))
        #expect(NewTaskSheet.submission(source: placeholder).source == placeholder)
    }

    @Test func submissionCarriesOnlyTheTrimmedDraft() {
        let draft = NewTaskSheet.submission(source: "  Repair the acceptance rig\n")
        #expect(draft == .init(source: "Repair the acceptance rig"))
    }

    /// What the sheet sends: the trimmed source and the per-role lines, and
    /// never a name, which the daemon's task namer supplies.
    @Test func sendCarriesTheLinesAndNoName() async throws {
        let server = MockServer()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        let model = TaskSubmissionModel(coordinator: coordinator)
        // An unanswered send keeps the saved command inspectable.
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        let choices: [NewTaskSheet.Role: AgentChoiceDisplay.Selection] = [
            .specifier: UsageFixtures.agentSelection("claude-secondary"),
            .implementer: UsageFixtures.agentSelection("claude-secondary"),
            .remediator: UsageFixtures.agentSelection("claude-secondary"),
            .reviewer: UsageFixtures.agentSelection("codex-primary"),
        ]
        _ = await NewTaskSheet.send(
            NewTaskSheet.submission(source: " Add a health endpoint.\n"), projectID: "project-1", choices: choices,
            through: model)
        let command = try #require(model.pendingSubmissions.first)
        guard case .submit_task(let payload) = command.payload else {
            Issue.record("expected a submit_task command")
            return
        }
        #expect(payload.source == "Add a health endpoint.")
        #expect(payload.name == nil)
        #expect(payload.task_lines == AgentChoiceDisplay.taskLines(choices: choices))
    }

    @Test func aRepositoryPathGoesAtTheEndAfterASpace() {
        #expect(NewTaskSheet.appendingPath("docs/design/", to: "") == "docs/design/")
        #expect(NewTaskSheet.appendingPath("docs/design/", to: "Match") == "Match docs/design/")
        #expect(NewTaskSheet.appendingPath("docs/design/", to: "Match ") == "Match docs/design/")
        #expect(NewTaskSheet.appendingPath("docs/design/", to: "Match\n") == "Match\ndocs/design/")
    }

    @Test func aFolderRowSummarizesItsFiles() {
        let files: [Components.Schemas.TaskAttachment] = [
            .init(
                digest: "sha256:" + String(repeating: "a", count: 64), name: "design/a.txt",
                media_type: .application_sol_octet_hyphen_stream, claimed_media_type: "text/plain", size_bytes: 1000),
            .init(
                digest: "sha256:" + String(repeating: "b", count: 64), name: "design/b.txt",
                media_type: .application_sol_octet_hyphen_stream, claimed_media_type: "text/plain", size_bytes: 1000),
        ]
        #expect(TaskPageView.folderSummary(files) == "2 files · \(ComposerAttachmentChip.size(2000))")
        #expect(TaskPageView.folderSummary(Array(files.prefix(1))).hasPrefix("1 file · "))
    }

    // MARK: The draft (#2935)

    private func filledDraft() -> NewTaskDraftStore {
        let drafts = NewTaskDraftStore(deploymentID: "mock", deviceID: "device")
        drafts.draft.source = "Add a health endpoint."
        drafts.draft.projectID = "project-1"
        drafts.draft.choices[.reviewer] = UsageFixtures.agentSelection("codex-primary")
        return drafts
    }

    /// Sends the store's draft as Submit does: detached first, settled by
    /// the result.
    private func submit(_ drafts: NewTaskDraftStore, through model: TaskSubmissionModel) async -> Bool {
        let sent = NewTaskSheet.detach(drafts)
        #expect(drafts.draft.isEmpty)
        let taskID = await NewTaskSheet.send(
            NewTaskSheet.submission(source: sent.source), projectID: "project-1", choices: [:], through: model)
        return NewTaskSheet.settle(drafts, sent: sent, after: model.state, taskID: taskID)
    }

    private func model(failingSubmitWith status: Int? = nil) async -> TaskSubmissionModel {
        let server = MockServer()
        if let status {
            await server.setBeforeRespond { operationID in
                if operationID == "submitCommand" { throw MockServer.ForcedStatus(status) }
            }
        }
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        return TaskSubmissionModel(coordinator: coordinator)
    }

    @Test func aSuccessfulSubmitClearsTheDraft() async {
        let drafts = filledDraft()
        let settled = await submit(drafts, through: await model())
        #expect(settled)
        #expect(drafts.draft.isEmpty)
    }

    /// A lost response may have committed, and its request waits in
    /// Unconfirmed submissions; a draft left behind would offer to send it
    /// again.
    @Test func aLostResponseClearsTheDraft() async {
        let drafts = filledDraft()
        let model = await model(failingSubmitWith: 503)
        let settled = await submit(drafts, through: model)
        #expect(model.state == .lost)
        #expect(settled)
        #expect(drafts.draft.isEmpty)
    }

    @Test func aRejectionPutsTheDraftBack() async {
        let drafts = filledDraft()
        let model = await model(failingSubmitWith: 400)
        let settled = await submit(drafts, through: model)
        guard case .rejected = model.state else {
            Issue.record("expected a rejection, got \(model.state)")
            return
        }
        #expect(!settled)
        #expect(drafts.draft.source == "Add a health endpoint.")
        #expect(drafts.draft.choices[.reviewer] != nil)
    }

    /// While a request is in flight the store holds no draft, so a composer
    /// reopened then can't send it twice; a draft started there survives a
    /// rejection of the first.
    @Test func aDraftStartedWhileSubmittingSurvivesTheSubmitsResult() {
        let drafts = filledDraft()
        let sent = NewTaskSheet.detach(drafts)
        #expect(drafts.draft.isEmpty)
        drafts.draft.source = "Something new"
        #expect(!NewTaskSheet.settle(drafts, sent: sent, after: .rejected("No."), taskID: nil))
        #expect(drafts.draft.source == "Something new")
        #expect(NewTaskSheet.settle(drafts, sent: sent, after: .lost, taskID: nil))
        #expect(drafts.draft.source == "Something new")
    }

    /// Closing while Up shows a recalled prompt keeps what the operator
    /// typed, not the prompt; after a submit took the draft, it keeps nothing.
    @Test func closingMidBrowseKeepsTheTypedText() {
        let drafts = filledDraft()
        let browse = TaskPromptBrowseState()
        drafts.draft.source = browse.up(from: drafts.draft.source, history: ["An old prompt"]) ?? ""
        #expect(drafts.draft.source == "An old prompt")
        NewTaskSheet.close(drafts, browse: browse, submitted: false)
        #expect(drafts.draft.source == "Add a health endpoint.")

        let empty = NewTaskDraftStore(deploymentID: "mock", deviceID: "device")
        let recalled = TaskPromptBrowseState()
        empty.draft.source = recalled.up(from: "", history: ["An old prompt"]) ?? ""
        NewTaskSheet.close(empty, browse: recalled, submitted: false)
        #expect(empty.draft.isEmpty)

        let submitted = NewTaskDraftStore(deploymentID: "mock", deviceID: "device")
        let browsing = TaskPromptBrowseState()
        _ = browsing.up(from: "Typed", history: ["An old prompt"])
        NewTaskSheet.close(submitted, browse: browsing, submitted: true)
        #expect(submitted.draft.isEmpty)
    }

    @Test func onlyACreatedOrPossiblyCreatedTaskSettlesTheDraft() {
        #expect(NewTaskSheet.settles(.idle, taskID: "task-1"))
        #expect(NewTaskSheet.settles(.submitted(taskID: "task-1"), taskID: nil))
        #expect(NewTaskSheet.settles(.lost, taskID: nil))
        #expect(!NewTaskSheet.settles(.rejected("No."), taskID: nil))
        #expect(!NewTaskSheet.settles(.idle, taskID: nil))
    }

    @Test func discardOffersItselfOnlyForAnEditableDraft() {
        var draft = NewTaskDraft()
        draft.projectID = "project-1"
        #expect(!NewTaskSheet.canDiscard(draft, isFrozen: false))
        draft.source = "Something"
        #expect(NewTaskSheet.canDiscard(draft, isFrozen: false))
        #expect(!NewTaskSheet.canDiscard(draft, isFrozen: true))
    }

    /// A restored project the synced projects no longer hold falls back to
    /// the first of them, as a fresh composer starts; the text stays.
    @Test func aGoneProjectFallsBackToTheFirstKnownOne() {
        #expect(NewTaskSheet.resolvedProject("b", in: ["a", "b"]) == "b")
        #expect(NewTaskSheet.resolvedProject("gone", in: ["a", "b"]) == "a")
        #expect(NewTaskSheet.resolvedProject(nil, in: ["a", "b"]) == "a")
        #expect(NewTaskSheet.resolvedProject("gone", in: []) == "")
    }
}
