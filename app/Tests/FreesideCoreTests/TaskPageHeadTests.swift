import Foundation
import FreesideAPI
import FreesideTestSupport
import SwiftUI
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// The task page's head (#2609): the documents line under the status, the
/// outline row's chips, and the specification and prompt readers the line
/// opens (#2612).
@Suite @MainActor struct TaskPageHeadTests {
    private let approvedDigest = "sha256:" + String(repeating: "c", count: 64)
    private let promptDigest = "sha256:" + String(repeating: "e", count: 64)
    private let repo = "freeside-ai/freeside"

    private func links(
        _ page: TaskProgressFixtures.Case, history: Components.Schemas.TaskTimeline?,
        completion: Components.Schemas.WorkUnitCompletionFacts? = nil
    ) -> [TaskTimelinePresentation.DocumentLink] {
        TaskTimelinePresentation.documentLinks(
            page.task, timeline: history, completion: completion, items: page.items.map(\.item))
    }

    private func completion(pr number: Int) -> Components.Schemas.WorkUnitCompletionFacts {
        .init(pr_number: number, merge_commit_sha: "4be1d0a7", recorded_at: Date(timeIntervalSince1970: 0))
    }

    private func issueURL(_ number: Int) -> URL? { URL(string: "https://github.com/\(repo)/issues/\(number)") }

    /// An item under the task that names the PR's repository.
    private func readyItem(pr number: Int) -> Components.Schemas.AttentionItemSnapshot {
        var item = TaskStageFixtures.item(.ready_for_final_review, id: "head-ready", hoursOld: 0)
        item.item.pr_reference = .init(value1: .init(repo: repo, number: number))
        return item
    }

    /// A held approval whose Specification claim binds `digest`.
    private func approval(binding digest: String) throws -> Components.Schemas.AttentionItemSnapshot {
        var item = TaskStageFixtures.item(.spec_approval, id: "head-approval", hoursOld: 0)
        let index = try #require(
            item.item.agent_claims.firstIndex { $0.label == AgentClaimLabels.specification })
        item.item.agent_claims[index].digest = digest
        return item
    }

    // MARK: Documents line

    @Test func anIssueTaskWithANamedPullRequestLinksAllThree() throws {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 2431)]
        let source = try #require(TaskDisplay.issueReference(page.task))
        let issue = try #require(Int(source.dropFirst()))
        #expect(
            links(page, history: page.history, completion: completion(pr: 2431)) == [
                .issue(number: issue, url: issueURL(issue)),
                .specification(digest: approvedDigest, version: nil),
                .pullRequest(number: 2431, url: URL(string: "https://github.com/\(repo)/pull/2431")),
            ])
    }

    @Test func aTypedTaskLeadsWithItsPromptThenTheSpecificationAndNoPullRequest() {
        var page = TaskStageFixtures.page(.implementation)
        page.task.source = .init(
            value1: .work_item_artifact(
                .init(
                    kind: .work_item_artifact, work_item_artifact_id: "source-1",
                    work_item_artifact_digest: .init(value1: promptDigest))))
        page.items = []
        #expect(
            links(page, history: page.history) == [
                .prompt(digest: promptDigest), .specification(digest: approvedDigest, version: nil), .noPullRequest,
            ])
    }

    @Test func aTaskWithNoSourceHasNeitherIssueNorPrompt() {
        var page = TaskStageFixtures.page(.implementation)
        page.task.source = nil
        page.items = []
        #expect(
            links(page, history: page.history) == [
                .specification(digest: approvedDigest, version: nil), .noPullRequest,
            ])
    }

    @Test func aTaskStillInSpecificationHasNoSpecificationLink() {
        var page = TaskStageFixtures.page(.specification)
        page.items = []
        let parts = links(page, history: page.history)
        #expect(!parts.contains { if case .specification = $0 { true } else { false } })
        #expect(parts.last == .noPullRequest)
    }

    @Test func aPullRequestNoHeldItemNamesIsPlainText() {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 99)]
        #expect(
            links(page, history: page.history, completion: completion(pr: 2431)).last
                == .pullRequest(number: 2431, url: nil))
    }

    @Test func theHistoryRecordsThePullRequestBeforeACompletionDoes() throws {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 77)]
        var history = try #require(page.history)
        history.events.insert(
            .init(kind: .pr_opened, recorded_at: Date(), run_id: page.task.run_ids.last, pr_number: 77), at: 0)
        #expect(
            links(page, history: history).last
                == .pullRequest(number: 77, url: URL(string: "https://github.com/\(repo)/pull/77")))
    }

    @Test func beforeTheHistoryLoadsThePullRequestPartIsLeftOut() {
        var page = TaskStageFixtures.page(.review)
        page.items = []
        let parts = links(page, history: nil)
        #expect(parts.count == 1)
        if case .issue = parts.first {} else { Issue.record("expected the issue alone, got \(parts)") }
    }

    /// A PR the run's completion names needs no history, so it draws
    /// before the history loads; only `No pull request yet` waits for it.
    @Test func aCompletionNamesThePullRequestBeforeTheHistoryLoads() {
        var page = TaskStageFixtures.page(.review)
        page.items = [readyItem(pr: 2431)]
        #expect(
            links(page, history: nil, completion: completion(pr: 2431)).last
                == .pullRequest(number: 2431, url: URL(string: "https://github.com/\(repo)/pull/2431")))
    }

    /// The version word is the held approval's, and only when its
    /// Specification claim binds the digest the campaign approved.
    @Test func theVersionComesOnlyFromAnApprovalBindingTheApprovedDigest() throws {
        var page = TaskStageFixtures.page(.implementation)
        let matching = try approval(binding: approvedDigest)
        let version = try #require(DecisionDetailView.specificationVersion(in: matching.item))
        page.items = [matching]
        #expect(
            links(page, history: page.history).contains(
                .specification(digest: approvedDigest, version: version)))

        page.items = [try approval(binding: "sha256:" + String(repeating: "d", count: 64))]
        #expect(
            links(page, history: page.history).contains(.specification(digest: approvedDigest, version: nil)))
        #expect(SpecApprovalReader.versionChip(nil) == nil)
        #expect(SpecApprovalReader.versionChip(version)?.label == "Version \(version)")
    }

    /// VoiceOver reads each part with somewhere to go as a link naming
    /// where it goes; a part with nowhere to go is plain text.
    @Test func eachDocumentLinkSaysWhereItGoes() {
        let url = URL(string: "https://github.com/\(repo)/issues/712")
        let cases: [(TaskTimelinePresentation.DocumentLink, String, String, String?)] = [
            (.issue(number: 712, url: url), "Issue #712", "Issue 712", "Opens on GitHub"),
            (.prompt(digest: promptDigest), "Prompt", "Prompt", "Opens the prompt reader"),
            (
                .specification(digest: approvedDigest, version: 2), "Specification v2", "Specification version 2",
                "Opens the specification reader"
            ),
            (
                .specification(digest: approvedDigest, version: nil), "Specification", "Specification",
                "Opens the specification reader"
            ),
            (.pullRequest(number: 2431, url: url), "Pull request #2431", "Pull request 2431", "Opens on GitHub"),
            (.pullRequest(number: 2431, url: nil), "Pull request #2431", "Pull request 2431", nil),
            (.noPullRequest, "No pull request yet", "No pull request yet", nil),
        ]
        for (link, title, spoken, hint) in cases {
            #expect(link.title == title)
            #expect(link.spokenTitle == spoken)
            #expect(link.accessibilityHint == hint)
        }
    }

    /// The two reader parts open their reader through the same link-trait
    /// button; the GitHub and plain parts open none.
    @Test func eachReaderPartOpensItsOwnDocument() {
        #expect(
            TaskTimelinePresentation.DocumentLink.prompt(digest: promptDigest).readerRequest
                == .init(digest: promptDigest, document: .prompt))
        #expect(
            TaskTimelinePresentation.DocumentLink.specification(digest: approvedDigest, version: 2).readerRequest
                == .init(digest: approvedDigest, document: .specification(version: 2)))
        let none: [TaskTimelinePresentation.DocumentLink] = [
            .issue(number: 712, url: nil), .pullRequest(number: 2431, url: nil), .noPullRequest,
        ]
        #expect(none.allSatisfy { $0.readerRequest == nil })
    }

    // MARK: Look-Alike Marker

    /// Acceptance (#1922): a look-alike task's header draws the marker its
    /// row draws, so a card on the page says which task it belongs to, and
    /// a task with no look-alike draws none. The twin shares the source
    /// issue under another name.
    @Test func onlyALookAlikeTasksHeaderDrawsTheMarker() throws {
        let page = TaskStageFixtures.page(.review)
        let now = RunFixtures.screenshotInstant
        let utc = try #require(TimeZone(identifier: "UTC"))
        let locale = Locale(identifier: "en_US")
        func marker(_ coordinator: SyncCoordinator, _ task: Components.Schemas.Task) -> String? {
            TaskPageView.lookAlikeMarker(task, in: coordinator.unifiedList, now: now, locale: locale, timeZone: utc)
        }
        let (alone, task) = try TaskStageFixtures.coordinator(page)
        #expect(TaskDisplay.issueReference(task.task) != nil)
        #expect(marker(alone, task.task) == nil)

        var twin = task
        twin.task.id = "task-twin"
        twin.task.display_names.task = .init(text: "Another name for the same issue", source: ._operator)
        twin.task.created_at = task.task.created_at.addingTimeInterval(-3600)
        let (paired, _) = try TaskStageFixtures.coordinator(page, otherTasks: [twin])
        #expect(
            marker(paired, task.task)
                == TaskDisplay.lookAlikeMarker(task.task, now: now, locale: locale, timeZone: utc))
        #expect(
            marker(paired, twin.task) == TaskDisplay.lookAlikeMarker(twin.task, now: now, locale: locale, timeZone: utc)
        )
    }

    // MARK: Outline and Task Actions

    /// Task Actions sits in the outline row at regular width and in the
    /// navigation bar on a phone, so the phone's row holds no menu. Its own
    /// item draws while the Identifiers footer does: once the history
    /// loads, or while the leading decision names identifiers.
    @Test func taskActionsSitWherePinnedAndHoldTodaysItems() {
        #expect(TaskPageView.TaskActionsSlot(compact: false) == .outlineRow)
        #expect(TaskPageView.TaskActionsSlot(compact: true) == .navigationBar)
        #expect(TaskPageView.taskActions(nil, task: .init(taskID: "task-1")).isEmpty)
        #expect(
            TaskPageView.taskActions(
                nil, task: .init(taskID: "task-1"), decision: [.init(label: "Item", value: "item-1")]
            )
            .map(\.title) == ["Copy all identifiers"])
        let history = TaskStageFixtures.page(.review).history
        #expect(
            TaskPageView.taskActions(history, task: .init(taskID: "task-1")).map(\.title) == ["Copy all identifiers"])
    }

    /// Acceptance 3 (#3041): the Conversation chip sits where its thread
    /// is, in page order: before the Timeline for the card's thread, after
    /// Runs for Earlier Conversations, and nowhere with neither.
    @Test func theConversationChipFollowsTheThread() {
        #expect(
            TaskPageSection.outline(decisions: 2, conversation: .inCard(messages: 3)) == [
                .decisions, .task, .conversation, .timeline, .runs,
            ])
        #expect(OutlineConversation.inCard(messages: 3).messages == 3)
        #expect(
            TaskPageSection.outline(decisions: 0, conversation: .earlier(messages: 5)) == [
                .task, .timeline, .runs, .conversation,
            ])
        #expect(OutlineConversation.earlier(messages: 5).messages == 5)
        #expect(TaskPageSection.outline(decisions: 0, conversation: nil) == [.task, .timeline, .runs])
        #expect(TaskPageSection.outline(decisions: 1, conversation: nil) == [.decisions, .task, .timeline, .runs])
    }

    // MARK: Readers

    @Test func theReaderLoadsTheApprovedBytesAndNamesTheDigestWhenItCannot() async {
        let bytes = Data("# Specification\n\nStore first, then API.\n".utf8)
        let loaded = AttachmentLoader(
            client: APIClientFactory.mock(server: MockServer(attachments: [approvedDigest: bytes])))
        #expect(
            await TaskDocumentReaderSheet.load(approvedDigest) { await loaded.nonImageBytes(for: $0) }
                == .loaded(bytes))

        let missing = AttachmentLoader(client: APIClientFactory.mock(server: MockServer(attachments: [:])))
        #expect(
            await TaskDocumentReaderSheet.load(approvedDigest) { await missing.nonImageBytes(for: $0) }
                == .unavailable)

        let server = MockServer(attachments: [approvedDigest: bytes])
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { throw InjectedFailure() }
        }
        let failing = AttachmentLoader(client: APIClientFactory.mock(server: server))
        #expect(
            await TaskDocumentReaderSheet.load(approvedDigest) { await failing.nonImageBytes(for: $0) }
                == .unavailable)
    }

    /// The prompt reader reads the bytes a server serves under the
    /// prompt's digest, and a missing or failed read is the unavailable
    /// state, which names the digest under the prompt's row. MockServerTests
    /// covers a submission storing those bytes.
    @Test func thePromptReaderLoadsTheSubmittedBytesAndNamesTheDigestWhenItCannot() async {
        let bytes = Data("Add a health endpoint.\n\nKeep the old route for one release.\n".utf8)
        let loaded = AttachmentLoader(
            client: APIClientFactory.mock(server: MockServer(attachments: [promptDigest: bytes])))
        #expect(
            await TaskDocumentReaderSheet.load(promptDigest) { await loaded.nonImageBytes(for: $0) } == .loaded(bytes))

        let missing = AttachmentLoader(client: APIClientFactory.mock(server: MockServer(attachments: [:])))
        #expect(
            await TaskDocumentReaderSheet.load(promptDigest) { await missing.nonImageBytes(for: $0) } == .unavailable)

        let server = MockServer(attachments: [promptDigest: bytes])
        await server.setBeforeRespond { operationID in
            if operationID == "getAttachment" { throw InjectedFailure() }
        }
        let failing = AttachmentLoader(client: APIClientFactory.mock(server: server))
        #expect(
            await TaskDocumentReaderSheet.load(promptDigest) { await failing.nonImageBytes(for: $0) } == .unavailable)

        let prompt = TaskDocumentRequest.Document.prompt
        #expect(prompt.eyebrow == "Prompt reader")
        #expect(prompt.title == "Prompt")
        #expect(prompt.chip == nil)
        #expect(prompt.unavailableTitle == "Prompt unavailable")
        #expect(prompt.unavailableDescription == "Freeside could not read the submitted prompt from the daemon.")
        #expect(prompt.framing.keyword == "Prompt")
        #expect(prompt.framing.digestLabel == "Prompt Digest")
    }

    /// An operator-typed prompt is not a claim: no `(unverified)` label and
    /// the plain card. The specification reader keeps both, on the task
    /// page and on a `spec_approval` card, which takes the default framing.
    @Test func thePromptIsNotAClaimWhileTheSpecificationIs() {
        let prompt = TaskDocumentRequest.Document.prompt.framing
        #expect(!prompt.keyword.contains("unverified"))
        #expect(!prompt.dashedFrame)

        let specification = TaskDocumentRequest.Document.specification(version: 2)
        #expect(specification.framing == .specification)
        #expect(specification.framing.keyword == "Specification (unverified)")
        #expect(specification.framing.dashedFrame)
        #expect(specification.framing.digestLabel == "Daemon-Bound Digest")
        #expect(specification.eyebrow == "Specification reader")
        #expect(specification.title == "Specification")
        #expect(specification.chip?.label == "Version 2")
        #expect(
            SpecificationReaderView(text: "# Spec", mediaType: .text_sol_markdown, digest: approvedDigest).framing
                == .specification)
    }
}
