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
}
